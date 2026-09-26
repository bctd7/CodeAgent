package llm

// MessageState 表示消息生命周期及调用结果。
type MessageState string

const (
	// 仍然在生成
	StatePending MessageState = "pending"
	// 正常结束
	StateSuccess MessageState = "success"
	// 模型或 Provider 失败
	StateError MessageState = "error"
	// 用户中断或者超时
	StateAborted MessageState = "aborted"
	// 延迟响应（本阶段暂不生成）
	StateDeferred MessageState = "deferred"
)

// StopReason 表示模型停止生成的原因。
// - 正常成功使用 StopStop、StopLength、StopToolUse；
// - 安全拦截使用 StateError + StopSafety；
// - 普通错误、取消、内部契约错误仍使用空 StopReason。
type StopReason string

const (
	// 模型自然结束
	StopStop StopReason = "stop"
	// 达到输出长度上限
	StopLength StopReason = "length"
	// 需要继续执行工具
	StopToolUse StopReason = "tool_use"
	// 内容被安全过滤器拦截
	StopSafety StopReason = "safety"
)

// IsTerminal 表示消息处于已定义的终态（success / error / aborted / deferred）。
// 空值或 pending 均不是终态。
func (state MessageState) IsTerminal() bool {
	switch state {
	case StateSuccess, StateError, StateAborted, StateDeferred:
		return true
	default:
		return false
	}
}

// IsSuccessful 表示消息成功结束。
func (state MessageState) IsSuccessful() bool {
	return state == StateSuccess
}

// IsFailed 表示消息因错误或取消而结束。
func (state MessageState) IsFailed() bool {
	return state == StateError || state == StateAborted
}
