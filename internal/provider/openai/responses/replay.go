package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"CodeAgent/internal/llm"
)

const reasoningPrefix = "openai-responses/reasoning-v1:"
const textPrefix = "openai-responses/text-v1:"

// 来源是本地回放约束，不属于发给 API 的 reasoning item，不保存任何凭据。
type origin struct {
	Provider string      `json:"provider"`
	API      llm.APIKind `json:"api"`
	Model    string      `json:"model"`
	BaseURL  string      `json:"base_url"`
}

func modelOrigin(model llm.Model) origin {
	return origin{Provider: model.Provider, API: APIKind, Model: model.ID, BaseURL: model.BaseURL}
}

func (o origin) matches(message *llm.AssistantMessage, model llm.Model) bool {
	return o == modelOrigin(model) && message.API == APIKind && message.Provider == o.Provider && message.Model == o.Model
}

type reasoningReplay struct {
	Origin origin        `json:"origin"`
	Item   reasoningItem `json:"item"`
}

type textReplay struct {
	Origin origin `json:"origin"`
	ID     string `json:"id"`
	Phase  string `json:"phase,omitempty"`
}

func summaryText(parts []summaryPart) string {
	var text strings.Builder
	for _, part := range parts {
		if part.Text != nil {
			text.WriteString(*part.Text)
		}
	}
	return text.String()
}

func validateReasoning(item reasoningItem) error {
	if item.Type != "reasoning" || item.ID == "" || item.Summary == nil {
		return fmt.Errorf("reasoning item 缺少合法 type/id/summary")
	}
	if item.Status != "" && item.Status != "completed" {
		return fmt.Errorf("reasoning item 尚未完成")
	}
	for _, part := range item.Summary {
		if part.Type != "summary_text" || part.Text == nil {
			return fmt.Errorf("不支持的 reasoning 摘要类型 %q", part.Type)
		}
	}
	return nil
}

func saveReasoning(item outputItem, model llm.Model) (string, error) {
	value := reasoningItem{Type: item.Type, ID: item.ID, Summary: item.Summary, EncryptedContent: item.EncryptedContent, Status: item.Status}
	if err := validateReasoning(value); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(reasoningReplay{Origin: modelOrigin(model), Item: value})
	if err != nil {
		return "", err
	}
	return reasoningPrefix + string(encoded), nil
}

func replayReasoning(block llm.ContentBlock, message *llm.AssistantMessage, model llm.Model) (*reasoningItem, error) {
	// 纯文本思考、别的协议签名、取消和截断响应均不能升级为 Responses reasoning。
	if !strings.HasPrefix(block.ThinkingSignature, reasoningPrefix) || message.State != llm.StateSuccess || message.StopReason == llm.StopLength {
		return nil, nil
	}
	var saved reasoningReplay
	if err := json.Unmarshal([]byte(strings.TrimPrefix(block.ThinkingSignature, reasoningPrefix)), &saved); err != nil {
		return nil, fmt.Errorf("reasoning 回放元数据损坏")
	}
	if !saved.Origin.matches(message, model) {
		return nil, nil
	}
	if err := validateReasoning(saved.Item); err != nil {
		return nil, err
	}
	if summaryText(saved.Item.Summary) != block.Thinking {
		return nil, fmt.Errorf("reasoning 摘要已被修改，无法原样回放")
	}
	return &saved.Item, nil
}

func saveTextMetadata(item outputItem, model llm.Model) string {
	encoded, _ := json.Marshal(textReplay{Origin: modelOrigin(model), ID: item.ID, Phase: item.Phase})
	return textPrefix + string(encoded)
}

func replayTextMetadata(block llm.ContentBlock, message *llm.AssistantMessage, model llm.Model) (string, string, error) {
	if !strings.HasPrefix(block.TextSignature, textPrefix) {
		return "", "", nil
	}
	var saved textReplay
	if err := json.Unmarshal([]byte(strings.TrimPrefix(block.TextSignature, textPrefix)), &saved); err != nil {
		return "", "", fmt.Errorf("文本回放元数据损坏")
	}
	if !saved.Origin.matches(message, model) {
		return "", "", nil
	}
	if saved.Phase != "" && saved.Phase != "commentary" && saved.Phase != "final_answer" {
		return "", "", fmt.Errorf("不支持的 assistant phase")
	}
	return saved.ID, saved.Phase, nil
}
