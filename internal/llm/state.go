package llm

// MessageState 表示消息所处的宏观生命周期状态。
// 它回答「这条消息现在处于什么阶段」，而 StopReason 回答「成功结束时为什么结束」。
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

// StopReason 表示成功结束时「为什么结束」的具体原因。
// 它只在 State == StateSuccess 时有意义：其它状态（pending/error/aborted/deferred）
// 下它保持零值空串，消费者也不应读取它。这是约定，不靠代码强制。
type StopReason string

const (
	// 模型自然结束
	StopStop StopReason = "stop"
	// 达到输出长度上限
	StopLength StopReason = "length"
	// 需要继续执行工具
	StopToolUse StopReason = "tool_use"
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
