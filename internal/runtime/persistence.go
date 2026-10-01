package runtime

import (
	"context"
	"fmt"
	"io"
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
			if c.Kind == agent.ToolStarting {
				hooksRunning.Add(1)
				go func(c agent.Checkpoint) {
					defer hooksRunning.Done()
					ready <- hookResult{c, r.beforeTool(c)}
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
		// Hook 只决定工具许可；关联映射及数据库操作仍由此处串行处理。
		if decision.Kind == "" || decision.Kind == agent.Continue {
			decision = r.coordinate(c, record)
		}
		if err := r.driver.Resolve(context.Background(), c.ID, decision); err != nil {
			r.persistenceFault(err)
		}
		if c.Kind == agent.RunFinishing {
			delete(records, c.RunID)
		}
	}
}

func (r *Runtime) beforeTool(c agent.Checkpoint) agent.Decision {
	hookCtx, cancel := context.WithTimeout(c.Context, r.manager.config.RecordTimeout)
	decision := r.manager.config.Hooks.BeforeTool(hookCtx, hooks.ToolRequest{SessionID: r.SessionID(), RunID: c.RunID, StepIndex: c.StepIndex, Call: *c.ToolCall})
	hookErr := hookCtx.Err()
	cancel()
	if c.Context.Err() != nil {
		return agent.Decision{Kind: agent.Skip, Reason: "因取消而未执行"}
	}
	if hookErr != nil {
		return agent.Decision{Kind: agent.Skip, Reason: fmt.Sprintf("工具 Hook 未完成: %v", hookErr)}
	}
	if decision.Reason != "" {
		return agent.Decision{Kind: agent.Skip, Reason: decision.Reason}
	}
	return agent.Decision{Kind: agent.Continue}
}

// 每个保存节点使用独立、有界 context。故障后仍尝试保存在途结果及终态。
func (r *Runtime) coordinate(c agent.Checkpoint, record *runRecords) agent.Decision {
	ctx, cancel := context.WithTimeout(context.Background(), r.manager.config.RecordTimeout)
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
			_, err = r.session.RecordToolStart(ctx, assistantID, c.ToolCall.ID)
		}
	case agent.ToolFinished:
		assistantID := record.responses[c.ResponseID]
		if assistantID == "" {
			err = fmt.Errorf("工具结果缺少已保存的 assistant 关联")
		} else {
			_, err = r.session.AppendToolResult(ctx, assistantID, c.ToolResult)
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
				hookCtx, stopHook := context.WithTimeout(context.Background(), r.manager.config.RecordTimeout)
				r.manager.config.Hooks.Completed(hookCtx, hooks.Completion{SessionID: r.SessionID(), Result: result})
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
	return agent.Decision{Kind: agent.Continue}
}

func (r *Runtime) firstFault() error { r.mu.Lock(); defer r.mu.Unlock(); return r.fault }
func (r *Runtime) fillTitle(ctx context.Context, msg *llm.UserMessage) {
	for _, b := range msg.Content {
		if b.Type != llm.TypeText || strings.TrimSpace(b.Text) == "" {
			continue
		}
		title := []rune(strings.TrimSpace(b.Text))
		if len(title) > 80 {
			title = title[:80]
		}
		_, _ = r.manager.service.SetTitleIfEmpty(ctx, r.SessionID(), string(title))
		return
	}
}

const defaultRecordTimeout = 5 * time.Second
