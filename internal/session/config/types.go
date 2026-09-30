// Package config 定义 Session 的共享记录、查询参数和状态，不负责读取配置文件。
package config

import (
	"errors"
	"time"

	"CodeAgent/internal/llm"
)

var (
	ErrSessionArchived        = errors.New("session is archived")
	ErrRunActive              = errors.New("run is active")
	ErrRunFinished            = errors.New("run is finished")
	ErrToolResultConflict     = errors.New("tool result conflict")
	ErrToolCallNotFound       = errors.New("tool call not found")
	ErrSessionConflict        = errors.New("session conflict")
	ErrWorkingDirAlreadyBound = errors.New("working directory already bound")
)

// ArchiveFilter 表示会话列表的归档状态筛选选项。
type ArchiveFilter string

const (
	ArchiveFilterActive   ArchiveFilter = "active"   // 默认仅未归档
	ArchiveFilterArchived ArchiveFilter = "archived" // 仅已归档
	ArchiveFilterAll      ArchiveFilter = "all"      // 全部
)

// SessionListOptions 为会话列表查询提供筛选、搜索与分页参数。
// TitleSearch 是字面包含搜索；ASCII 字母忽略大小写，其他字符按 SQLite 默认规则比较。
type SessionListOptions struct {
	ArchiveFilter ArchiveFilter `json:"archive_filter"`
	TitleSearch   string        `json:"title_search"`
	Limit         int           `json:"limit"`
	Offset        int           `json:"offset"`
}

// SessionInfo 是会话元数据，不是 Agent 实例或数据库连接。
// 归档不会删除历史，切换历史分支不会回滚文件或工具副作用。
type SessionInfo struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	WorkingDir string     `json:"working_dir"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"` // nil 表示未归档。
	HeadID     string     `json:"head_id,omitempty"`     // 当前历史位置；空会话没有 head。
}

// RunRecord 保存一次实际开始的用户任务，不重复保存 Entry 中的消息正文。
// ID 必须在会话重新打开后仍保持唯一，不能重新从 run_1 开始编号。
// 重启后由调用方确认任务不再运行，再显式标记 aborted；读取与初始化不自动恢复。
type RunRecord struct {
	ID           string     `json:"id"`
	SessionID    string     `json:"session_id"`
	StartHeadID  string     `json:"start_head_id,omitempty"` // Run 开始前的历史 head；从空历史开始时为空。
	InputEntryID string     `json:"input_entry_id"`          // 本次用户提交的记录。
	LastEntryID  string     `json:"last_entry_id,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"` // nil 表示尚未收尾。
	State        RunState   `json:"state"`
	Steps        int        `json:"steps"` // 已发起的模型调用次数。

	// 当前 Agent 与 LLM 的结束原因值一致，复用已有类型，不再声明同义枚举。
	// success + length 仍表示输出截断；普通错误、取消使用空结束原因。
	StopReason    llm.StopReason `json:"stop_reason"`
	RawStopReason string         `json:"raw_stop_reason,omitempty"`
	ErrorMessage  string         `json:"error_message,omitempty"` // 保存可读说明，不保存 Go error 对象。
}

// RunCompletion 供结束 Run 使用，只包含终态与停止原因说明。
type RunCompletion struct {
	State         RunState       `json:"state"`
	StopReason    llm.StopReason `json:"stop_reason"`
	RawStopReason string         `json:"raw_stop_reason,omitempty"`
	ErrorMessage  string         `json:"error_message,omitempty"`
}

// RunDetail 包含一次 Run 的完整记录、消息、工具执行及用量统计。
type RunDetail struct {
	Record         RunRecord       `json:"record"`
	Entries        []Entry         `json:"entries"`
	ToolExecutions []ToolExecution `json:"tool_executions"`
	UsageByOrigin  []ModelUsage    `json:"usage_by_origin"`
	TotalUsage     llm.Usage       `json:"total_usage"`
}

// Entry 是会话树中一条已提交的原始记录，提交后不原地改写。
// ParentID 决定分支路径；Sequence 只表示会话内的写入顺序，不能据此拼接所有分支。
// Message 使用 LLM 消息类型；持久化编解码负责按角色恢复具体类型。
type Entry struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	ParentID  string    `json:"parent_id,omitempty"` // 根记录为空；非空时必须指向同一会话的已有记录。
	Sequence  int64     `json:"sequence"`            // 会话当中的实际写入顺序
	RunID     string    `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`

	// 保存消息事实，不保存请求准备阶段补出的工具结果或过滤后的消息。
	// 已消费的引导也作为一条 UserMessage 按实际发生顺序追加。
	Message llm.Message `json:"message"`
	// assistant 必须记录实际请求来源；用户消息与工具结果不重复填写。
	Origin *MessageModelOrigin `json:"origin,omitempty"`

	// 仅工具结果使用，与对应 assistant entry 和 tool call 关联。
	AssistantEntryID string `json:"assistant_entry_id,omitempty"`
	ToolCallID       string `json:"tool_call_id,omitempty"`
}

// ToolExecution 记录工具的执行事实，不是完整运行事件流。
// (AssistantEntryID, ToolCallID) 唯一定位一次调用；不能假设 ToolCallID 全局唯一。
// 工具名和参数取自所属 assistant，最终内容及 IsError 取自 ResultEntryID，不重复存储。
// 启动记录先提交，再调用工具；因此它不证明副作用已经发生。
// 完成时结果记录与 FinishedAt/ResultEntryID 应在同一事务内提交。
// 有启动记录而无结果时只能判定结果不明，不能自动重试。
type ToolExecution struct {
	SessionID        string     `json:"session_id"`
	RunID            string     `json:"run_id"`
	AssistantEntryID string     `json:"assistant_entry_id"`
	ToolCallID       string     `json:"tool_call_id"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	ResultEntryID    string     `json:"result_entry_id,omitempty"`
}

// MessageModelOrigin 是产生响应时的请求来源，用于恢复后的思考块同源检查。
// 不保存 API Key、鉴权头或 RequestOptions；BaseURL 不能包含凭据。
// 服务端返回的 ResponseModel/ResponseID 已由 AssistantMessage 保存。
type MessageModelOrigin struct {
	Provider string      `json:"provider"`
	API      llm.APIKind `json:"api"`
	ModelID  string      `json:"model_id"`
	BaseURL  string      `json:"base_url"`
}

// ModelUsage 按实际请求来源归类，避免不同 Provider 的同名模型混在一起。
// 总用量可由各项 Usage 相加得到；费用沿用调用当时的计算结果。
type ModelUsage struct {
	Origin MessageModelOrigin `json:"origin"`
	Usage  llm.Usage          `json:"usage"`
}

// RunState 是持久化 Run 的执行结果，与 Agent 的状态值对齐。
// Session 不依赖 agent 包；接入层负责转换，不保存 Handle 或 context。
type RunState string

const (
	// 只记录已经开始的 Run，pending 表示尚未结束，不表示仍在输入队列中。
	RunStatePending  RunState = "pending"
	RunStateSuccess  RunState = "success"
	RunStateError    RunState = "error"
	RunStateAborted  RunState = "aborted"
	RunStateDeferred RunState = "deferred" // 与现有 Agent 对齐，本阶段不主动产生。
)
