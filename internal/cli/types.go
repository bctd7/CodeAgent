// Package cli 提供独立命令适配层；真实入口、信号注册和 TUI 装配由调用方负责。
package cli

import (
	"context"
	"io"

	"CodeAgent/internal/agent"
	modelconfig "CodeAgent/internal/config"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/runtime"
	runtimeconfig "CodeAgent/internal/runtime/config"
	sessionconfig "CodeAgent/internal/session/config"
)

const (
	ExitOK        = 0
	ExitFailure   = 1
	ExitUsage     = 2
	ExitCancelled = 130
)

// Backend 与 Runtime 保留现有业务能力；接口返回值仅为便于注入而抽象。
type Backend interface {
	CreateSession(context.Context, runtimeconfig.CreateSessionOptions) (sessionconfig.SessionInfo, error)
	ListSessions(context.Context, sessionconfig.SessionListOptions) ([]sessionconfig.SessionInfo, error)
	OpenSession(context.Context, string, runtimeconfig.OpenSessionOptions) (Runtime, error)
	RenameSession(context.Context, string, string) error
	ArchiveSession(context.Context, string) error
	UnarchiveSession(context.Context, string) error
	CloseSession(context.Context, string) error
	Close(context.Context) error
}

type Runtime interface {
	SessionID() string
	Submit(context.Context, *llm.UserMessage) (RunHandle, error)
	Steer(string) error
	Snapshot() runtimeconfig.Snapshot
	SetModel(context.Context, sessionconfig.ModelSelection) error
	BindWorkingDir(context.Context, string) error
	GetCurrentHistory(context.Context) ([]sessionconfig.Entry, error)
	GetRun(context.Context, string) (sessionconfig.RunDetail, error)
	ListRuns(context.Context, int, int) ([]sessionconfig.RunRecord, error)
}

type RunHandle interface {
	ID() string
	Cancel()
	Wait(context.Context) (agent.RunResult, error)
}

// AdaptHost 仅适配 Go 不支持返回值协变的两个方法，其余方法直接提升，不实现业务。
func AdaptHost(host *runtime.Host) Backend {
	if host == nil {
		return nil
	}
	return hostAdapter{host}
}

type hostAdapter struct{ *runtime.Host }

