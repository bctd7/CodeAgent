package chat

import "encoding/json"

// chatRequest 是发送给模型的请求体（Chat Completions 协议）。
type chatRequest struct {
	Model               string        `json:"model"`
	Stream              bool          `json:"stream"`
	StreamOptions       streamOptions `json:"stream_options"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
}

// streamOptions 是流式请求的附加选项。
type streamOptions struct {
	// IncludeUsage 让最后一个 chunk 返回本次请求的 token 用量。
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 是对话历史中的一条消息。
// role 是消息作者（system/developer/user/assistant/tool）。
// content 是正文，可以是纯文本字符串，也可以是多模态内容块数组（转换逻辑在 request.go）。
type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

// contentPart 是多模态 content 数组里的一个元素。
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

// imageURL 是图片内容的 data URL。
type imageURL struct {
	URL string `json:"url"`
}

// chatTool 是提供给模型的工具定义。
type chatTool struct {
	Type     string       `json:"type"`
	Function functionTool `json:"function"`
}

// functionTool 是函数工具的具体定义。
type functionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

// chatToolCall 是 assistant 消息里发起的工具调用（作为历史回放）。
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function functionCallWire `json:"function"`
}

// functionCallWire 是工具调用的函数名和参数。
type functionCallWire struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// 响应侧

// chatCompletionChunk 是流式响应的一个 chunk（object 为 chat.completion.chunk）。
type chatCompletionChunk struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	SystemFingerprint string         `json:"system_fingerprint"`
	Choices           []choice       `json:"choices"`
	Obfuscation       string         `json:"obfuscation"`
	Usage             *chunkUsage    `json:"usage,omitempty"`
	Error             *providerError `json:"error,omitempty"`
}

// choice 是流式 chunk 中的一个选择（delta + 结束原因）。
type choice struct {
	Index        int           `json:"index"`
	Delta        responseDelta `json:"delta"`
	FinishReason string        `json:"finish_reason"` // 缺失、null 和空字符串均表示尚无结束原因。
}

// responseDelta 是单个候选答案的增量；缺失或 null 时保留零值。
type responseDelta struct {
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []toolCallDelta `json:"tool_calls"`
}

type toolCallDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id"`
	Function functionCallWire `json:"function"`
}

// chunkUsage 是 token 用量（最后一块才有，需 stream_options.include_usage）。
type chunkUsage struct {
	PromptTokens      int64                  `json:"prompt_tokens"`
	CompletionTokens  int64                  `json:"completion_tokens"`
	PromptDetails     promptTokenDetails     `json:"prompt_tokens_details"`
	CompletionDetails completionTokenDetails `json:"completion_tokens_details"`
}

// promptTokenDetails 是输入 token 的明细。
type promptTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// completionTokenDetails 是输出 token 的明细。
type completionTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// providerError 是 Provider 返回的错误（流中途出错时才有，防御性字段）。
type providerError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}
