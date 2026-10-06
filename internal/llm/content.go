package llm

// ContentType 判别内容块的具体类型。
type ContentType string

const (
	TypeText     ContentType = "text"
	TypeThinking ContentType = "thinking"
	TypeImage    ContentType = "image"
	TypeToolCall ContentType = "tool_call"
)

// ContentBlock 是内容块判别联合的扁平表示：Type 决定哪些字段有效，
// 其余字段保持零值。用 switch block.Type 区分具体类型。
type ContentBlock struct {
	Type ContentType

	// 文本
	Text          string
	TextSignature string // 签名

	// 思考内容
	Thinking          string
	ThinkingSignature string
	Redacted          bool   // 是否被 Provider 隐去
	RedactedData      string // 脱敏思考内容的不透明数据，不与图片 Data 混用

	// 图片
	Data     string // base64 编码的图片数据
	MimeType string

	// 工具调用
	ID               string
	Name             string
	Arguments        map[string]any // 工具参数
	ThoughtSignature string
	Namespace        string // 区分工具命名空间，用于扩展
}
