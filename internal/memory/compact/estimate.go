package compact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"CodeAgent/internal/llm"
)

// 字符单位与 Pi 的 JavaScript length 一致；不是 tokenizer。
func textChars(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

func charsToTokens(chars int) int {
	tokens := chars / 4
	if chars%4 != 0 {
		tokens++
	}
	return tokens
}

// 禁用 HTML 转义，避免把 JavaScript 中的一个字符估算成六个字符。
func marshalArguments(arguments map[string]any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(arguments); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})), nil
}

func addTokens(a, b int) (int, error) {
	if b < 0 || a > int(^uint(0)>>1)-b {
		return 0, fmt.Errorf("token estimate overflow")
	}
	return a + b, nil
}

// EstimateTokens 按 Pi 的字符数/4 启发式估算单条消息，不计完整请求开销。
// 图片使用固定近似值，不读取 base64；调用方不得当作精确计费值。
func EstimateTokens(message llm.Message) (int, error) {
	content, err := messageContent(message)
	if err != nil {
		return 0, err
	}
	_, assistant := message.(*llm.AssistantMessage)
	chars := 0
	for _, block := range content {
		part := 0
		switch block.Type {
		case llm.TypeText:
			if !utf8.ValidString(block.Text) {
				return 0, fmt.Errorf("invalid text UTF-8")
			}
			part = textChars(block.Text)
		case llm.TypeImage:
			part = 4800
		case llm.TypeThinking:
			if !assistant {
				return 0, fmt.Errorf("thinking outside assistant message")
			}
			if !block.Redacted {
				if !utf8.ValidString(block.Thinking) {
					return 0, fmt.Errorf("invalid thinking UTF-8")
				}
				part = textChars(block.Thinking)
			}
		case llm.TypeToolCall:
			if !assistant || block.ID == "" || block.Name == "" || !utf8.ValidString(block.ID) || !utf8.ValidString(block.Name) {
				return 0, fmt.Errorf("invalid tool call")
			}
			arguments, err := marshalArguments(block.Arguments)
			if err != nil {
				return 0, fmt.Errorf("tool arguments: %w", err)
			}
			part, err = addTokens(textChars(block.Name), textChars(arguments))
			if err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("unsupported content type %q", block.Type)
		}
		chars, err = addTokens(chars, part)
		if err != nil {
			return 0, err
		}
	}
	return charsToTokens(chars), nil
}

func messageContent(message llm.Message) ([]llm.ContentBlock, error) {
	switch m := message.(type) {
	case *llm.UserMessage:
		if m != nil {
			return m.Content, nil
		}
	case *llm.AssistantMessage:
		if m != nil {
			return m.Content, nil
		}
	case *llm.ToolResultMessage:
		if m != nil {
			return m.Content, nil
		}
	}
	return nil, fmt.Errorf("nil or unsupported message %T", message)
}
