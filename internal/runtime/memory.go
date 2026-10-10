package runtime

import (
	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/memory/longterm"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"sync"
	"time"
)

func (r *Runtime) triggerMemory(model ModelConfig) {
	if r.memory == nil {
		return
	}
	r.mu.Lock()
	if r.closing || r.fault != nil {
		r.mu.Unlock()
		return
	}
	if r.memoryDone != nil {
		r.memoryWake = true
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(r.life)
	done := make(chan struct{})
	r.memoryCancel = cancel
	r.memoryDone = done
	r.memoryModel = model
	r.mu.Unlock()
	go func() {
		defer cancel()
		defer close(done)
		for {
			err := r.consolidatePending(ctx)
			if err != nil {
				r.emit(Event{Diagnostic: err})
			}
			r.mu.Lock()
			wake := err == nil && r.memoryWake && !r.closing
			r.memoryWake = false
			if !wake {
				r.memoryDone = nil
				r.memoryCancel = nil
			}
			r.mu.Unlock()
			if !wake {
				return
			}
		}
	}()
}
func (r *Runtime) consolidatePending(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := r.session.NextMemoryBatch(ctx, r.host.options.MemoryScope)
		if err != nil {
			return err
		}
		if batch == nil {
			return nil
		}
		input := longterm.Batch{ID: batch.ID}
		for _, entry := range batch.Entries {
			input.Sources = append(input.Sources, longterm.Source{ID: entry.ID, Message: entry.Message})
		}
		result, err := r.memory.Consolidate(ctx, input)
		r.emit(Event{AuxiliaryUsage: &result.Execution.Usage})
		if err != nil {
			return err
		}
		if !result.Duplicate && result.Execution.StopReason != llm.StopStop {
			return fmt.Errorf("记忆整理未正常结束")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = r.session.AdvanceMemoryProgress(ctx, *batch); err != nil {
			return err
		}
	}
}
func (r *Runtime) stopMemory() {
	r.mu.Lock()
	cancel, done := r.memoryCancel, r.memoryDone
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

type memoryBudget struct {
	mu                            sync.Mutex
	output, current, turns, calls int
	streamed                      int
	step                          int
	err                           error
	limits                        longterm.ExecutionLimits
}

func (b *memoryBudget) addOutput(size int, snapshot bool, step int) error {
	return b.accountOutput(size, snapshot, false, step)
}

// Delta 覆盖尚未解析进 Snapshot 的工具参数；同轮按增量累计与快照大小的最大值计费，避免双计。
func (b *memoryBudget) accountOutput(size int, snapshot, delta bool, step int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if snapshot {
		if step != b.step {
			b.current = 0
			b.streamed = 0
			b.step = step
		}
		if delta {
			b.streamed += size
			size = b.streamed
		}
		if size > b.current {
			b.output += size - b.current
			b.current = size
		}
	} else {
		b.output += size
	}
	if b.output > b.limits.OutputBytes && b.err == nil {
		b.err = fmt.Errorf("%w: OutputBytes", longterm.ErrBudget)
	}
	return b.err
}

func (r *Runtime) executeMemory(ctx context.Context, task longterm.Task) (longterm.ExecutionResult, error) {
	r.mu.Lock()
	model := r.memoryModel
	r.mu.Unlock()
	budget := &memoryBudget{limits: task.ExecutionLimits}
	var a *agent.Agent
	var tools []agent.Tool
	for _, definition := range task.Tools {
		definition := definition
		tools = append(tools, agent.Tool{Tool: definition, ParallelSafe: false, Execute: func(callCtx context.Context, id string, args map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			result, err := task.CallTool(callCtx, llm.ContentBlock{Type: llm.TypeToolCall, ID: id, Name: definition.Name, Arguments: args})
			if err == nil && result == nil {
				err = fmt.Errorf("记忆工具没有返回结果")
			}
			if err == nil {
				encoded, encodingErr := json.Marshal(result.Content)
				err = encodingErr
				if err == nil {
					err = budget.addOutput(len(encoded), false, 0)
				}
				if err == nil && result.IsError {
					err = fmt.Errorf("记忆工具失败: %s", definition.Name)
				}
			}
			if err != nil {
				budget.mu.Lock()
				if budget.err == nil {
					budget.err = err
				}
				budget.mu.Unlock()
				a.StopExecution(err)
				return nil, err
			}
			return result.Content, nil
		}})
	}
	history := task.Messages
	message := &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "按本批任务完成整理。"}}, Timestamp: time.Now()}
	if len(history) > 0 {
		if last, ok := history[len(history)-1].(*llm.UserMessage); ok {
			message = last
			history = history[:len(history)-1]
		}
	}
	var err error
	a, err = agent.New(agent.Config{NewRunID: uuid.NewString, Model: model.Model, Options: model.Options, SystemPrompt: task.SystemPrompt, InitialHistory: history, Tools: tools, Stream: r.host.options.Stream, MaxParallelTools: 1, OnEvent: func(event agent.Event) {
		if event.LLMEvent == nil {
			return
		}
		if event.LLMEvent.Delta != "" {
			if err := budget.accountOutput(len(event.LLMEvent.Delta), true, true, event.StepIndex); err != nil {
				a.StopExecution(err)
				return
			}
		}
		snapshot := event.LLMEvent.Snapshot
		if snapshot == nil {
			snapshot = event.LLMEvent.FinalMessage
		}
		if snapshot == nil {
			return
		}
		encoded, err := json.Marshal(snapshot.Content)
		if err == nil {
			err = budget.addOutput(len(encoded), true, event.StepIndex)
		}
		if err != nil {
			a.StopExecution(err)
		}
	}})
	if err != nil {
		return longterm.ExecutionResult{}, err
	}
	driver, err := a.Driver()
	if err != nil {
		a.RequestClose()
		_ = a.Close()
		return longterm.ExecutionResult{}, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := driver.Next(context.Background())
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				a.StopExecution(err)
				return
			}
			decision := agent.Decision{Kind: agent.Continue}
			var failure error
			switch c.Kind {
			case agent.ModelRequestReady:
				encoded, err := json.Marshal(struct {
					SystemPrompt string
					Messages     []llm.Message
					Tools        []llm.Tool
				}{c.Request.SystemPrompt, c.Request.Messages, c.Request.Tools})
				if err != nil {
					failure = err
				} else if len(encoded) > task.ExecutionLimits.InputBytes {
					failure = fmt.Errorf("%w: InputBytes", longterm.ErrBudget)
				}
			case agent.ModelCallStarting:
				budget.mu.Lock()
				if budget.turns >= task.ExecutionLimits.Turns {
					failure = fmt.Errorf("%w: Turns", longterm.ErrBudget)
				} else {
					budget.turns++
				}
				budget.mu.Unlock()
			case agent.ModelResponseReady:
				encoded, err := json.Marshal(c.Response.Content)
				if err != nil {
					failure = err
				} else {
					failure = budget.addOutput(len(encoded), true, c.StepIndex)
				}
				count := 0
				for _, block := range c.Response.Content {
					if block.Type == llm.TypeToolCall {
						count++
					}
				}
				budget.mu.Lock()
				if budget.calls+count > task.ExecutionLimits.ToolCalls {
					failure = fmt.Errorf("%w: ToolCalls", longterm.ErrBudget)
				} else {
					budget.calls += count
				}
				budget.mu.Unlock()
			case agent.ToolFinished:
				// 未知/未执行工具也由 Agent 产生结果，不一定经过 Task.CallTool 包装器。
				if c.ToolOutcome == nil || c.ToolOutcome.Status != agent.ToolSucceeded || c.ToolOutcome.RawResult == nil || c.ToolOutcome.RawResult.IsError {
					failure = fmt.Errorf("记忆 Agent 工具未成功完成")
				}
			}
			if failure != nil {
				budget.mu.Lock()
				if budget.err == nil {
					budget.err = failure
				}
				budget.mu.Unlock()
				decision = agent.Decision{Kind: agent.Stop, Error: failure}
			}
			if err := driver.Resolve(context.Background(), c.ID, decision); err != nil {
				a.StopExecution(err)
				return
			}
		}
	}()
	handle, err := a.Submit(ctx, message)
	var result agent.RunResult
	if err == nil {
		result, err = handle.Wait(context.Background())
	}
	a.RequestClose()
	closeErr := a.Close()
	<-done
	budget.mu.Lock()
	turns, budgetErr := budget.turns, budget.err
	budget.mu.Unlock()
	output := longterm.ExecutionResult{StopReason: llm.StopReason(result.StopReason), Turns: turns, Usage: result.TotalUsage}
	if err == nil && (result.State != agent.RunStateSuccess || result.StopReason != agent.RunStopCompleted) {
		err = fmt.Errorf("记忆 Agent 未正常完成: %s %s", result.State, result.ErrorMessage)
	}
	return output, errors.Join(err, budgetErr, closeErr, ctx.Err())
}
