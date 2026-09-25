package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"CodeAgent/internal/llm"
)

// defaultMaxSSEEventSize 是单个 SSE 事件的大小上限。
const defaultMaxSSEEventSize = 1 << 20 // 1MB

// mapStopReason 把 OpenAI 的 finish_reason 映射成领域层的 StopReason。
// 返回 false 表示未知的结束原因。
func mapStopReason(raw string) (llm.StopReason, bool) {
	switch raw {
	case "stop", "end":
		return llm.StopStop, true
	case "length":
		return llm.StopLength, true
	case "tool_calls", "function_call":
		return llm.StopToolUse, true
	default:
		return "", false
	}
}

// abortOrFail 统一处理"取消就 Abort，否则 Fail"的收尾。
// 出错时使用当前累积消息 msg 收尾，保留已生成的内容。
func abortOrFail(writer llm.StreamWriter, msg llm.AssistantMessage, err error) {
	if cause := writer.Context().Err(); cause != nil {
		writer.Abort(cause)
		return
	}
	msg.ErrorMessage = err.Error()
	writer.Fail(msg, nil)
}

// snapshotMessage 仅复制 Content 切片头及元素，底层字符串和解析完成的参数 Map 保持只读复用。
func snapshotMessage(msg llm.AssistantMessage) *llm.AssistantMessage {
	snap := msg
	if len(msg.Content) > 0 {
		snap.Content = make([]llm.ContentBlock, len(msg.Content))
		copy(snap.Content, msg.Content)
	}
	return &snap
}

type toolCallState struct {
	contentIndex int
	args         strings.Builder
}

type streamState struct {
	writer        llm.StreamWriter
	pricing       llm.Pricing
	message       llm.AssistantMessage
	textIndex     int
	thinkingIndex int
	tools         map[int]*toolCallState // key: chunk 中的 tool_call index
}

func newStreamState(writer llm.StreamWriter, pricing llm.Pricing, initial llm.AssistantMessage) *streamState {
	return &streamState{
		writer:        writer,
		pricing:       pricing,
		message:       initial,
		textIndex:     -1,
		thinkingIndex: -1,
		tools:         make(map[int]*toolCallState),
	}
}

func (s *streamState) emitEvent(evtType llm.EventType, contentIndex int, delta string) error {
	return s.writer.Emit(llm.Event{
		Type:         evtType,
		ContentIndex: contentIndex,
		Delta:        delta,
		Partial:      snapshotMessage(s.message),
	})
}

func (s *streamState) handleChunk(chunk chatCompletionChunk) error {
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

	// 3. choice index 0 的增量与 4. choice index 0 的结束原因
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.message.RawStopReason = *choice.FinishReason
		}
		if err := s.applyDelta(choice.Delta); err != nil {
			return err
		}
	}
	return nil
}

func (s *streamState) applyDelta(delta chunkDelta) error {
	// 思考增量
	if delta.ReasoningContent != "" {
		if s.thinkingIndex == -1 {
			s.thinkingIndex = len(s.message.Content)
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeThinking,
			})
			if err := s.emitEvent(llm.EventThinkingStart, s.thinkingIndex, ""); err != nil {
				return err
			}
		}
		s.message.Content[s.thinkingIndex].Thinking += delta.ReasoningContent
		if err := s.emitEvent(llm.EventThinkingDelta, s.thinkingIndex, delta.ReasoningContent); err != nil {
			return err
		}
	}

	// 文本增量
	if delta.Content != "" {
		if s.textIndex == -1 {
			s.textIndex = len(s.message.Content)
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeText,
			})
			if err := s.emitEvent(llm.EventTextStart, s.textIndex, ""); err != nil {
				return err
			}
		}
		s.message.Content[s.textIndex].Text += delta.Content
		if err := s.emitEvent(llm.EventTextDelta, s.textIndex, delta.Content); err != nil {
			return err
		}
	}

	// 工具增量
	for _, tc := range delta.ToolCalls {
		tool, exists := s.tools[tc.Index]
		if !exists {
			contentIndex := len(s.message.Content)
			s.message.Content = append(s.message.Content, llm.ContentBlock{
				Type: llm.TypeToolCall,
				ID:   tc.ID,
				Name: tc.Function.Name,
			})
			tool = &toolCallState{contentIndex: contentIndex}
			s.tools[tc.Index] = tool
			if err := s.emitEvent(llm.EventToolCallStart, contentIndex, ""); err != nil {
				return err
			}
		} else {
			if tc.ID != "" {
				s.message.Content[tool.contentIndex].ID = tc.ID
			}
			if tc.Function.Name != "" {
				s.message.Content[tool.contentIndex].Name = tc.Function.Name
			}
		}
		if tc.Function.Arguments != "" {
			tool.args.WriteString(tc.Function.Arguments)
			if err := s.emitEvent(llm.EventToolCallDelta, tool.contentIndex, tc.Function.Arguments); err != nil {
				return err
			}
		}
	}

	return nil
}

// completeBlocks 仅完成参数解析和块结束事件；失败由 run 统一收尾。
func (s *streamState) completeBlocks() (llm.StopReason, error) {
	// 1. 检查是否收到可识别的 finish_reason
	if s.message.RawStopReason == "" {
		return "", fmt.Errorf("未收到 finish_reason")
	}
	stopReason, ok := mapStopReason(s.message.RawStopReason)
	if !ok {
		return "", fmt.Errorf("未知的 finish_reason %q", s.message.RawStopReason)
	}

	// 2. 解析所有工具调用的参数，写入对应块
	for _, tool := range s.tools {
		block := &s.message.Content[tool.contentIndex]
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
			eventType = llm.EventTextEnd
		case llm.TypeThinking:
			eventType = llm.EventThinkingEnd
		case llm.TypeToolCall:
			eventType = llm.EventToolCallEnd
		default:
			continue
		}
		if err := s.emitEvent(eventType, idx, ""); err != nil {
			return "", err
		}
	}

	return stopReason, nil
}

// run 在后台 goroutine 里执行整个请求-响应流程。
func (c *Client) run(writer llm.StreamWriter, request llm.Request, endpoint string, body []byte, initial llm.AssistantMessage) {
	// 1. 构造并发送 HTTP 请求，检查响应。
	apiKey := os.Getenv(c.envKey)
	req, err := http.NewRequestWithContext(writer.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		abortOrFail(writer, initial, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		abortOrFail(writer, initial, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		abortOrFail(writer, initial, fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}

	// 2. 初始化完整的累积消息，发布 EventStart。
	stream := newStreamState(writer, request.Model.Pricing, initial)
	if err := stream.emitEvent(llm.EventStart, 0, ""); err != nil {
		return
	}

	// 3. 调用 readSSE，在回调中解析和处理每个 JSON chunk。
	err = readSSE(resp.Body, defaultMaxSSEEventSize, func(data []byte) error {
		var chunk chatCompletionChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return err
		}
		return stream.handleChunk(chunk)
	})
	if err != nil {
		// Emit 失败时流内核已经终止，重复收尾不会覆盖其结果。
		abortOrFail(writer, stream.message, err)
		return
	}

	// 4. readSSE 正常返回后，检查结束原因并完成内容块。
	stopReason, err := stream.completeBlocks()
	if err != nil {
		abortOrFail(writer, stream.message, err)
		return
	}

	// 5. 调用 writer.Finish(message)。
	stream.message.State = llm.StateSuccess
	stream.message.StopReason = stopReason
	writer.Finish(stream.message)
}
