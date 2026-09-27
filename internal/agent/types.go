package agent

import (
	"context"

	"CodeAgent/internal/llm"
)

// StreamFunc 是调用底层大模型流式调度的函数契约，与 Provider 层兼容。
type StreamFunc func(context.Context, llm.Request) (llm.StreamReader, error)

// AgentStatus 表示 Agent 实例（对话窗口）整体的生命周期状态。
type AgentStatus string

const (
	AgentStatusIdle       AgentStatus = "idle"
	AgentStatusRunning    AgentStatus = "running"
	AgentStatusCancelling AgentStatus = "cancelling"
	// 实例已关闭，不再接受任务；对话归档时由外层关闭实例。
	AgentStatusClosed AgentStatus = "closed"
)

// RunState 表示单次任务的状态。
type RunState string

const (
	RunStatePending RunState = "pending"
	// 正常结束
	RunStateSuccess RunState = "success"
	// 任务执行失败，包括模型、Provider 或工具内部故障。
	RunStateError RunState = "error"
	// 用户中断或者超时
	RunStateAborted RunState = "aborted"
	// 延迟响应（本阶段暂不生成）
	RunStateDeferred RunState = "deferred"
)

// RunStopReason 表示单次任务的结束原因，与 LLM 层相同原因使用一致的值。
// State 表示执行结果，StopReason 提供结构化原因，ErrorMessage 提供可读说明。
type RunStopReason string

const (
	RunStopCompleted RunStopReason = "stop"
	/*
		RunStopStepLimit RunStopReason = "step_limit"
	*/
	RunStopTruncated RunStopReason = "length"
	// 服务端明确报告内容过滤或拒绝；对应 StateError，具体说明见 ErrorMessage。
	RunStopSafety RunStopReason = "safety"
	// 与 LLM 工具调用原因对齐；当前 Agent 自动执行工具，不以此结束 Run。
	RunStopToolUse RunStopReason = "tool_use"
)

// RunResult 保存单次任务（Run）的完整执行结果。
type RunResult struct {
	RunID      string               `json:"run_id"`
	State      RunState             `json:"state"`
	Steps      int                  `json:"steps"`
	Messages   []llm.Message        `json:"messages"`
	Usage      map[string]llm.Usage `json:"usage"` // 按模型 ID 汇总的 Token 用量
	TotalUsage llm.Usage            `json:"total_usage"`
	StopReason RunStopReason        `json:"stop_reason"`
	// 导致任务结束的服务端原始原因（如 content_filter），用于诊断，不是错误说明。
	// 用户取消或本地故障没有对应的服务端结束原因时，留空。
	RawStopReason string `json:"raw_stop_reason,omitempty"`
	// 面向用户的失败或中断说明；前端直接展示，不必拼接 RawStopReason 或 Error。
	ErrorMessage string `json:"error_message,omitempty"`
	// 可选的底层 Go 错误对象，供日志或 errors.Is/As 使用，不作为前端展示依据。
	// nil 不代表任务成功，执行结果以 State 为准。暂时保留，后续无使用需求时可删除。
	Error error `json:"error,omitempty"`
}

// Snapshot 是 Agent 当前内部状态的只读快照。
type Snapshot struct {
	Status       AgentStatus   `json:"status"`
	CurrentRunID string        `json:"current_run_id,omitempty"`
	CurrentModel llm.Model     `json:"current_model"`
	PendingModel *llm.Model    `json:"pending_model,omitempty"`
	QueueLength  int           `json:"queue_length"`
	QueuedRunIDs []string      `json:"queued_run_ids"`
	HistoryCount int           `json:"history_count"`
	History      []llm.Message `json:"history"`
}

// Config 包含创建 Agent 所需的配置与依赖。
type Config struct {
	Model          llm.Model
	Options        llm.RequestOptions
	SystemPrompt   string
	InitialHistory []llm.Message
	Stream         StreamFunc
	Tools          []Tool
	OnEvent        func(Event)
	/*
		MaxSteps int // 当前不启用调用次数上限
	*/
	MaxParallelTools int // 默认 4，负数拒绝
}
