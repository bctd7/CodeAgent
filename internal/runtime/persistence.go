package runtime

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/llm"
	sessionconfig "CodeAgent/internal/session/config"
)

// runRecords 只保存执行标识与 Session 记录的关联，不承载执行状态机。
type runRecords struct {
	started   bool
	responses map[agent.CheckpointID]string
}

// drive 是唯一驱动者。保存是固定流程，执行决策仍由 Agent 作出。
func (r *Runtime) drive() {
	defer close(r.driverDone)
	// 唯一读取者可继续接收结果节点，不被工具 Hook 的等待阻挡。
	incoming := make(chan agent.Checkpoint)
	go func() {
		defer close(incoming)
		for {
			c, err := r.driver.Next(context.Background())
			if err == io.EOF {
				return
			}
			if err != nil {
				r.persistenceFault(err)
				continue
			}
			incoming <- c
		}
	}()
	type hookResult struct {
		checkpoint agent.Checkpoint
		decision   agent.Decision
	}
	ready := make(chan hookResult)
	var hooksRunning sync.WaitGroup
	defer hooksRunning.Wait()
	records := make(map[string]*runRecords)
	for {
		var c agent.Checkpoint
		var decision agent.Decision
		select {
		case next, ok := <-incoming:
			if !ok {
				return
			}
			c = next
			if c.Kind == agent.ModelRequestReady || c.Kind == agent.ModelResponseReady || c.Kind == agent.ToolStarting || c.Kind == agent.ToolFinished {
				hooksRunning.Add(1)
				go func(c agent.Checkpoint) {
					defer hooksRunning.Done()
					hookCtx, cancel := context.WithTimeout(c.Context, r.host.config.RecordTimeout)
					defer cancel()
					decision := agent.Decision{Kind: agent.Continue}
					switch c.Kind {
					case agent.ModelRequestReady:
						replacement, err := r.host.config.Hooks.BeforeModelRequest(hookCtx, hooks.ModelRequest{
							SessionID: r.SessionID(), RunID: c.RunID, StepIndex: c.StepIndex,
							Context: llm.RequestContext{SystemPrompt: c.Request.SystemPrompt, Messages: c.Request.Messages},
						})
						if err != nil {
							decision = agent.Decision{Kind: agent.Stop, Error: fmt.Errorf("before model request: %w", err)}
						} else {
							decision.RequestContext = replacement
						}
					case agent.ModelResponseReady:
						r.host.config.Hooks.AfterModelResponse(hookCtx, hooks.ModelResponse{SessionID: r.SessionID(), RunID: c.RunID, StepIndex: c.StepIndex, Response: *c.Response})
					case agent.ToolStarting:
						result := r.host.config.Hooks.BeforeTool(hookCtx, hooks.ToolRequest{SessionID: r.SessionID(), RunID: c.RunID, StepIndex: c.StepIndex, Call: *c.ToolCall})
						switch {
						case c.Context.Err() != nil:
							decision = agent.Decision{Kind: agent.Skip, Reason: "cancelled before tool execution"}
						case hookCtx.Err() != nil:
							decision = agent.Decision{Kind: agent.Skip, Reason: fmt.Sprintf("before tool: %v", hookCtx.Err())}
						case result.Reason != "":
							decision = agent.Decision{Kind: agent.Skip, Reason: result.Reason}
						default:
							decision.ToolArguments = result.Arguments
						}
					case agent.ToolFinished:
						content, err := r.host.config.Hooks.AfterTool(hookCtx, hooks.ToolResponse{SessionID: r.SessionID(), RunID: c.RunID, StepIndex: c.StepIndex, Result: *c.ToolResult})
						if err != nil {
							decision = agent.Decision{Kind: agent.Stop, Error: fmt.Errorf("after tool: %w", err)}
							// 保留原始输出供审计，加工失败后不得将其重新提供给模型。
							empty := []llm.ContentBlock{}
							copy := *c.ToolResult
							copy.ModelContent = &empty
							c.ToolResult = &copy
						} else if content != nil && !reflect.DeepEqual(*content, c.ToolResult.Content) {
							decision.ToolContent = content
						}
					}
					ready <- hookResult{c, decision}
				}(c)
				continue
			}
		case hook := <-ready:
			c, decision = hook.checkpoint, hook.decision
		}
		record := records[c.RunID]
		if record == nil {
			record = &runRecords{responses: make(map[agent.CheckpointID]string)}
			records[c.RunID] = record
		}
		// Hook 转换结果交回这里串行保存；不让回调持有记录映射或存储连接。
		if decision.Kind == "" {
			decision.Kind = agent.Continue
		}
		if decision.Kind == agent.Continue || c.Kind == agent.ToolFinished {
			decision = r.coordinate(c, record, decision)
		}
		if err := r.driver.Resolve(context.Background(), c.ID, decision); err != nil {
			r.persistenceFault(err)
		}
		if c.Kind == agent.RunFinishing {
			delete(records, c.RunID)
		}
	}
}

