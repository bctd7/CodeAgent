// Package tui 提供独立终端展示层，只消费脱敏投影并提交操作意图。
// 命令业务、会话与 Run 生命周期、审批权限均由调用方负责。
package tui

import (
	"context"
	"io"
)

type ItemKind string

const (
	UserItem      ItemKind = "user"
	AssistantItem ItemKind = "assistant"
	NoticeItem    ItemKind = "notice"
	ToolItem      ItemKind = "tool"
)

// Tool 仅接收已脱敏的展示文本，Diff 是上层已有文本，不在 UI 中计算。
type Tool struct{ Name, State, Summary, Parameters, Output, Diff string }
type Item struct {
	ID   string
	Kind ItemKind
	Text string
	Tool *Tool
}
type Status struct {
	SessionID, SessionTitle, Model, ModelName, RunID, State, Detail string
	Running                                                         bool
}
type Snapshot struct {
	Status  Status
	Items   []Item
	Notices []Notice
}
type Notice struct{ ID, Text string }
type Choice struct{ ID, Label, Detail string }
type Command struct{ Name, Description string }
type Approval struct{ ID, Source, Tool, Parameters, Diff string }
type ApprovalDecision string

const (
	ApproveOnce        ApprovalDecision = "once"
	ApproveSessionTool ApprovalDecision = "session-tool"
	Reject             ApprovalDecision = "reject"
)

type ActionKind string

const (
	Send           ActionKind = "send"
	Steer          ActionKind = "steer"
	ExecuteCommand ActionKind = "command"
	CancelRun      ActionKind = "cancel-run"
	Quit           ActionKind = "quit"
	ListSessions   ActionKind = "list-sessions"
	ListModels     ActionKind = "list-models"
	SelectSession  ActionKind = "select-session"
	SelectModel    ActionKind = "select-model"
	AnswerApproval ActionKind = "approval"
	DismissNotice  ActionKind = "dismiss-notice"
)

type Action struct {
	Kind                                                   ActionKind
	SessionID, RunID, Text, ChoiceID, ApprovalID, NoticeID string
	Decision                                               ApprovalDecision
}

// Handler 在 Tea 命令中异步执行，须遵守 context。取消/退出可与其他操作并发；
// 调用方将其路由至持有的当前句柄。成功仅表示操作受理，Run 状态以事件为准。
// 命令原文交给 CLI 共享斜杠命令注册表，此包不解析命令业务或保存授权。
// 错误及返回事件同样必须先脱敏，不允许传入密钥、请求头或原始配置对象。
type Handler func(context.Context, Action) ([]Event, error)
type Config struct {
	Initial  Snapshot
	Commands []Command
	Handle   Handler
	// Events 由调用方拥有并关闭。发送后禁止修改投影；关闭只停止订阅。
	Events <-chan Event
}
type IO struct {
	In  io.Reader
	Out io.Writer
}

type Event interface{ tuiEvent() }

// Scope 隔离切换会话后的迟到事件。RunID 为空表示会话级事件。
type Scope struct{ SessionID, RunID string }
type SnapshotEvent struct{ Snapshot Snapshot }
type ItemEvent struct {
	Scope
	Item Item
}
type DeltaEvent struct {
	Scope
	ItemID, Text string
}
type StatusEvent struct {
	Scope
	Status Status
}

// 选项查询结果应通过 Handler 返回以关联此次请求；直接注入表示主动打开弹层。
type ChoicesEvent struct {
	Scope
	Kind    ActionKind
	Choices []Choice
}
type ApprovalEvent struct {
	Scope
	Approval Approval
}
type ApprovalResolvedEvent struct {
	Scope
	ID string
}
type NoticeEvent struct {
	Scope
	ID, Text   string
	Persistent bool
}
type NoticeDismissedEvent struct{ ID string }
type PanelEvent struct {
	Scope
	Title, Text string
}

func (NoticeDismissedEvent) tuiEvent() {}
func (PanelEvent) tuiEvent()           {}

// ExitEvent 由后端处理退出意图后发出；UI 不取消任务、不关闭 Host。
type ExitEvent struct{}

func (SnapshotEvent) tuiEvent()         {}
func (ItemEvent) tuiEvent()             {}
func (DeltaEvent) tuiEvent()            {}
func (StatusEvent) tuiEvent()           {}
func (ChoicesEvent) tuiEvent()          {}
func (ApprovalEvent) tuiEvent()         {}
func (ApprovalResolvedEvent) tuiEvent() {}
func (NoticeEvent) tuiEvent()           {}
func (ExitEvent) tuiEvent()             {}
