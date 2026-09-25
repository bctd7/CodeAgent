package llm

import (
	"encoding/json"
	"time"
)

// Role 是消息在对话中的身份。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是统一消息的判别联合。
// 未导出的 message 方法把可实现该接口的类型封闭在本包内（sealed），
// 调用方用类型断言区分 User / Assistant / ToolResult 三种消息。
type Message interface {
	message()
}

func (*UserMessage) message()       {}
func (*AssistantMessage) message()  {}
func (*ToolResultMessage) message() {}

// UserMessage 只允许 Text 和 Image 内容块。
type UserMessage struct {
	Role      Role
	Content   []ContentBlock
	Timestamp time.Time
}

// AssistantMessage 是模型一次调用在任意时刻的快照。
type AssistantMessage struct {
	Role          Role
	Content       []ContentBlock
	Provider      string
	API           APIKind
	Model         string
	ResponseModel string
	ResponseID    string
	Usage         Usage
	State         MessageState
	StopReason    StopReason
	//未加工过的错误原因
	RawStopReason string
	ErrorMessage  string
	Timestamp     time.Time
}

// ToolResultMessage 用 ToolCallID 与 Assistant 发出的调用精确配对。
type ToolResultMessage struct {
	Role       Role
	ToolCallID string
	ToolName   string
	Content    []ContentBlock
	Details    json.RawMessage
	Usage      *Usage
	IsError    bool
	Timestamp  time.Time
}