func (r *Runtime) coordinate(c agent.Checkpoint, record *runRecords, decision agent.Decision) agent.Decision {
	// 取消或 Hook 失败后仍须完成事实记录。
	ctx, cancel := context.WithTimeout(context.Background(), r.host.config.RecordTimeout)
	defer cancel()
	var err error
	switch c.Kind {
	case agent.RunStarting:
		_, err = r.session.StartRun(ctx, c.RunID, c.UserMessage)
		if err == nil {
			record.started = true
			r.fillTitle(ctx, c.UserMessage)
		}
	case agent.InputConsumed:
		_, err = r.session.AppendMessage(ctx, c.RunID, c.UserMessage, nil)
	case agent.ModelRequestReady:
		_, err = r.session.RecordStep(ctx, c.RunID)
	case agent.ModelResponseReady:
		origin := c.Origin
		source := &sessionconfig.MessageModelOrigin{Provider: origin.Provider, API: origin.API, ModelID: origin.ModelID, BaseURL: origin.BaseURL}
		entry, saveErr := r.session.AppendMessage(ctx, c.RunID, c.Response, source)
		err = saveErr
		if err == nil {
			record.responses[c.ID] = entry.ID
		}
	case agent.ToolStarting:
		assistantID := record.responses[c.ResponseID]
		if assistantID == "" {
			err = fmt.Errorf("工具启动缺少已保存的 assistant 关联")
		} else {
			_, err = r.session.RecordToolStart(ctx, assistantID, c.ToolCall.ID, decision.ToolArguments)
		}
	case agent.ToolFinished:
		assistantID := record.responses[c.ResponseID]
		if assistantID == "" {
			err = fmt.Errorf("工具结果缺少已保存的 assistant 关联")
		} else {
			result := *c.ToolResult
			if decision.ToolContent != nil {
				result.ModelContent = decision.ToolContent
			}
			_, err = r.session.AppendToolResult(ctx, assistantID, &result)
		}
	case agent.RunFinishing:
		if record.started {
			result := *c.Result
			fault := r.firstFault()
			if fault != nil {
				result.State = agent.RunStateError
				result.StopReason, result.RawStopReason = "", ""
				result.ErrorMessage = fmt.Sprintf("持久化失败: %v", fault)
			}
			_, err = r.session.FinishRun(ctx, c.RunID, sessionconfig.RunCompletion{State: sessionconfig.RunState(result.State), StopReason: llm.StopReason(result.StopReason), RawStopReason: result.RawStopReason, ErrorMessage: result.ErrorMessage})
			if err == nil && result.State == agent.RunStateSuccess && result.StopReason == agent.RunStopCompleted {
				hookCtx, stopHook := context.WithTimeout(context.Background(), r.host.config.RecordTimeout)
				r.host.config.Hooks.Completed(hookCtx, hooks.Completion{SessionID: r.SessionID(), Result: result})
				stopHook()
			}
			if err == nil && fault != nil {
				return agent.Decision{Kind: agent.Stop, Error: fault}
			}
		}
	default:
		err = fmt.Errorf("未知执行节点 %q", c.Kind)
	}
	if err != nil {
		r.persistenceFault(err)
		return agent.Decision{Kind: agent.Stop, Error: r.firstFault()}
	}
	return decision
}

func (r *Runtime) fillTitle(ctx context.Context, msg *llm.UserMessage) {
	for _, b := range msg.Content {
		if b.Type != llm.TypeText || strings.TrimSpace(b.Text) == "" {
			continue
		}
		title := []rune(strings.TrimSpace(b.Text))
		if len(title) > 80 {
			title = title[:80]
		}
		_, _ = r.host.service.SetTitleIfEmpty(ctx, r.SessionID(), string(title))
		return
	}
}

const defaultRecordTimeout = 5 * time.Second
