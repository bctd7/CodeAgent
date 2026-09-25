package llm

// APIKind 表示模型使用的线协议族，而不是具体厂商。
type APIKind string
type InputKind string

const APIKindOpenAI APIKind = "openai-completions"
const APIKindAnthropic APIKind = "anthropic"

const (
	InputText  InputKind = "text"
	InputImage InputKind = "image"
)

// Pricing 的单位是“每一百万 token 的价格”。
type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
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
	Input         []InputKind
	ContextWindow int
	MaxTokens     int
	Pricing       Pricing
}
