package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"CodeAgent/internal/llm"
)

// EncodeMessage 将消息序列化为标准 JSON 文本。
func EncodeMessage(message llm.Message) ([]byte, error) {
	if message == nil {
		return nil, fmt.Errorf("编码消息: 消息为 nil")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("编码消息失败: %w", err)
	}
	return data, nil
}

// DecodeMessage 按 Role 恢复具体消息。保留失败、取消及不完整响应，供历史审计使用。
// JSON 字段沿用 LLM 结构体；字段变更时须同步考虑已有记录的迁移。
func DecodeMessage(data []byte) (llm.Message, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("解码消息: JSON 不是有效的 UTF-8")
	}
	var head struct {
		Role llm.Role `json:"role"`
	}
	// Unmarshal 同时检查完整 JSON，拒绝尾随的第二个值。
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("读取消息角色: %w", err)
	}

	var message llm.Message
	switch head.Role {
	case llm.RoleUser:
		message = &llm.UserMessage{}
	case llm.RoleAssistant:
		message = &llm.AssistantMessage{}
	case llm.RoleTool:
		message = &llm.ToolResultMessage{}
	default:
		return nil, fmt.Errorf("解码消息: 未知或缺失的角色 %q", head.Role)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	// 工具参数中的数字恢复为 json.Number，避免大整数经过 float64 后失真。
	decoder.UseNumber()
	// 不静默丢弃当前程序无法识别的持久化字段。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(message); err != nil {
		return nil, fmt.Errorf("解码 %s 消息: %w", head.Role, err)
	}
	return message, nil
}
