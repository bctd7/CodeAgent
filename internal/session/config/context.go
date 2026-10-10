package config

import (
	"CodeAgent/internal/llm"
	"time"
)

type EntryKind string

const (
	MessageEntry EntryKind = "message"
	ContextEntry EntryKind = "context"
)

type ProcessingState string

const (
	ProcessingPending  ProcessingState = "pending"
	ProcessingOriginal ProcessingState = "original"
	ProcessingReplaced ProcessingState = "replaced"
)

// ContextInput 是宿主提供的模型上下文；SourceID 是该来源的一次事件标识。
// Message 只允许 user 形状以承载上下文，Entry.Kind 明确它不是用户输入。
type ContextInput struct {
	SourceKind string
	SourceID   string
	Message    *llm.UserMessage
}

// ContextRecord 保留稳定 EntryID 和来源；Pending 时 Message 必须为 nil。
type ContextRecord struct {
	EntryID              string
	Kind                 EntryKind
	SourceKind, SourceID string
	Processing           ProcessingState
	Message              llm.Message
	Origin               *MessageModelOrigin
}

type ToolStatus string

const (
	ToolStarted     ToolStatus = "started"
	ToolSucceeded   ToolStatus = "succeeded"
	ToolFailed      ToolStatus = "failed"
	ToolCanceled    ToolStatus = "canceled"
	ToolDenied      ToolStatus = "denied"
	ToolUnavailable ToolStatus = "unavailable"
	ToolSkipped     ToolStatus = "skipped"
	ToolDeferred    ToolStatus = "deferred"
)

// ToolResultInput 显式记录执行事实，不从 IsError 猜测是否执行。
type ToolResultInput struct {
	Result       *llm.ToolResultMessage
	Arguments    map[string]any
	Status       ToolStatus
	Executed     bool
	ErrorMessage string
}

// CompactionInput.SourceIDs 是本次压缩使用的有序 Entry 引用，不包含已经被 PreviousID 覆盖的前缀。
// FirstKeptIndex 对应本次 SourceIDs；Session 不重新切分、不生成摘要。
type CompactionInput struct {
	HeadID         string
	PreviousID     string
	SourceIDs      []string
	FirstKeptIndex int
	Summary        string
	Origin         MessageModelOrigin
	Usage          llm.Usage
}
type Compaction struct {
	ID        string
	SessionID string
	CompactionInput
	CreatedAt time.Time
}

type ChildTaskInput struct {
	ID             string
	ParentRunID    string
	ChildSessionID string
	Role           string
	FileScope      []string
}
type ChildTask struct {
	ChildTaskInput
	ParentSessionID string
	CreatedAt       time.Time
}

// MemoryProgress 持久边界可能位于其他分支；NextMemoryBatch 根据共同祖先计算有效覆盖。
type MemoryProgress struct {
	SessionID, ScopeKey, CoveredEntryID string
	UpdatedAt                           time.Time
}

// MemoryBatch 固定到一个已正常结束的主 Run。Entries 只包含原始消息，不包含宿主上下文。
// ExpectedEntryID 用于推进时比较，HeadID 固定本次查询使用的历史路径。
type MemoryBatch struct {
	ID, SessionID, ScopeKey, RunID, HeadID, EndEntryID, ExpectedEntryID string
	Entries                                                             []Entry
}
