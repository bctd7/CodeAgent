package responses

import "encoding/json"

// 仅声明本适配器支持的 Responses HTTP/SSE 字段，不复用 Chat Completions 结构。
type responseRequest struct {
	Model           string         `json:"model"`
	Input           []any          `json:"input"`
	Instructions    string         `json:"instructions,omitempty"`
	Tools           []functionTool `json:"tools,omitempty"`
	Stream          bool           `json:"stream"`
	Store           bool           `json:"store"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	Reasoning       *reasoning     `json:"reasoning,omitempty"`
	Include         []string       `json:"include,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type functionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type inputMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content any    `json:"content"`
	Phase   string `json:"phase,omitempty"`
}

type inputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputImage struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

type inputCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type inputCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

type outputPart struct {
	Type    string  `json:"type"`
	Text    *string `json:"text,omitempty"`
	Refusal *string `json:"refusal,omitempty"`
}

type summaryPart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

type outputItem struct {
	Type      string       `json:"type"`
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	Role      string       `json:"role"`
	Phase     string       `json:"phase"`
	Content   []outputPart `json:"content"`
	CallID    string       `json:"call_id"`
	Name      string       `json:"name"`
	Arguments *string      `json:"arguments"`
	Namespace string       `json:"namespace"`
	Async     bool         `json:"async"`
	Caller    *struct {
		Type string `json:"type"`
	} `json:"caller"`
	Summary          []summaryPart `json:"summary"`
	EncryptedContent *string       `json:"encrypted_content"`
}

// reasoningItem 保留摘要分段和官方 opaque 字段，不产生或解码隐藏推理。
type reasoningItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []summaryPart `json:"summary"`
	EncryptedContent *string       `json:"encrypted_content,omitempty"`
	Status           string        `json:"status,omitempty"`
}

type responseUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	InputDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responseObject struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Status     string         `json:"status"`
	Output     []outputItem   `json:"output"`
	Usage      *responseUsage `json:"usage"`
	Error      *responseError `json:"error"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type streamEvent struct {
	Type         string          `json:"type"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	SummaryIndex *int            `json:"summary_index"`
	ItemID       string          `json:"item_id"`
	Item         *outputItem     `json:"item"`
	Part         *outputPart     `json:"part"`
	Delta        *string         `json:"delta"`
	Text         *string         `json:"text"`
	Refusal      *string         `json:"refusal"`
	Arguments    *string         `json:"arguments"`
	Response     *responseObject `json:"response"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
}
