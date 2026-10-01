package agent

import (
	"fmt"
	"time"

	"CodeAgent/internal/llm"
)

// ModelOrigin 记录消息产生时的具体模型来源与端点，用于思考块同源性精准核验。
type ModelOrigin struct {
	Provider string
	API      llm.APIKind
	ModelID  string
	BaseURL  string
}

// prepareRequestHistory 将历史整理成目标模型的请求，不修改原始消息。
// Assistant 与紧随其后的工具结果成组处理；对缺失结果补充说明，跳过结构错误的旧响应。
func prepareRequestHistory(
	history []llm.Message,
	targetModel llm.Model,
	tools []llm.Tool,
	options llm.RequestOptions,
	systemPrompt string,
	originMap map[*llm.AssistantMessage]ModelOrigin,
) (llm.Request, error) {
	prepared := make([]llm.Message, 0, len(history))
	for i := 0; i < len(history); i++ {
		switch message := history[i].(type) {
		case *llm.UserMessage:
			prepared = append(prepared, message)
		case *llm.ToolResultMessage:
			// 独立出现的工具结果属于孤立结果，不发送给模型。
			continue
		case *llm.AssistantMessage:
			// 收集紧随其后的工具结果成组处理。
			var toolResults []*llm.ToolResultMessage
			for i+1 < len(history) {
				result, ok := history[i+1].(*llm.ToolResultMessage)
				if !ok {
					break
				}
				toolResults = append(toolResults, result)
				i++
			}

			// 失败、取消和截断的响应可能包含半截工具调用，整组排除。
			if message.State == llm.StateError || message.State == llm.StateAborted || message.StopReason == llm.StopLength {
				continue
			}

			var toolCalls []llm.ContentBlock
			for _, block := range message.Content {
				if block.Type == llm.TypeToolCall {
					toolCalls = append(toolCalls, block)
				}
			}
			// 本次响应若因结束原因与工具调用不一致而失败，后续请求不回传它。
			if (len(toolCalls) > 0) != (message.StopReason == llm.StopToolUse) {
				continue
			}

			// 配对并整理工具结果；非法调用或结果整组跳过，不阻断后续任务。
			var pairedResults []llm.Message
			if len(toolCalls) > 0 {
				var err error
				pairedResults, err = pairToolResults(toolCalls, toolResults)
				if err != nil {
					continue
				}
			}

			// 统一处理思考块同源兼容性。无工具调用且思考块被过滤为空时跳过。
			assistant, ok := projectAssistantThinking(message, targetModel, originMap)
			if !ok {
				continue
			}
			prepared = append(prepared, assistant)
			prepared = append(prepared, pairedResults...)
		}
	}
	return llm.Request{
		Model:        targetModel,
		SystemPrompt: systemPrompt,
		Messages:     prepared,
		Tools:        tools,
		Options:      options,
	}, nil
}

// pairToolResults 校验并按工具调用的原始顺序组织工具结果。
// 对缺失结果补充错误说明，对非法结构（空 ID、重复 ID、跨组串配）返回明确错误。
func pairToolResults(toolCalls []llm.ContentBlock, toolResults []*llm.ToolResultMessage) ([]llm.Message, error) {
	resultsByID := make(map[string]*llm.ToolResultMessage, len(toolCalls))
	for _, call := range toolCalls {
		if call.ID == "" {
			return nil, fmt.Errorf("assistant 消息包含空的工具调用 ID (工具: %s)", call.Name)
		}
		if _, exists := resultsByID[call.ID]; exists {
			return nil, fmt.Errorf("assistant 消息包含重复的工具调用 ID: %s", call.ID)
		}
		resultsByID[call.ID] = nil
	}

	for _, res := range toolResults {
		if res == nil || res.ToolCallID == "" {
			return nil, fmt.Errorf("工具结果缺少 ToolCallID")
		}
		previous, exists := resultsByID[res.ToolCallID]
		if !exists {
			return nil, fmt.Errorf("工具结果 ToolCallID %s 不属于当前 assistant 的工具调用", res.ToolCallID)
		}
		if previous != nil {
			return nil, fmt.Errorf("工具结果存在重复的 ToolCallID: %s", res.ToolCallID)
		}
		resultsByID[res.ToolCallID] = res
	}

	paired := make([]llm.Message, 0, len(toolCalls))
	for _, call := range toolCalls {
		if res := resultsByID[call.ID]; res != nil {
			paired = append(paired, res)
		} else {
			paired = append(paired, &llm.ToolResultMessage{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Content: []llm.ContentBlock{
					{Type: llm.TypeText, Text: "此工具调用没有可用的执行结果，无法确认是否已执行或产生副作用。请先核实当前状态，再决定是否重试。"},
				},
				IsError:   true,
				Timestamp: time.Now(),
			})
		}
	}
	return paired, nil
}

// projectAssistantThinking 按同源四要素（Provider、API、Model ID、BaseURL）过滤 assistant 消息中的思考块。
func projectAssistantThinking(
	m *llm.AssistantMessage,
	targetModel llm.Model,
	originMap map[*llm.AssistantMessage]ModelOrigin,
) (*llm.AssistantMessage, bool) {
	origin, hasOrigin := originMap[m]
	sameOrigin := hasOrigin &&
		origin.Provider == targetModel.Provider &&
		origin.API == targetModel.API &&
		origin.ModelID == targetModel.ID &&
		origin.BaseURL == targetModel.BaseURL

	var filteredBlocks []llm.ContentBlock
	for _, b := range m.Content {
		switch b.Type {
		case llm.TypeThinking:
			if sameOrigin {
				filteredBlocks = append(filteredBlocks, b)
			}
		default:
			filteredBlocks = append(filteredBlocks, b)
		}
	}
	if len(filteredBlocks) == 0 {
		return nil, false
	}
	asstCopy := *m
	asstCopy.Content = filteredBlocks
	return &asstCopy, true
}

// copyHistory 浅拷贝消息切片，用于接收初始历史和返回只读快照。
func copyHistory(src []llm.Message) []llm.Message {
	if len(src) == 0 {
		return nil
	}
	dst := make([]llm.Message, len(src))
	copy(dst, src)
	return dst
}
