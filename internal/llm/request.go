package llm

// Tool 是模型可见的工具定义。
type Tool struct {
	Name        string
	Description string
	Schema      []byte
}

// RequestOptions 是一次请求的传输级选项。
type RequestOptions struct {
	APIKey      string
	Headers     map[string]string
	Temperature *float64
	MaxTokens   int
	OpenAI      *OpenAIOptions
	Anthropic   *AnthropicOptions
}

// Request 是一次模型调用的完整输入。
// 调用方在流结束前必须将 Request 及其所有子字段保持为只读。
type Request struct {
	Model        Model
	SystemPrompt string
	Messages     []Message
	Tools        []Tool
	Options      RequestOptions
}
