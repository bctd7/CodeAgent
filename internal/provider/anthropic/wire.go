package anthropic

import "encoding/json"

// --- 请求侧 -------------------------------------------------------------

// messageRequestWire 是发送给 Anthropic Messages API 的请求体。
type messageRequestWire struct {
	Model        string            `json:"model"`
	Messages     []messageWire     `json:"messages"`
	System       string            `json:"system,omitempty"`
	MaxTokens    int               `json:"max_tokens"`
	Temperature  *float64          `json:"temperature,omitempty"`
	Stream       bool              `json:"stream"`
	Tools        []toolWire        `json:"tools,omitempty"`
	Thinking     *thinkingWire     `json:"thinking,omitempty"`
	OutputConfig *outputConfigWire `json:"output_config,omitempty"`
}

// messageWire 是 Anthropic Messages 请求中的单条消息。
type messageWire struct {
	Role    string             `json:"role"` // "user" 或 "assistant"
	Content []contentBlockWire `json:"content"`
}

// contentBlockWire 是 Anthropic 请求消息中的单个内容块。
// Text、Thinking、Input 使用指针区分“字段不存在”与“字段值为空（如空字符串或空 map）”。
type contentBlockWire struct {
	Type string `json:"type"`

	// 文本
	Text *string `json:"text,omitempty"`

	// 图片
	Source *imageSourceWire `json:"source,omitempty"`

	// 思考内容
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`

	// 脱敏思考内容（redacted_thinking）
	Data string `json:"data,omitempty"`

	// 工具调用（tool_use）
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input *map[string]any `json:"input,omitempty"`

	// 工具结果（tool_result）
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Content   any    `json:"content,omitempty"` // string 或 []contentBlockWire
}

// imageSourceWire 是 Anthropic 图片块中的 base64 源描述。
type imageSourceWire struct {
	Type      string `json:"type"`       // 图片源类型固定为 "base64"
	MediaType string `json:"media_type"` // 图片媒体类型，例如 "image/jpeg" 或 "image/png"
	Data      string `json:"data"`       // base64 编码的图片数据
}

// toolWire 是 Anthropic 工具定义。
type toolWire struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// thinkingWire 是 Anthropic 思考参数结构。
type thinkingWire struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// outputConfigWire 是 Anthropic 独立输出参数配置。
type outputConfigWire struct {
	Effort string `json:"effort,omitempty"`
}

// --- 响应侧 -------------------------------------------------------------

// responseEvent 是 Anthropic SSE 事件帧的通用协议事件结构。
type responseEvent struct {
	Type         string                    `json:"type"`
	Index        int                       `json:"index"`
	Message      *responseMessageData      `json:"message,omitempty"`
	ContentBlock *responseContentBlockData `json:"content_block,omitempty"`
	Delta        *responseDeltaData        `json:"delta,omitempty"`
	Usage        *responseUsageData        `json:"usage,omitempty"`
	Error        *responseErrorData        `json:"error,omitempty"`
}

// responseMessageData 是 message_start 携带的消息初始元数据与用量。
type responseMessageData struct {
	ID    string              `json:"id"`
	Type  string              `json:"type"`
	Role  string              `json:"role"`
	Model string              `json:"model"`
	Usage *responseStartUsage `json:"usage"`
}

// responseStartUsage 是 message_start 携带的输入及缓存 token 用量。
type responseStartUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// responseContentBlockData 是 content_block_start 携带的内容块定义。
type responseContentBlockData struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Data     string          `json:"data"`  // redacted_thinking 的不透明数据
	Input    json.RawMessage `json:"input"` // 初始工具参数
}

// responseDeltaData 是 content_block_delta 或 message_delta 携带的增量数据。
type responseDeltaData struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// responseUsageData 是 message_delta 携带的累计输出 token 用量。
type responseUsageData struct {
	OutputTokens int `json:"output_tokens"`
}

// responseErrorData 是服务端返回的错误信息。
type responseErrorData struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
