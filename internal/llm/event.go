package llm

// EventType 区分消息生命周期通知和内容块更新通知。
type EventType string

const (
	EventMessageStart       EventType = "start"
	EventTextBlockStart     EventType = "text_start"
	EventTextDelta          EventType = "text_delta"
	EventTextBlockEnd       EventType = "text_end"
	EventThinkingBlockStart EventType = "thinking_start"
	EventThinkingDelta      EventType = "thinking_delta"
	EventThinkingBlockEnd   EventType = "thinking_end"
	EventToolCallBlockStart EventType = "toolcall_start"
	EventToolCallDelta      EventType = "toolcall_delta"
	EventToolCallBlockEnd   EventType = "toolcall_end"
	EventMessageComplete    EventType = "done"
	EventMessageError       EventType = "error"
)

// Event 是 Provider 发给消费者的内部通知，不是服务端的 SSE 帧。
// Message 包含 ContentBlock；Delta 是本次增量，Snapshot 是更新后的完整消息快照。
type Event struct {
	Type EventType `json:"type"`
	// 内容块在 Message.Content 中的位置；仅内容块事件使用。
	BlockIndex int
	// 本次新增的文本、思考或工具参数片段；仅 Delta 事件使用。
	Delta string
	// 非终止事件携带的当前完整消息快照，只读。
	Snapshot *AssistantMessage
	// 消息完成、失败或取消时的最终消息，只读。
	FinalMessage *AssistantMessage
}
