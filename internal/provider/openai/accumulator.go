package openai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"CodeAgent/internal/llm"
)

// messageSnapshot 仅复制 Content 切片头及元素，底层字符串和解析完成的参数 Map 保持只读复用。
func messageSnapshot(msg llm.AssistantMessage) *llm.AssistantMessage {
	snap := msg
	if len(msg.Content) > 0 {
		snap.Content = make([]llm.ContentBlock, len(msg.Content))
		copy(snap.Content, msg.Content)
	}
	return &snap
}

// toolCallAccumulator 暂存一个工具调用尚未解析完成的参数。
type toolCallAccumulator struct {
	blockIndex int
	args       strings.Builder
}

// responseAccumulator 将服务端 Chunk 中的 Delta 累积到 Message 的 Block，
// 并发布内部 Event。事件队列和流的生命周期由 llm.StreamWriter 管理。
type responseAccumulator struct {
	writer             llm.StreamWriter
	pricing            llm.Pricing
	message            llm.AssistantMessage
	textBlockIndex     int
	thinkingBlockIndex int
	tools              map[int]*toolCallAccumulator // key: chunk 中的 tool_call index
}

func newResponseAccumulator(writer llm.StreamWriter, pricing llm.Pricing, initial llm.AssistantMessage) *responseAccumulator {
	return &responseAccumulator{
		writer:             writer,
		pricing:            pricing,
		message:            initial,
		textBlockIndex:     -1,
		thinkingBlockIndex: -1,
		tools:              make(map[int]*toolCallAccumulator),
	}
}

func (s *responseAccumulator) emitEvent(eventType llm.EventType, blockIndex int, delta string) error {
	return s.writer.Emit(llm.Event{
		Type:       eventType,
		BlockIndex: blockIndex,
		Delta:      delta,
		Snapshot:   messageSnapshot(s.message),
	})
}

func (s *responseAccumulator) applyChunk(chunk chatCompletionChunk) error {
	// 1. 服务端错误
	if chunk.Error != nil {
		msg := chunk.Error.Message
		if msg == "" {
			msg = chunk.Error.Type
		}
		if msg == "" {
			msg = "未知服务端错误"
		}
		return fmt.Errorf("provider error: %s", msg)
	}

	// 2. 响应 ID、响应模型与 usage
	if chunk.ID != "" {
		s.message.ResponseID = chunk.ID
	}
	if chunk.Model != "" {
		s.message.ResponseModel = chunk.Model
	}
	if chunk.Usage != nil {
		prompt := chunk.Usage.PromptTokens
		cached := chunk.Usage.PromptDetails.CachedTokens
		input := prompt - cached
		if input < 0 {
			input = 0
		}
		output := chunk.Usage.CompletionTokens
		reasoning := chunk.Usage.CompletionDetails.ReasoningTokens
		s.message.Usage = llm.Usage{
			Input:     input,
			Output:    output,
			CacheRead: cached,
			Reasoning: reasoning,
			Total:     input + output + cached,
		}
		s.message.Usage = llm.CalculateCost(s.message.Usage, s.pricing)
	}

	// 只处理候选答案 0，记录结束原因并将增量应用到内容块。
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}

		if choice.FinishReason != "" {
			s.message.RawStopReason = choice.FinishReason
		}
		if err := s.applyDelta(choice.Delta); err != nil {
			return err
		}
	}
	return nil
}

