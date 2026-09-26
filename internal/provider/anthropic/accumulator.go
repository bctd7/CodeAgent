package anthropic

import (
	"encoding/json"
	"fmt"
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
	rawInput json.RawMessage
	hasDelta bool
	args     strings.Builder
}

// responseAccumulator 将服务端 SSE 帧累积到 Message，并发布内部 Event。
type responseAccumulator struct {
	writer              llm.StreamWriter
	pricing             llm.Pricing
	message             llm.AssistantMessage
	blockMap            map[int]int                  // Anthropic index -> message.Content index
	tools               map[int]*toolCallAccumulator // Anthropic index -> toolCallAccumulator
	receivedMessageStop bool
}

func newResponseAccumulator(writer llm.StreamWriter, pricing llm.Pricing, initial llm.AssistantMessage) *responseAccumulator {
	return &responseAccumulator{
		writer:   writer,
		pricing:  pricing,
		message:  initial,
		blockMap: make(map[int]int),
		tools:    make(map[int]*toolCallAccumulator),
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

func (s *responseAccumulator) applyEvent(event responseEvent) error {
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			s.message.ResponseID = event.Message.ID
			s.message.ResponseModel = event.Message.Model
			if event.Message.Usage != nil {
				s.message.Usage.Input = int64(event.Message.Usage.InputTokens)
				s.message.Usage.Output = int64(event.Message.Usage.OutputTokens)
				s.message.Usage.CacheRead = int64(event.Message.Usage.CacheReadInputTokens)
				s.message.Usage.CacheWrite = int64(event.Message.Usage.CacheCreationInputTokens)
				s.message.Usage.Total = s.message.Usage.Input + s.message.Usage.Output + s.message.Usage.CacheRead + s.message.Usage.CacheWrite
				s.message.Usage = llm.CalculateCost(s.message.Usage, s.pricing)
			}
		}
		return s.emitEvent(llm.EventMessageStart, 0, "")

	case "content_block_start":
		if event.ContentBlock == nil {
			return fmt.Errorf("content_block_start 缺少 content_block 载荷")
		}
		cb := event.ContentBlock
		blockIndex := len(s.message.Content)
		s.blockMap[event.Index] = blockIndex

		switch cb.Type {
		case "text":
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeText,
				Text: cb.Text,
			})
			return s.emitEvent(llm.EventTextBlockStart, blockIndex, "")

		case "thinking":
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type:     llm.TypeThinking,
				Thinking: cb.Thinking,
			})
			return s.emitEvent(llm.EventThinkingBlockStart, blockIndex, "")

		case "redacted_thinking":
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type:         llm.TypeThinking,
				Redacted:     true,
				RedactedData: cb.Data,
			})
			return s.emitEvent(llm.EventThinkingBlockStart, blockIndex, "")

		case "tool_use":
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeToolCall,
				ID:   cb.ID,
				Name: cb.Name,
			})
			s.tools[event.Index] = &toolCallAccumulator{
				rawInput: cb.Input,
				hasDelta: false,
			}
			return s.emitEvent(llm.EventToolCallBlockStart, blockIndex, "")

		default:
			return fmt.Errorf("不支持的内容块类型: %q", cb.Type)
		}

	case "content_block_delta":
		if event.Delta == nil {
			return nil
		}
		blockIndex, ok := s.blockMap[event.Index]
		if !ok {
			return fmt.Errorf("未知的 content block index: %d", event.Index)
		}

		switch event.Delta.Type {
		case "text_delta":
			s.message.Content[blockIndex].Text += event.Delta.Text
			return s.emitEvent(llm.EventTextDelta, blockIndex, event.Delta.Text)

		case "thinking_delta":
			s.message.Content[blockIndex].Thinking += event.Delta.Thinking
			return s.emitEvent(llm.EventThinkingDelta, blockIndex, event.Delta.Thinking)

		case "signature_delta":
			s.message.Content[blockIndex].ThinkingSignature += event.Delta.Signature
			return nil

		case "input_json_delta":
			tool, ok := s.tools[event.Index]
			if !ok {
				return fmt.Errorf("未找到工具调用累积器 index: %d", event.Index)
			}
			tool.hasDelta = true
			tool.args.WriteString(event.Delta.PartialJSON)
			return s.emitEvent(llm.EventToolCallDelta, blockIndex, event.Delta.PartialJSON)

		default:
			return fmt.Errorf("不支持的 delta 类型: %q", event.Delta.Type)
		}

	case "content_block_stop":
		blockIndex, ok := s.blockMap[event.Index]
		if !ok {
			return fmt.Errorf("未知的 content block index: %d", event.Index)
		}
		block := &s.message.Content[blockIndex]

		switch block.Type {
		case llm.TypeText:
			return s.emitEvent(llm.EventTextBlockEnd, blockIndex, "")

		case llm.TypeThinking:
			return s.emitEvent(llm.EventThinkingBlockEnd, blockIndex, "")

		case llm.TypeToolCall:
			tool, ok := s.tools[event.Index]
			if !ok {
				return fmt.Errorf("未找到工具调用累积器 index: %d", event.Index)
			}

			var payload []byte
			if tool.hasDelta {
				payload = []byte(tool.args.String())
			} else {
				payload = tool.rawInput
			}
			if len(payload) == 0 {
				return fmt.Errorf("工具 %s 缺少参数 (未收到 input 或参数增量)", block.Name)
			}

			var args map[string]any
			if err := json.Unmarshal(payload, &args); err != nil {
				return fmt.Errorf("工具 %s 参数解析失败: %w", block.Name, err)
			}
			if args == nil {
				return fmt.Errorf("工具 %s 参数必须是 JSON 对象，不能为 null", block.Name)
			}
			block.Arguments = args
			return s.emitEvent(llm.EventToolCallBlockEnd, blockIndex, "")

		default:
			return nil
		}

	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			s.message.RawStopReason = event.Delta.StopReason
		}
		if event.Usage != nil {
			s.message.Usage.Output = int64(event.Usage.OutputTokens)
			s.message.Usage.Total = s.message.Usage.Input + s.message.Usage.Output + s.message.Usage.CacheRead + s.message.Usage.CacheWrite
			s.message.Usage = llm.CalculateCost(s.message.Usage, s.pricing)
		}
		return nil

	case "message_stop":
		s.receivedMessageStop = true
		return nil

	case "ping":
		return nil

	case "error":
		msg := ""
		if event.Error != nil {
			msg = event.Error.Message
			if msg == "" {
				msg = event.Error.Type
			}
		}
		if msg == "" {
			msg = "未知服务端错误"
		}
		return fmt.Errorf("provider error: %s", msg)

	default:
		// 忽略无关新增事件
		return nil
	}
}

// mapStopReason 把 Anthropic 的 stop_reason 映射成领域层的 StopReason。
func mapStopReason(raw string) (llm.StopReason, error) {
	switch raw {
	case "end_turn", "stop_sequence":
		return llm.StopStop, nil
	case "max_tokens":
		return llm.StopLength, nil
	case "tool_use":
		return llm.StopToolUse, nil
	case "refusal":
		return llm.StopSafety, fmt.Errorf("内容被模型拒绝 (refusal)")
	case "content_filter":
		return llm.StopSafety, fmt.Errorf("内容被安全过滤器拦截 (content_filter)")
	case "pause_turn":
		return "", fmt.Errorf("暂不支持的结束原因: pause_turn")
	case "":
		return "", fmt.Errorf("未收到 stop_reason")
	default:
		return "", fmt.Errorf("未知的 stop_reason %q", raw)
	}
}
