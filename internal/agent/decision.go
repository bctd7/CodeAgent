package agent

import (
	"CodeAgent/internal/llm"
	"fmt"
)

// classifyResponse 只决定完整响应的后续动作，不调用模型、工具或外部协调。
// 非 nil 结果表示结束 Run；否则由 Agent 调度返回的工具调用。
func classifyResponse(message *llm.AssistantMessage, internalErr error) (*RunResult, []llm.ContentBlock) {
	result := &RunResult{RawStopReason: message.RawStopReason, ErrorMessage: message.ErrorMessage, Error: internalErr}
	switch message.State {
	case llm.StateAborted:
		result.State = RunStateAborted
		result.RawStopReason = ""
		return result, nil
	case llm.StateError:
		result.State = RunStateError
		if message.StopReason == llm.StopSafety {
			result.StopReason = RunStopSafety
		}
		return result, nil
	case llm.StateSuccess:
	default:
		result.State = RunStateError
		result.ErrorMessage = fmt.Sprintf("模型响应没有合法终态: %q", message.State)
		return result, nil
	}
	if message.StopReason == llm.StopLength {
		result.State, result.StopReason = RunStateSuccess, RunStopTruncated
		result.ErrorMessage = "模型输出达到最大长度被截断"
		return result, nil
	}
	var calls []llm.ContentBlock
	for _, block := range message.Content {
		if block.Type == llm.TypeToolCall {
			calls = append(calls, block)
		}
	}
	if (len(calls) > 0) != (message.StopReason == llm.StopToolUse) {
		result.State = RunStateError
		result.ErrorMessage = "模型响应以 tool_use 结束，但没有工具调用"
		if len(calls) > 0 {
			result.ErrorMessage = fmt.Sprintf("模型响应包含工具调用，但 StopReason 为 %q (期望 %q)", message.StopReason, llm.StopToolUse)
		}
		return result, nil
	}
	if len(calls) == 0 {
		result.State, result.StopReason = RunStateSuccess, RunStopCompleted
		return result, nil
	}
	if err := validateToolCallBatch(calls); err != nil {
		result.State, result.Error, result.ErrorMessage = RunStateError, err, err.Error()
		return result, nil
	}
	return nil, calls
}
