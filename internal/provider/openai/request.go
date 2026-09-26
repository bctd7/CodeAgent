package openai

import (
	"encoding/json"
	"fmt"
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

	case *llm.ToolResultMessage:
		for _, block := range m.Content {
			if block.Type == llm.TypeImage {
				return chatMessage{}, fmt.Errorf("Chat Completions 协议不支持工具结果包含图片")
			}
		}
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
