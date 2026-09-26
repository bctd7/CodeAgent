package llm

// APIKind 表示模型使用的线协议族，而不是具体厂商。
type APIKind string

const (
	APIKindOpenAI    APIKind = "openai-completions"
	APIKindAnthropic APIKind = "anthropic"
)

// Pricing 的单位是“每一百万 token 的价格”。
type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// OpenAIOptions 是 OpenAI 协议专属的请求参数。
type OpenAIOptions struct {
	ReasoningEffort string `toml:"reasoning_effort" json:"reasoning_effort,omitempty"`
	MaxTokensField  string `toml:"max_tokens_field" json:"max_tokens_field,omitempty"`
}

// AnthropicOptions 是 Anthropic 协议专属的思考与请求参数。
type AnthropicOptions struct {
	Thinking     string `toml:"thinking" json:"thinking,omitempty"`
	BudgetTokens int    `toml:"budget_tokens" json:"budget_tokens,omitempty"`
	Effort       string `toml:"effort" json:"effort,omitempty"`
}

// Model 是一次请求使用的显式模型配置快照。
// 它不持有连接、API key 或 goroutine：切换模型就是整体
// 替换一个新的 Model 值，而不是在共享对象上原地改字段。
type Model struct {
	Provider      string
	ID            string
	DisplayName   string
	API           APIKind
	BaseURL       string
	Reasoning     bool
	ContextWindow int
	MaxTokens     int
	Pricing       Pricing
}
