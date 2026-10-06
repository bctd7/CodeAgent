package chat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"CodeAgent/internal/llm"
)

// buildRequestBody 把领域层的 llm.Request 翻译成 wire 的 JSON 请求体。
func buildRequestBody(request llm.Request) ([]byte, error) {
	wire := chatRequest{
		Model:         request.Model.ID,
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
		Temperature:   request.Options.Temperature,
	}

	// 输出限制优先使用请求选项，其次模型配置；均未设置时省略。
	maxTokens := request.Options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = request.Model.MaxTokens
	}
	if maxTokens > 0 {
		if request.Options.OpenAI != nil && request.Options.OpenAI.MaxTokensField == "max_completion_tokens" {
			wire.MaxCompletionTokens = maxTokens
		} else {
			wire.MaxTokens = maxTokens
		}
	}

	// reasoning_effort 只在显式配置时发送。
	if request.Options.OpenAI != nil && request.Options.OpenAI.ReasoningEffort != "" {
		wire.ReasoningEffort = request.Options.OpenAI.ReasoningEffort
	}

	if request.SystemPrompt != "" {
		wire.Messages = append(wire.Messages, chatMessage{Role: "system", Content: request.SystemPrompt})
	}
	messages, err := convertMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	wire.Messages = append(wire.Messages, messages...)
	for _, tool := range request.Tools {
		wire.Tools = append(wire.Tools, chatTool{
			Type: "function",
			Function: functionTool{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  json.RawMessage(tool.Schema),
			},
		})
	}
	return json.Marshal(wire)
}

// convertMessages 按连续工具结果分组：先发出所有工具文本，再追加带来源的图片消息。
// 只调整请求消息，不修改领域历史。
func convertMessages(messages []llm.Message) ([]chatMessage, error) {
	var out []chatMessage
	for i := 0; i < len(messages); {
		if _, ok := messages[i].(*llm.ToolResultMessage); !ok {
			message, err := convertMessage(messages[i])
			if err != nil {
				return nil, err
			}
			out = append(out, message)
			i++
			continue
		}
		var images []llm.ContentBlock
		for i < len(messages) {
			tool, ok := messages[i].(*llm.ToolResultMessage)
			if !ok {
				break
			}
			var text strings.Builder
			var attached []llm.ContentBlock
			for _, block := range tool.Content {
				switch block.Type {
				case llm.TypeText:
					text.WriteString(block.Text)
				case llm.TypeImage:
					if err := validateToolImage(block); err != nil {
						return nil, fmt.Errorf("工具 %q 调用 %q: %w", tool.ToolName, tool.ToolCallID, err)
					}
					attached = append(attached, block)
				}
			}
			if len(attached) > 0 {
				status := "success"
				if tool.IsError {
					status = "error"
				}
				label := fmt.Sprintf("Images from tool %q (tool_call_id=%q, status=%s):", tool.ToolName, tool.ToolCallID, status)
				images = append(images, llm.ContentBlock{Type: llm.TypeText, Text: label})
				images = append(images, attached...)
				if text.Len() == 0 {
					text.WriteString("Tool returned image(s); see the attached tool images below. Status: " + status + ".")
				}
			}
			out = append(out, chatMessage{Role: "tool", ToolCallID: tool.ToolCallID, Content: text.String()})
			i++
		}
		if len(images) > 0 {
			content, err := convertContent(images)
			if err != nil {
				return nil, err
			}
			out = append(out, chatMessage{Role: "user", Content: content})
		}
	}
	return out, nil
}

// convertMessage 转换非工具消息；工具结果由 convertMessages 分组处理。
func convertMessage(message llm.Message) (chatMessage, error) {
	switch m := message.(type) {
	case *llm.UserMessage:
		content, err := convertContent(m.Content)
		if err != nil {
			return chatMessage{}, err
		}
		return chatMessage{Role: "user", Content: content}, nil

	case *llm.AssistantMessage:
		content, err := convertContent(m.Content)
		if err != nil {
			return chatMessage{}, err
		}
		toolCalls, err := convertToolCalls(m.Content)
		if err != nil {
			return chatMessage{}, err
		}
		return chatMessage{
			Role:             "assistant",
			Content:          content,
			ReasoningContent: convertReasoning(m.Content),
			ToolCalls:        toolCalls,
		}, nil

	default:
		return chatMessage{}, fmt.Errorf("不支持的消息类型 %T", message)
	}
}

// convertContent 把 text + image 块转成 content 字段。
// 无图片 → 字符串（拼接不擅自加入换行）；有图片 → contentPart 数组。
func convertContent(blocks []llm.ContentBlock) (any, error) {
	hasImage := false
	for _, block := range blocks {
		if block.Type == llm.TypeImage {
			hasImage = true
			break
		}
	}
	if !hasImage {
		var texts []string
		for _, block := range blocks {
			if block.Type == llm.TypeText {
				texts = append(texts, block.Text)
			}
		}
		return strings.Join(texts, ""), nil
	}
	parts := make([]contentPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case llm.TypeText:
			parts = append(parts, contentPart{Type: "text", Text: block.Text})
		case llm.TypeImage:
			parts = append(parts, contentPart{
				Type:     "image_url",
				ImageURL: &imageURL{URL: "data:" + block.MimeType + ";base64," + block.Data},
			})
		}
	}
	return parts, nil
}

// convertReasoning 把 thinking 块拼成 reasoning_content 字符串，不擅自加入换行。
func convertReasoning(blocks []llm.ContentBlock) string {
	var parts []string
	for _, block := range blocks {
		if block.Type == llm.TypeThinking {
			parts = append(parts, block.Thinking)
		}
	}
	return strings.Join(parts, "")
}

// convertToolCalls 把 toolCall 块转成 tool_calls 数组。序列化失败返回具体错误。
func convertToolCalls(blocks []llm.ContentBlock) ([]chatToolCall, error) {
	var calls []chatToolCall
	for _, block := range blocks {
		if block.Type != llm.TypeToolCall {
			continue
		}
		args, err := json.Marshal(block.Arguments)
		if err != nil {
			return nil, fmt.Errorf("工具 %s 参数序列化失败: %w", block.Name, err)
		}
		calls = append(calls, chatToolCall{
			ID:       block.ID,
			Type:     "function",
			Function: functionCallWire{Name: block.Name, Arguments: string(args)},
		})
	}
	return calls, nil
}

// validateToolImage 仅验证传输语法，不解码像素，也不声称模型支持该格式。
// 保留现有 read 可输出的 MIME；具体模型可接受的格式由服务端校验。
func validateToolImage(block llm.ContentBlock) error {
	switch block.MimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
	default:
		return fmt.Errorf("不支持的图片 MIME %q", block.MimeType)
	}
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(block.Data)))
	if err != nil || n == 0 {
		return fmt.Errorf("图片数据必须为非空标准 base64")
	}
	return nil
}
