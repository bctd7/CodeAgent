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

// defaultMaxSSEFrameSize 是单个 SSE 帧的大小上限。
const defaultMaxSSEFrameSize = 1 << 20 // 1MB

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

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.message.RawStopReason = *choice.FinishReason
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
			if tc.ID != "" {
				s.message.Content[tool.blockIndex].ID = tc.ID
			}
			if tc.Function.Name != "" {
				s.message.Content[tool.blockIndex].Name = tc.Function.Name
			}
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

	// 2. 初始化完整的累积消息，发布 EventMessageStart。
	accumulator := newResponseAccumulator(writer, request.Model.Pricing, initial)
	if err := accumulator.emitEvent(llm.EventMessageStart, 0, ""); err != nil {
		return
	}

	// 3. 读取 SSE 帧，将每帧的 JSON 数据解码为 Chunk 并累积消息。
	err = readSSEFrames(resp.Body, defaultMaxSSEFrameSize, func(data []byte) error {
		var chunk chatCompletionChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return err
		}
		return accumulator.applyChunk(chunk)
	})
	if err != nil {
		// Emit 失败时流内核已经终止，重复收尾不会覆盖其结果。
		abortOrFail(writer, accumulator.message, err)
		return
	}

	// 4. readSSEFrames 正常返回后，检查结束原因并完成内容块。
	stopReason, err := accumulator.completeBlocks()
	if err != nil {
		abortOrFail(writer, accumulator.message, err)
		return
	}

	// 5. 调用 writer.Finish(message)。
	accumulator.message.State = llm.StateSuccess
	accumulator.message.StopReason = stopReason
	writer.Finish(accumulator.message)
}
