// Package hooks 执行调用方明确授权的 Claude Code 命令钩子子集。
// 会话存储、工具执行、权限应用和 Agent 生命周期由调用方负责。
package hooks

import (
	"encoding/json"
	"errors"
)

// Event 表示本包支持的命令钩子事件。
type Event string

const (
	SessionStart       Event = "SessionStart"
	UserPromptSubmit   Event = "UserPromptSubmit"
	PreToolUse         Event = "PreToolUse"
	PostToolUse        Event = "PostToolUse"
	PostToolUseFailure Event = "PostToolUseFailure"
	PostToolBatch      Event = "PostToolBatch"
	Stop               Event = "Stop"
	SessionEnd         Event = "SessionEnd"
)

var (
	ErrConfiguration = errors.New("invalid hook configuration")
	ErrInput         = errors.New("invalid hook input")
	ErrProtocol      = errors.New("invalid hook output")
	ErrUnsupported   = errors.New("unsupported hook capability")
	ErrOutputLimit   = errors.New("hook output limit exceeded")
)

// 公共与辅助类型：共享字段、权限和执行通知。

// CommonInput 及嵌套事件数据在调用期间按只读约定使用。
// 没有真实会话记录时省略 TranscriptPath，本包不伪造记录路径。
type CommonInput struct {
	// ProjectDir 是用于 CLAUDE_PROJECT_DIR 的可选宿主元数据，
	// 不额外发送到协议中；未提供时使用真实工作目录 CWD。
	ProjectDir string `json:"-"`
	SessionID  string `json:"session_id"`
	CWD        string `json:"cwd"`
	// TranscriptPath 由宿主提供，只有存在真实记录时才填写。
	TranscriptPath string `json:"transcript_path,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	PromptID       string `json:"prompt_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	AgentType      string `json:"agent_type,omitempty"`
	ScratchpadDir  string `json:"scratchpad_dir,omitempty"`
}

// ToolInput 携带工具名称、调用标识及调用方提供的 JSON 输入。
type ToolInput struct {
	// IsMCP 只能由可信宿主设置，不根据模型可见名称推断。
	IsMCP     bool            `json:"-"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
	MCPServer json.RawMessage `json:"mcp_server,omitempty"`
}

// ToolStatus 表示宿主记录的工具执行状态。
type ToolStatus string

const (
	ToolSucceeded   ToolStatus = "succeeded"
	ToolFailed      ToolStatus = "failed"
	ToolDenied      ToolStatus = "denied"
	ToolCanceled    ToolStatus = "canceled"
	ToolUnavailable ToolStatus = "unavailable"
)

// BatchTool 的 ToolResponse 保存最终面向模型的内容，与 PostToolUse 的结构化输出不同。
// Status 仅供宿主使用，不发送到协议中。
type BatchTool struct {
	ToolInput
	ToolResponse json.RawMessage `json:"tool_response"`
	Status       ToolStatus      `json:"-"`
}

// Permission 表示工具执行前的权限决策；空值表示没有决策。
type Permission string

const (
	NoPermissionDecision Permission = ""
	Allow                Permission = "allow"
	Deny                 Permission = "deny"
	Ask                  Permission = "ask"
	Defer                Permission = "defer"
)

// Report 保存单个钩子的进程事实及输出解释问题。
type Report struct {
	Source, HookID         string
	Event                  Event
	Command, StatusMessage string
	Execution              Execution
	// Error 保存 interpret 返回的问题，不重复合并 Execution.Error。
	// 非 nil 不一定撤销有效输出；主动拒绝由事件结果表达。
	Error error
}

// Notices 按配置顺序返回，与进程完成顺序无关。
// 命令失败作为诊断事实保留，不自动转换为权限决策。
type Notices struct {
	Reports        []Report
	SystemMessages []string
}

// Control 表示停止处理请求及其原因，实际停止由宿主应用。
type Control struct {
	StopProcessing bool
	StopReasons    []string
}

// 事件输入：以下八个 Input 按 Event 常量顺序排列。

type SessionStartInput struct {
	CommonInput
	Source       string `json:"source"`
	Model        string `json:"model,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
}

type UserPromptSubmitInput struct {
	CommonInput
	Prompt       string `json:"prompt"`
	SessionTitle string `json:"session_title,omitempty"`
}

type PreToolUseInput struct {
	CommonInput
	ToolInput
}

type PostToolUseInput struct {
	CommonInput
	ToolInput
	ToolResponse json.RawMessage `json:"tool_response"`
	DurationMS   *int64          `json:"duration_ms,omitempty"`
}

type PostToolUseFailureInput struct {
	CommonInput
	ToolInput
	Error       string `json:"error"`
	IsInterrupt bool   `json:"is_interrupt"`
	DurationMS  *int64 `json:"duration_ms,omitempty"`
}

type PostToolBatchInput struct {
	CommonInput
	ToolCalls []BatchTool `json:"tool_calls"`
}

type StopInput struct {
	CommonInput
	StopHookActive       bool            `json:"stop_hook_active"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	BackgroundTasks      json.RawMessage `json:"background_tasks,omitempty"`
	SessionCrons         json.RawMessage `json:"session_crons,omitempty"`
}

type SessionEndInput struct {
	CommonInput
	Reason string `json:"reason"`
}

// 事件结果：以下八个 Result 与事件输入保持相同顺序。

type SessionStartResult struct {
	Notices
	Control
	AdditionalContext []string
}

type UserPromptSubmitResult struct {
	Notices
	Control
	Rejected                   bool
	Reasons, AdditionalContext []string
}

type PreToolUseResult struct {
	Notices
	Control
	Permission Permission
	Reasons    []string
	// UpdatedInput 为 nil 表示没有改写；宿主负责应用非 nil 的改写。
	UpdatedInput      *json.RawMessage
	AdditionalContext []string
}

type PostToolUseResult struct {
	Notices
	Control
	Feedback, AdditionalContext []string
	// UpdatedToolOutput 为 nil 表示保留原始工具输出。
	UpdatedToolOutput *json.RawMessage
}

type PostToolUseFailureResult struct {
	Notices
	Control
	Feedback, AdditionalContext []string
}

type PostToolBatchResult struct {
	Notices
	Control
	StopLoop                   bool
	Reasons, AdditionalContext []string
}

type StopResult struct {
	Notices
	Control
	ContinueTurn               bool
	Reasons, AdditionalContext []string
}

// SessionEndResult 仅返回通知，退出码为 2 时也不产生控制决策。
type SessionEndResult struct{ Notices }