func (s *responseAccumulator) applyDelta(delta responseDelta) error {
	// 思考增量
	if delta.ReasoningContent != "" {
		if s.thinkingBlockIndex == -1 {
			s.thinkingBlockIndex = len(s.message.Content)
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeThinking,
			})
			if err := s.emitEvent(llm.EventThinkingBlockStart, s.thinkingBlockIndex, ""); err != nil {
				return err
			}
		}
		s.message.Content[s.thinkingBlockIndex].Thinking += delta.ReasoningContent
		if err := s.emitEvent(llm.EventThinkingDelta, s.thinkingBlockIndex, delta.ReasoningContent); err != nil {
			return err
		}
	}

	// 文本增量
	if delta.Content != "" {
		if s.textBlockIndex == -1 {
			s.textBlockIndex = len(s.message.Content)
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeText,
			})
			if err := s.emitEvent(llm.EventTextBlockStart, s.textBlockIndex, ""); err != nil {
				return err
			}
		}
		s.message.Content[s.textBlockIndex].Text += delta.Content
		if err := s.emitEvent(llm.EventTextDelta, s.textBlockIndex, delta.Content); err != nil {
			return err
		}
	}

	// 工具增量
	for _, tc := range delta.ToolCalls {
		tool, exists := s.tools[tc.Index]
		if !exists {
			blockIndex := len(s.message.Content)
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeToolCall,
				ID:   tc.ID,
				Name: tc.Function.Name,
			})
			tool = &toolCallAccumulator{blockIndex: blockIndex}
			s.tools[tc.Index] = tool
			if err := s.emitEvent(llm.EventToolCallBlockStart, blockIndex, ""); err != nil {
				return err
			}
		} else {
			s.message.Content[tool.blockIndex].ID += tc.ID
			s.message.Content[tool.blockIndex].Name += tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			tool.args.WriteString(tc.Function.Arguments)
			if err := s.emitEvent(llm.EventToolCallDelta, tool.blockIndex, tc.Function.Arguments); err != nil {
				return err
			}
		}
	}

	return nil
}

// completeBlocks 仅完成参数解析和块结束事件；失败由 run 统一收尾。
func (s *responseAccumulator) completeBlocks() (llm.StopReason, error) {
	// 1. 映射并校验结束原因
	stopReason, err := mapStopReason(s.message.RawStopReason)
	if err != nil {
		return stopReason, err
	}

	// 2. 按 blockIndex 顺序解析所有工具调用的参数，写入对应块
	toolList := make([]*toolCallAccumulator, 0, len(s.tools))
	for _, tool := range s.tools {
		toolList = append(toolList, tool)
	}
	sort.Slice(toolList, func(i, j int) bool {
		return toolList[i].blockIndex < toolList[j].blockIndex
	})
	for _, tool := range toolList {
		block := &s.message.Content[tool.blockIndex]
		var args map[string]any
		if err := json.Unmarshal([]byte(tool.args.String()), &args); err != nil {
			return "", fmt.Errorf("工具 %s 参数解析失败: %w", block.Name, err)
		}
		if args == nil {
			return "", fmt.Errorf("工具 %s 参数必须是 JSON 对象，不能为 null", block.Name)
		}
		block.Arguments = args
	}

	// 3. 按 Content 顺序发布 TextEnd、ThinkingEnd、ToolCallEnd
	for idx, block := range s.message.Content {
		var eventType llm.EventType
		switch block.Type {
		case llm.TypeText:
			eventType = llm.EventTextBlockEnd
		case llm.TypeThinking:
			eventType = llm.EventThinkingBlockEnd
		case llm.TypeToolCall:
			eventType = llm.EventToolCallBlockEnd
		default:
			continue
		}
		if err := s.emitEvent(eventType, idx, ""); err != nil {
			return "", err
		}
	}

	return stopReason, nil
}

// mapStopReason 把 OpenAI 的 finish_reason 映射成领域层的 StopReason。
// 对正常结束、空值、安全过滤和未知原因统一返回映射结果或具体错误。
func mapStopReason(raw string) (llm.StopReason, error) {
	switch raw {
	case "stop", "end":
		return llm.StopStop, nil
	case "length":
		return llm.StopLength, nil
	case "tool_calls", "function_call":
		return llm.StopToolUse, nil
	case "":
		return "", fmt.Errorf("未收到 finish_reason")
	case "content_filter":
		return llm.StopSafety, fmt.Errorf("内容被安全过滤器拦截 (content_filter)")
	default:
		return "", fmt.Errorf("未知的 finish_reason %q", raw)
	}
}
