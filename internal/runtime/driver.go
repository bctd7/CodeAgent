package runtime

import (
	"CodeAgent/internal/agent"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/llm"
	sc "CodeAgent/internal/session/config"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

func (r *Runtime) onAgentEvent(event agent.Event) {
	if event.Kind == agent.EventInputDropped {
		if message, ok := event.Message.(*llm.UserMessage); ok {
			r.mu.Lock()
			delete(r.inputs, message)
			r.mu.Unlock()
		}
	}
	r.emit(Event{Agent: &event})
}

// drive 是唯一 Next 读取者。已交付的工具前置交接独立等待，不阻塞其他事实节点。
func (r *Runtime) drive(driver *agent.Driver, done chan struct{}) {
	defer close(done)
	var preparing sync.WaitGroup
	defer preparing.Wait()
	for {
		c, err := driver.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			r.persistFault(err)
			return
		}
		if c.Kind == agent.ToolStarting {
			assistant, err := r.assistantID(c)
			if err != nil {
				r.persistFault(err)
				r.resolve(driver, c, agent.Decision{Kind: agent.Stop, Error: err})
				continue
			}
			preparing.Add(1)
			go func() { defer preparing.Done(); r.resolve(driver, c, r.prepareTool(c, assistant)) }()
			continue
		}
		decision := r.handleCheckpoint(c)
		r.resolve(driver, c, decision)
	}
}
func (r *Runtime) resolve(driver *agent.Driver, c agent.Checkpoint, decision agent.Decision) {
	ctx, cancel := r.recordContext()
	defer cancel()
	if err := driver.Resolve(ctx, c.ID, decision); err != nil {
		r.persistFault(fmt.Errorf("确认节点 %d: %w", c.ID, err))
	}
}
func (r *Runtime) handleCheckpoint(c agent.Checkpoint) agent.Decision {
	decision := agent.Decision{Kind: agent.Continue}
	var err error
	persist := false
	switch c.Kind {
	case agent.RunStarting:
		err = r.startRun(c)
	case agent.InputConsumed:
		ctx, cancel := r.recordContext()
		_, err = r.session.AppendMessage(ctx, c.RunID, c.UserMessage, nil)
		cancel()
		persist = true
	case agent.ModelRequestReady:
		decision.RequestContext, err = r.prepareRequest(c)
	case agent.ModelCallStarting:
		ctx, cancel := r.recordContext()
		_, err = r.session.RecordStep(ctx, c.RunID)
		cancel()
		persist = true
	case agent.ModelResponseReady:
		err = r.saveResponse(c)
		persist = err != nil
		if err == nil && r.modelHooks != nil {
			err = r.modelHooks.AfterModelResponse(c.Context, hooks.ModelResponse{SessionID: r.id, RunID: c.RunID, StepIndex: c.StepIndex, Response: *c.Response})
		}
	case agent.ToolFinished:
		decision.ToolContent, err = r.finishTool(c)
	case agent.ToolBatchFinished:
		decision.ContextMessages, err = r.finishBatch(c)
	case agent.RunEnding:
		decision, err = r.endCandidate(c)
	case agent.RunFinishing:
		err = r.finishRun(c)
		persist = true
	default:
		err = fmt.Errorf("未知 Driver 节点: %s", c.Kind)
	}
	if err != nil {
		if persist {
			r.persistFault(err)
		}
		return agent.Decision{Kind: agent.Stop, Error: err}
	}
	return decision
}

func (r *Runtime) finishTool(c agent.Checkpoint) (*[]llm.ContentBlock, error) {
	assistant, err := r.assistantID(c)
	if err != nil {
		r.persistFault(err)
		return nil, err
	}
	outcome := c.ToolOutcome
	if outcome == nil || outcome.RawResult == nil {
		return nil, fmt.Errorf("工具完成缺少原始事实")
	}
	ctx, cancel := r.recordContext()
	entry, err := r.session.AppendToolResult(ctx, assistant, sc.ToolResultInput{Result: outcome.RawResult, Arguments: outcome.Call.Arguments, Status: sc.ToolStatus(outcome.Status), Executed: outcome.Executed, ErrorMessage: outcome.Error})
	cancel()
	if err != nil {
		r.persistFault(err)
		return nil, err
	}
	var replacement *[]llm.ContentBlock
	if r.hooks != nil {
		binding := r.bindings[outcome.Call.Name]
		// Agent 可报告没有绑定的 unavailable，仍保存原始结果；不伪造可信工具身份。
		if binding != nil {
			input, err := toolInput(binding, outcome.Call.ID, outcome.Call.Arguments)
			if err != nil {
				return nil, err
			}
			var values []string
			var notices hooks.Notices
			var control hooks.Control
			var duration *int64
			if outcome.Executed {
				value := outcome.FinishedAt.Sub(outcome.StartedAt).Milliseconds()
				duration = &value
			}
			if outcome.Status == agent.ToolSucceeded {
				raw, encodeErr := json.Marshal(outcome.RawResult.Content)
				if encodeErr != nil {
					return nil, encodeErr
				}
				out, hookErr := r.hooks.PostToolUse(c.Context, hooks.PostToolUseInput{CommonInput: r.common(), ToolInput: input, ToolResponse: raw, DurationMS: duration})
				notices = out.Notices
				control = out.Control
				err = hookError(notices, hookErr)
				values = append(out.Feedback, out.AdditionalContext...)
				if err == nil && out.UpdatedToolOutput != nil {
					blocks, parseErr := visibleOutput(*out.UpdatedToolOutput)
					if parseErr != nil {
						return nil, parseErr
					}
					replacement = &blocks
				}
			} else {
				out, hookErr := r.hooks.PostToolUseFailure(c.Context, hooks.PostToolUseFailureInput{CommonInput: r.common(), ToolInput: input, Error: outcome.Error, IsInterrupt: outcome.Status == agent.ToolCanceled, DurationMS: duration})
				notices = out.Notices
				control = out.Control
				err = hookError(notices, hookErr)
				values = append(out.Feedback, out.AdditionalContext...)
			}
			r.emit(Event{Notices: &notices})
			if err != nil {
				return nil, err
			}
			if control.StopProcessing {
				return nil, fmt.Errorf("工具后 Hook 停止: %v", control.StopReasons)
			}
			additions := contexts("hook_post_tool", fmt.Sprint(c.ID), values)
			r.mu.Lock()
			r.feedback[c.RunID] = append(r.feedback[c.RunID], feedbacks(additions)...)
			r.mu.Unlock()
		}
	}
	ctx, cancel = r.recordContext()
	err = r.session.CompleteToolProcessing(ctx, entry.ID, replacement)
	cancel()
	if err != nil {
		r.persistFault(err)
		return nil, err
	}
	return replacement, nil
}

