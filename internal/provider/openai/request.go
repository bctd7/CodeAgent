package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"CodeAgent/internal/llm"
)

// contentPart 是多模态 content 数组里的一个元素。
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

// imageURL 是图片内容的 data URL。
type imageURL struct {
	URL string `json:"url"`
}

// buildRequestBody 把领域层的 llm.Request 翻译成 wire 的 JSON 请求体。
func buildRequestBody(request llm.Request) ([]byte, error) {
	wire := chatRequest{
		Model:         request.Model.ID,
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
		Temperature:   request.Options.Temperature,
		MaxTokens:     request.Options.MaxTokens,
	}
	if wire.MaxTokens == 0 {
		wire.MaxTokens = request.Model.MaxTokens
	}
	if request.SystemPrompt != "" {
		wire.Messages = append(wire.Messages, chatMessage{Role: "system", Content: request.SystemPrompt})
	}
	for _, message := range request.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return nil, err
		}
		wire.Messages = append(wire.Messages, converted)
	}
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

// convertMessage 把一条 llm.Message 翻译成 wire 的 chatMessage。
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
		return chatMessage{
			Role:             "assistant",
			Content:          content,
			ReasoningContent: convertReasoning(m.Content),
			ToolCalls:        convertToolCalls(m.Content),
		}, nil

	case *llm.ToolResultMessage:
		content, err := convertContent(m.Content)
		if err != nil {
			return chatMessage{}, err
		}
		return chatMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: content}, nil

	default:
		return chatMessage{}, fmt.Errorf("不支持的消息类型 %T", message)
	}
}

// convertContent 把 text + image 块转成 content 字段。
// 无图片 → 字符串；有图片 → contentPart 数组。
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
		return strings.Join(texts, "\n"), nil
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

// convertReasoning 把 thinking 块拼成 reasoning_content 字符串。
func convertReasoning(blocks []llm.ContentBlock) string {
	var parts []string
	for _, block := range blocks {
		if block.Type == llm.TypeThinking {
			parts = append(parts, block.Thinking)
		}
	}
	return strings.Join(parts, "\n")
}

// convertToolCalls 把 toolCall 块转成 tool_calls 数组。
func convertToolCalls(blocks []llm.ContentBlock) []chatToolCall {
	var calls []chatToolCall
	for _, block := range blocks {
		if block.Type != llm.TypeToolCall {
			continue
		}
		args, _ := json.Marshal(block.Arguments) // map[string]any → JSON 字符串
		calls = append(calls, chatToolCall{
			ID:       block.ID,
			Type:     "function",
			Function: functionCallWire{Name: block.Name, Arguments: string(args)},
		})
	}
	return calls
}
