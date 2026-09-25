package llm

// Event 是事件，是模型完成任务的过程，最后汇总成为 AssistantMessage。
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	EventToolCallStart EventType = "toolcall_start"
	EventToolCallDelta EventType = "toolcall_delta"
	EventToolCallEnd   EventType = "toolcall_end"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

type Event struct {
	Type EventType `json:"type"`
	// 块的索引
	ContentIndex int
	// 新增的负载内容
	Delta string
	// 当前累积快照
	Partial *AssistantMessage
	// 终态全量数据
	Final *AssistantMessage
}