// Hook 可返回任意 JSON。ContentBlock 数组保留；其余数据作为正文，不重写工具身份。
func visibleOutput(raw json.RawMessage) ([]llm.ContentBlock, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return []llm.ContentBlock{}, nil
	}
	if text, ok := value.(string); ok {
		if text == "" {
			return []llm.ContentBlock{}, nil
		}
		return []llm.ContentBlock{{Type: llm.TypeText, Text: text}}, nil
	}
	if array, ok := value.([]any); ok {
		if len(array) == 0 {
			return []llm.ContentBlock{}, nil
		}
		var blocks []llm.ContentBlock
		if json.Unmarshal(raw, &blocks) == nil {
			valid := true
			for _, block := range blocks {
				if block.Type != llm.TypeText && block.Type != llm.TypeImage {
					valid = false
					break
				}
			}
			if valid {
				return blocks, nil
			}
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []llm.ContentBlock{{Type: llm.TypeText, Text: string(encoded)}}, nil
}

func (r *Runtime) finishBatch(c agent.Checkpoint) ([]llm.Message, error) {
	var additions []sc.ContextInput
	if r.hooks != nil {
		var batch []hooks.BatchTool
		for _, outcome := range c.ToolBatch {
			binding := r.bindings[outcome.Call.Name]
			if binding == nil {
				continue
			}
			input, err := toolInput(binding, outcome.Call.ID, outcome.Call.Arguments)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(outcome.Result.Content)
			if err != nil {
				return nil, err
			}
			status := hooks.ToolStatus(outcome.Status)
			if outcome.Status == agent.ToolSkipped || outcome.Status == agent.ToolDeferred {
				status = hooks.ToolUnavailable
			}
			batch = append(batch, hooks.BatchTool{ToolInput: input, ToolResponse: raw, Status: status})
		}
		out, err := r.hooks.PostToolBatch(c.Context, hooks.PostToolBatchInput{CommonInput: r.common(), ToolCalls: batch})
		r.emit(Event{Notices: &out.Notices})
		if err = hookError(out.Notices, err); err != nil {
			return nil, err
		}
		if out.StopProcessing || out.StopLoop {
			return nil, fmt.Errorf("PostToolBatch 停止: %v", append(out.Reasons, out.StopReasons...))
		}
		additions = contexts("hook_batch", fmt.Sprint(c.ID), out.AdditionalContext)
	}
	r.mu.Lock()
	pending := r.feedback[c.RunID]
	delete(r.feedback, c.RunID)
	r.mu.Unlock()
	for _, value := range pending {
		additions = append(additions, value.input)
	}
	ctx, cancel := r.recordContext()
	defer cancel()
	messages, err := r.saveContexts(ctx, c.RunID, additions)
	if err != nil {
		r.persistFault(err)
	}
	return messages, err
}
func (r *Runtime) endCandidate(c agent.Checkpoint) (agent.Decision, error) {
	decision := agent.Decision{Kind: agent.Continue}
	if c.Result == nil || c.Result.State != agent.RunStateSuccess || c.Result.StopReason != agent.RunStopCompleted {
		return decision, nil
	}
	messages, err := r.collectChildren(c)
	if err != nil {
		return decision, err
	}
	if r.hooks != nil {
		records := r.records[c.RunID]
		last := ""
		for i:=len(c.Result.Messages)-1;i>=0;i-- {if response,ok:=c.Result.Messages[i].(*llm.AssistantMessage);ok {for _,block:=range response.Content {if block.Type==llm.TypeText {last+=block.Text}};break}}
		out, err := r.hooks.Stop(c.Context, hooks.StopInput{CommonInput: r.common(), StopHookActive: records.stopActive, LastAssistantMessage: last})
		r.emit(Event{Notices: &out.Notices})
		if err = hookError(out.Notices, err); err != nil {
			return decision, err
		}
		if out.StopProcessing {
			return decision, fmt.Errorf("Stop Hook 停止: %v", out.StopReasons)
		}
		values := out.AdditionalContext
		if out.ContinueTurn {
			values = append(values, strings.Join(out.Reasons, "\n"))
			if strings.TrimSpace(strings.Join(values, "")) == "" {
				values = []string{"Stop Hook 要求继续当前任务。"}
			}
			records.stopActive = true
		}
		ctx, cancel := r.recordContext()
		extra, err := r.saveContexts(ctx, c.RunID, contexts("hook_stop", fmt.Sprint(c.ID), values))
		cancel()
		if err != nil {
			r.persistFault(err)
			return decision, err
		}
		messages = append(messages, extra...)
	}
	if len(messages) > 0 {
		decision.Kind = agent.Repeat
		decision.ContextMessages = messages
	}
	return decision, nil
}
