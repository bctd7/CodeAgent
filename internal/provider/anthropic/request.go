package anthropic

import (
	"encoding/json"
	"fmt"

	"CodeAgent/internal/llm"
)

// buildRequestBody 将领域层的 llm.Request 校验并序列化为 Anthropic Messages 协议请求体。
func buildRequestBody(request llm.Request) ([]byte, error) {
	// 1. max_tokens 校验：请求选项优先，其次模型配置；必须为正数，不猜模型上限
	maxTokens := request.Options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = request.Model.MaxTokens
	}
	if maxTokens <= 0 {
		return nil, fmt.Errorf("anthropic 请求缺少有效的 max_tokens (需在 options 或 model 中配置正数)")
	}

	// 2. 独立装配 thinking
	thinking, err := buildThinking(request.Options.Anthropic, maxTokens)
	if err != nil {
		return nil, err
	}

	// 3. 独立装配 output_config
	outputConfig := buildOutputConfig(request.Options.Anthropic)

	// 4. 转换消息
	messages, err := convertMessages(request.Messages)
	if err != nil {
		return nil, err
	}

	// 5. 转换工具
	tools, err := convertTools(request.Tools)
	if err != nil {
		return nil, err
	}

	wireReq := messageRequestWire{
		Model:        request.Model.ID,
		Messages:     messages,
		System:       request.SystemPrompt,
		MaxTokens:    maxTokens,
		Temperature:  request.Options.Temperature,
		Stream:       true,
		Tools:        tools,
		Thinking:     thinking,
		OutputConfig: outputConfig,
	}

	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("序列化 anthropic 请求失败: %w", err)
	}

	return body, nil
}

func buildThinking(opts *llm.AnthropicOptions, maxTokens int) (*thinkingWire, error) {
	if opts == nil || opts.Thinking == "" {
		return nil, nil
	}

	switch opts.Thinking {
	case "enabled":
		if opts.BudgetTokens <= 0 {
			return nil, fmt.Errorf("thinking 设为 enabled 时必须配置正数 budget_tokens")
		}
		if opts.BudgetTokens >= maxTokens {
			return nil, fmt.Errorf("budget_tokens (%d) 必须小于本次 max_tokens (%d)", opts.BudgetTokens, maxTokens)
		}
		return &thinkingWire{
			Type:         "enabled",
			BudgetTokens: opts.BudgetTokens,
		}, nil

	case "adaptive":
		return &thinkingWire{
			Type: "adaptive",
		}, nil

	case "disabled":
		return &thinkingWire{
			Type: "disabled",
		}, nil

	default:
		tw := &thinkingWire{
			Type: opts.Thinking,
		}
		if opts.BudgetTokens > 0 {
			tw.BudgetTokens = opts.BudgetTokens
		}
		return tw, nil
	}
}

func buildOutputConfig(opts *llm.AnthropicOptions) *outputConfigWire {
	if opts == nil || opts.Effort == "" {
		return nil
	}
	return &outputConfigWire{
		Effort: opts.Effort,
	}
}

func convertMessages(msgs []llm.Message) ([]messageWire, error) {
	var result []messageWire

	appendUserBlocks := func(blocks []contentBlockWire) {
		if len(result) > 0 && result[len(result)-1].Role == "user" {
			result[len(result)-1].Content = append(result[len(result)-1].Content, blocks...)
		} else {
			result = append(result, messageWire{
				Role:    "user",
				Content: blocks,
			})
		}
	}

	for _, msg := range msgs {
		switch m := msg.(type) {
		case *llm.UserMessage:
			blocks, err := convertTextImageContent(m.Content)
			if err != nil {
				return nil, err
			}
			appendUserBlocks(blocks)

		case *llm.ToolResultMessage:
			block, err := convertToolResult(m)
			if err != nil {
				return nil, err
			}
			appendUserBlocks([]contentBlockWire{block})

		case *llm.AssistantMessage:
			blocks, err := convertAssistantContent(m.Content)
			if err != nil {
				return nil, err
			}
			if len(result) > 0 && result[len(result)-1].Role == "assistant" {
				result[len(result)-1].Content = append(result[len(result)-1].Content, blocks...)
			} else {
				result = append(result, messageWire{
					Role:    "assistant",
					Content: blocks,
				})
			}

		default:
			return nil, fmt.Errorf("不支持的消息类型: %T", msg)
		}
	}

	return result, nil
}

func convertTextImageContent(contents []llm.ContentBlock) ([]contentBlockWire, error) {
	var blocks []contentBlockWire
	for _, c := range contents {
		switch c.Type {
		case llm.TypeText:
			text := c.Text
			blocks = append(blocks, contentBlockWire{
				Type: "text",
				Text: &text,
			})
		case llm.TypeImage:
			if c.MimeType == "" {
				return nil, fmt.Errorf("图片内容块缺少 mime_type")
			}
			blocks = append(blocks, contentBlockWire{
				Type: "image",
				Source: &imageSourceWire{
					Type:      "base64",
					MediaType: c.MimeType,
					Data:      c.Data,
				},
			})
		default:
			return nil, fmt.Errorf("内容块不支持的内容类型: %q", c.Type)
		}
	}
	return blocks, nil
}

func convertAssistantContent(contents []llm.ContentBlock) ([]contentBlockWire, error) {
	var blocks []contentBlockWire
	for _, c := range contents {
		switch c.Type {
		case llm.TypeText:
			text := c.Text
			blocks = append(blocks, contentBlockWire{
				Type: "text",
				Text: &text,
			})
		case llm.TypeThinking:
			if c.Redacted {
				blocks = append(blocks, contentBlockWire{
					Type: "redacted_thinking",
					Data: c.RedactedData,
				})
			} else {
				thinking := c.Thinking
				blocks = append(blocks, contentBlockWire{
					Type:      "thinking",
					Thinking:  &thinking,
					Signature: c.ThinkingSignature,
				})
			}
		case llm.TypeToolCall:
			args := c.Arguments
			if args == nil {
				args = map[string]any{}
			}
			blocks = append(blocks, contentBlockWire{
				Type:  "tool_use",
				ID:    c.ID,
				Name:  c.Name,
				Input: &args,
			})
		default:
			return nil, fmt.Errorf("assistant 消息不支持的内容块类型: %q", c.Type)
		}
	}
	return blocks, nil
}

func convertToolResult(msg *llm.ToolResultMessage) (contentBlockWire, error) {
	block := contentBlockWire{
		Type:      "tool_result",
		ToolUseID: msg.ToolCallID,
		IsError:   msg.IsError,
	}

	if len(msg.Content) == 0 {
		block.Content = ""
		return block, nil
	}

	inner, err := convertTextImageContent(msg.Content)
	if err != nil {
		return contentBlockWire{}, err
	}
	block.Content = inner
	return block, nil
}

func convertTools(tools []llm.Tool) ([]toolWire, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	var result []toolWire
	for _, t := range tools {
		var schema map[string]any
		if len(t.Schema) > 0 {
			if err := json.Unmarshal(t.Schema, &schema); err != nil {
				return nil, fmt.Errorf("工具 %s schema 序列化失败: %w", t.Name, err)
			}
		}
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		result = append(result, toolWire{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return result, nil
}
