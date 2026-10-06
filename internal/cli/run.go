package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

func (c *CLI) run(ctx context.Context, i Invocation, streams IO) int {
	output := RunOutput{SessionID: i.SessionID, State: agent.RunStateError}
	r, _, err := c.open(ctx, i)
	var handle RunHandle
	if err == nil {
		output.SessionID = r.SessionID()
		handle, err = r.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: i.Task}}, Timestamp: time.Now()})
	}
	if err == nil {
		output.RunID = handle.ID()
		var result agent.RunResult
		result, err = handle.Wait(ctx)
		// 只有调用方 context 真正结束才请求取消；单纯的 Wait 超时错误不表示任务取消。
		if err != nil && ctx.Err() != nil {
			handle.Cancel()
			var cleanupErr error
			result, cleanupErr = handle.Wait(context.WithoutCancel(ctx))
			err = errors.Join(err, cleanupErr)
		}
		if result.State != "" {
			output.State, output.Steps, output.TotalUsage, output.StopReason = result.State, result.Steps, result.TotalUsage, result.StopReason
			output.Text = assistantText(result.Messages)
			output.ErrorMessage = result.ErrorMessage
		}
	}
	code := ExitOK
	if err != nil {
		output.ErrorMessage = err.Error()
		// Wait 自身超时而调用方仍有效是等待失败，不代表底层任务已取消。
		code = ExitFailure
		if ctx.Err() != nil || output.State == agent.RunStateAborted {
			code = ExitCancelled
		}
		if code == ExitCancelled && output.State == agent.RunStateError && handle == nil {
			output.State = agent.RunStateAborted
		}
	} else {
		switch output.State {
		case agent.RunStateSuccess:
		case agent.RunStateAborted:
			code = ExitCancelled
		default:
			code = ExitFailure
		}
	}
	if code != ExitOK && output.ErrorMessage == "" {
		output.ErrorMessage = "任务未成功完成：" + string(output.State)
	}
	if output.ErrorMessage != "" {
		fmt.Fprintln(streams.Err, output.ErrorMessage)
	}
	if i.JSON {
		if writeErr := json.NewEncoder(streams.Out).Encode(output); writeErr != nil {
			fmt.Fprintln(streams.Err, writeErr)
			return ExitFailure
		}
	} else if output.Text != "" {
		if _, writeErr := fmt.Fprintln(streams.Out, output.Text); writeErr != nil {
			fmt.Fprintln(streams.Err, writeErr)
			return ExitFailure
		}
	}
	return code
}

func assistantText(messages []llm.Message) string {
	var text []string
	for _, message := range messages {
		assistant, ok := message.(*llm.AssistantMessage)
		if !ok || assistant == nil {
			continue
		}
		for _, block := range assistant.Content {
			if block.Type == llm.TypeText {
				text = append(text, block.Text)
			}
		}
	}
	return strings.Join(text, "\n")
}

// EndInput 由交互层在 EOF/中断时调用；只取消当前已持有句柄并等待必要收尾。
// 去掉输入 context 的取消，不增设会被误解为任务取消的等待超时。
func (c *CLI) EndInput(ctx context.Context, state ChatState) (agent.RunResult, error) {
	if state.Runtime == nil || state.CurrentHandle == nil {
		return agent.RunResult{}, nil
	}
	current := state.Runtime.Snapshot().Agent.CurrentRunID
	if current == "" {
		return agent.RunResult{}, nil
	}
	if state.CurrentHandle.ID() != current {
		return agent.RunResult{}, fmt.Errorf("没有持有当前任务的句柄，不能取消其他任务")
	}
	state.CurrentHandle.Cancel()
	return state.CurrentHandle.Wait(context.WithoutCancel(ctx))
}