func (h hostAdapter) OpenSession(ctx context.Context, id string, opts runtimeconfig.OpenSessionOptions) (Runtime, error) {
	r, err := h.Host.OpenSession(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	return runtimeAdapter{r}, nil
}

type runtimeAdapter struct{ *runtime.Runtime }

func (r runtimeAdapter) Submit(ctx context.Context, message *llm.UserMessage) (RunHandle, error) {
	return r.Runtime.Submit(ctx, message)
}

// ModelInfo 只保存可显示字段，不保存地址、凭据、请求头或传输选项。
type ModelInfo struct {
	Ref           string      `json:"ref"`
	Provider      string      `json:"provider"`
	ID            string      `json:"id"`
	DisplayName   string      `json:"display_name"`
	API           llm.APIKind `json:"api"`
	Reasoning     bool        `json:"reasoning"`
	ContextWindow int         `json:"context_window"`
	MaxTokens     int         `json:"max_tokens"`
}

func ProjectModels(entries []modelconfig.ModelEntry) []ModelInfo {
	models := make([]ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(models, projectModel(entry.Ref, entry.Model))
	}
	return models
}

// 模型目录与状态展示共用这一份允许字段，避免输出边界不一致。
func projectModel(ref string, m llm.Model) ModelInfo {
	return ModelInfo{Ref: ref, Provider: m.Provider, ID: m.ID,
		DisplayName: m.DisplayName, API: m.API, Reasoning: m.Reasoning,
		ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens}
}

// StatusInfo 只用于状态展示；历史通过 /history 单独获取。
// ErrorMessage 只给固定故障说明，不复制可能包含内部信息的原始错误。
type StatusInfo struct {
	SessionID    string              `json:"session_id"`
	State        runtimeconfig.State `json:"state"`
	ErrorMessage string              `json:"error_message,omitempty"`
	ModelRef     string              `json:"model_ref"`
	WorkingDir   string              `json:"working_dir"`
	AgentStatus  agent.AgentStatus   `json:"agent_status"`
	CurrentRunID string              `json:"current_run_id,omitempty"`
	QueueLength  int                 `json:"queue_length"`
	QueuedRunIDs []string            `json:"queued_run_ids"`
	HistoryCount int                 `json:"history_count"`
	CurrentModel ModelInfo           `json:"current_model"`
	PendingModel *ModelInfo          `json:"pending_model,omitempty"`
}

func projectStatus(snapshot runtimeconfig.Snapshot) StatusInfo {
	a := snapshot.Agent
	status := StatusInfo{SessionID: snapshot.SessionID, State: snapshot.State,
		ModelRef: snapshot.Model.Ref, WorkingDir: snapshot.WorkingDir, AgentStatus: a.Status,
		CurrentRunID: a.CurrentRunID, QueueLength: a.QueueLength,
		QueuedRunIDs: append([]string{}, a.QueuedRunIDs...), HistoryCount: a.HistoryCount,
		CurrentModel: projectModel("", a.CurrentModel)}
	if snapshot.ErrorMessage != "" || snapshot.State == runtimeconfig.StateFaulted {
		status.ErrorMessage = "运行时发生故障"
	}
	if a.PendingModel != nil {
		model := projectModel("", *a.PendingModel)
		status.PendingModel = &model
	}
	return status
}

type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// ChatState 由未来 TUI 持有。CurrentHandle 只能填写当前任务的已持有句柄，不能按任意 ID 远程取消。
type ChatState struct {
	Runtime       Runtime
	CurrentHandle RunHandle
}

// TUI 接收初始选择与命令适配层；退出时返回最后状态，供输入结束或中断时收尾。
// TUI 负责显示和输入，不得向单次 run 的 stdout 写入日志。
type TUI interface {
	Run(context.Context, *CLI, ChatState, IO) (ChatState, error)
}

type Config struct {
	Backend Backend
	Models  []ModelInfo
	TUI     TUI
	Version string
}

type Invocation struct {
	Action      string
	SessionID   string
	ModelRef    string
	WorkingDir  string
	Title       string
	Task        string
	ListOptions sessionconfig.SessionListOptions
	JSON        bool
}

type Intent string

const (
	ShowResult    Intent = "show_result"
	EnterSession  Intent = "enter_session"
	SelectSession Intent = "select_session"
	TaskSubmitted Intent = "task_submitted"
	Quit          Intent = "quit"
)

// CommandResult 是交互结果，不直接打印聊天 UI。执行对象不参与 JSON 序列化。
type CommandResult struct {
	Intent   Intent                      `json:"intent"`
	Runtime  Runtime                     `json:"-"`
	Handle   RunHandle                   `json:"-"`
	Session  *sessionconfig.SessionInfo  `json:"session,omitempty"`
	Sessions []sessionconfig.SessionInfo `json:"sessions,omitempty"`
	Models   []ModelInfo                 `json:"models,omitempty"`
	Snapshot *StatusInfo                 `json:"snapshot,omitempty"`
	History  []sessionconfig.Entry       `json:"history,omitempty"`
	Runs     []sessionconfig.RunRecord   `json:"runs,omitempty"`
	Run      *sessionconfig.RunDetail    `json:"run,omitempty"`
	Usage    *RunUsage                   `json:"usage,omitempty"`
	Help     string                      `json:"help,omitempty"`
}

type RunUsage struct {
	RunID         string                     `json:"run_id"`
	UsageByOrigin []sessionconfig.ModelUsage `json:"usage_by_origin"`
	TotalUsage    llm.Usage                  `json:"total_usage"`
}

// RunOutput 是单次 run 的 JSON 契约，不序列化 Go error、模型配置或完整会话历史。
type RunOutput struct {
	SessionID    string              `json:"session_id,omitempty"`
	RunID        string              `json:"run_id,omitempty"`
	State        agent.RunState      `json:"state"`
	Text         string              `json:"text"`
	Steps        int                 `json:"steps"`
	TotalUsage   llm.Usage           `json:"total_usage"`
	StopReason   agent.RunStopReason `json:"stop_reason,omitempty"`
	ErrorMessage string              `json:"error,omitempty"`
}
