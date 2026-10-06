package responses

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"CodeAgent/internal/llm"
)

func buildRequestBody(request llm.Request) ([]byte, error) {
	wire := responseRequest{
		Model: request.Model.ID, Input: []any{}, Instructions: request.SystemPrompt,
		Stream: true, Store: false, Temperature: request.Options.Temperature,
		MaxOutputTokens: request.Options.MaxTokens,
	}
	if wire.MaxOutputTokens <= 0 {
		wire.MaxOutputTokens = request.Model.MaxTokens
	}
	if wire.MaxOutputTokens < 0 {
		wire.MaxOutputTokens = 0
	}
	effort := ""
	if request.Options.OpenAI != nil {
		effort = request.Options.OpenAI.ReasoningEffort
	}
	if request.Model.Reasoning || effort != "" {
		wire.Reasoning = &reasoning{Effort: effort, Summary: "auto"}
		wire.Include = []string{"reasoning.encrypted_content"}
	}
	for i, message := range request.Messages {
		items, err := convertMessage(message, request.Model)
		if err != nil {
			return nil, fmt.Errorf("openairesponses: 消息 %d: %w", i, err)
		}
		wire.Input = append(wire.Input, items...)
	}
	for i, tool := range request.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return nil, fmt.Errorf("openairesponses: 工具 %d 缺少名称", i)
		}
		schema := json.RawMessage(tool.Schema)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(schema, &object) != nil || object == nil {
			return nil, fmt.Errorf("openairesponses: 工具 %d 的 schema 必须是 JSON 对象", i)
		}
		// 原 schema 不改写；显式关闭 strict，避免 Responses 自动将可选参数变为必填。
		wire.Tools = append(wire.Tools, functionTool{
			Type: "function", Name: tool.Name, Description: tool.Description, Parameters: schema, Strict: false,
		})
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openairesponses: 请求编码失败: %w", err)
	}
	return data, nil
}

func convertMessage(message llm.Message, model llm.Model) ([]any, error) {
	switch m := message.(type) {
	case *llm.UserMessage:
		if m == nil {
			return nil, fmt.Errorf("user 消息为 nil")
		}
		content, err := textImages(m.Content)
		if err != nil {
			return nil, err
		}
		if len(content) == 0 {
			content = []any{inputText{Type: "input_text", Text: ""}}
		}
		return []any{inputMessage{Type: "message", Role: "user", Content: content}}, nil
	case *llm.ToolResultMessage:
		if m == nil || m.ToolCallID == "" {
			return nil, fmt.Errorf("工具结果缺少 call_id")
		}
		// 上层负责 ModelContent 投影，本层只序列化当前 Content，不修改历史或注入 user。
		content, err := textImages(m.Content)
		if err != nil {
			return nil, err
		}
		var output any = content
		if len(content) == 0 {
			output = ""
		}
		return []any{inputCallOutput{Type: "function_call_output", CallID: m.ToolCallID, Output: output}}, nil
	case *llm.AssistantMessage:
		if m == nil {
			return nil, fmt.Errorf("assistant 消息为 nil")
		}
		return assistantItems(m, model)
	default:
		return nil, fmt.Errorf("不支持的消息类型 %T", message)
	}
}

func assistantItems(message *llm.AssistantMessage, model llm.Model) ([]any, error) {
	var items []any
	var text strings.Builder
	var textKey, phase string
	haveText := false
	flush := func() {
		if haveText {
			items = append(items, inputMessage{Type: "message", Role: "assistant", Content: text.String(), Phase: phase})
			text.Reset()
			haveText = false
		}
	}
	for _, block := range message.Content {
		if block.Type != llm.TypeText {
			flush()
		}
		switch block.Type {
		case llm.TypeText:
			key, nextPhase, err := replayTextMetadata(block, message, model)
			if err != nil {
				return nil, err
			}
			if haveText && textKey != key {
				flush()
			}
			textKey, phase, haveText = key, nextPhase, true
			text.WriteString(block.Text)
		case llm.TypeThinking:
			item, err := replayReasoning(block, message, model)
			if err != nil {
				return nil, err
			}
			if item != nil {
				items = append(items, item)
			}
		case llm.TypeToolCall:
			if block.ID == "" || block.Name == "" {
				return nil, fmt.Errorf("工具调用缺少 call_id 或 name")
			}
			if block.Namespace != "" {
				return nil, fmt.Errorf("尚不支持命名空间工具")
			}
			args := block.Arguments
			if args == nil {
				args = map[string]any{}
			}
			encoded, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("工具参数不是合法 JSON: %w", err)
			}
			items = append(items, inputCall{Type: "function_call", CallID: block.ID, Name: block.Name, Arguments: string(encoded)})
		default:
			return nil, fmt.Errorf("assistant 不支持内容类型 %q", block.Type)
		}
	}
	flush()
	return items, nil
}

func textImages(blocks []llm.ContentBlock) ([]any, error) {
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case llm.TypeText:
			parts = append(parts, inputText{Type: "input_text", Text: block.Text})
		case llm.TypeImage:
			switch block.MimeType {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
			default:
				return nil, fmt.Errorf("图片 mime_type 不支持")
			}
			if block.Data == "" {
				return nil, fmt.Errorf("图片数据为空")
			}
			if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(block.Data))); err != nil {
				return nil, fmt.Errorf("图片数据不是合法 base64")
			}
			parts = append(parts, inputImage{Type: "input_image", ImageURL: "data:" + block.MimeType + ";base64," + block.Data, Detail: "auto"})
		default:
			return nil, fmt.Errorf("仅支持 text/image 输入，收到 %q", block.Type)
		}
	}
	return parts, nil
}
