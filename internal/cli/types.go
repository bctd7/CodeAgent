// Package cli assembles existing services and provides terminal and evaluation flows.
// The executable must call Main or Execute explicitly; importing cli has no side effects.
package cli

import (
	"CodeAgent/internal/agent"
	"CodeAgent/internal/config"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/instructions"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/mcp"
	"CodeAgent/internal/memory/compact"
	"CodeAgent/internal/memory/longterm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	"CodeAgent/internal/skills"
	"CodeAgent/internal/tools/extend/webfetch"
	"CodeAgent/internal/tui"
	"context"
	"io"
	"time"
)

const (
	ExitOK               = 0
	ExitFailure          = 1
	ExitUsage            = 2
	ExitApprovalRequired = 3
	ExitDeferred         = 4
	ExitTimeout          = 124
	ExitCancelled        = 130
)

// IO has one input consumer. Interactive is a trusted property supplied by the host,
// never inferred from prompt content. CloseInput optionally unblocks an owned reader.
type IO struct {
	In          io.Reader
	Out, Err    io.Writer
	Interactive bool
	CloseInput  func() error
}

// Options is the trusted embedding boundary. ModelEntries and Stream can be supplied
// by a host or controlled evaluation; nil ModelEntries uses the existing TOML loader.
type Options struct {
	Version                      string
	ConfigPath, DataDir          string
	ModelEntries                 []config.ModelEntry
	Stream                       agent.StreamFunc
	SystemPrompt                 string
	Instructions                 instructions.Options
	Skills                       []skills.Source
	HookSources                  []hooks.Source
	HookOptions                  hooks.Options
	ModelHooks                   *hooks.ModelHooks
	Policy                       permission.Policy
	Compact                      compact.Settings
	MemoryDirectory, MemoryScope string
	MemoryLimits                 longterm.Limits
	Subagents                    bool
	Tools                        []runtime.ToolBinding
	WebFetch                     *webfetch.WebFetchOptions
	MCP                          *mcp.Manager // borrowed and already started; never closed or reconnected here
	CleanupTimeout               time.Duration
	TUIRun                       func(context.Context, tui.Config, tui.IO) error // nil uses the real terminal frontend
}

type ModelInfo struct {
	DisplayName   string      `json:"display_name"`
	Ref           string      `json:"ref"`
	Provider      string      `json:"provider"`
	ID            string      `json:"id"`
	API           llm.APIKind `json:"api"`
	ContextWindow int         `json:"context_window"`
}
type Capabilities struct {
	Skills         []skills.Source         `json:"skills"`
	Instructions   instructions.Options    `json:"instructions"`
	DefaultTools   bool                    `json:"default_tools"`
	CustomTools    int                     `json:"custom_tools"`
	MCP            bool                    `json:"mcp"`
	ScriptHooks    bool                    `json:"script_hooks"`
	ModelHooks     bool                    `json:"model_hooks"`
	Compact        compact.Settings        `json:"compact"`
	Memory         bool                    `json:"memory"`
	MemoryScope    string                  `json:"memory_scope,omitempty"`
	MemoryShutdown string                  `json:"memory_shutdown,omitempty"`
	Subagents      bool                    `json:"subagents"`
	WebFetch       bool                    `json:"webfetch"`
	WorkMode       permission.WorkMode     `json:"work_mode"`
	ApprovalMode   permission.ApprovalMode `json:"approval_mode"`
}
type Status struct {
	SessionID    string             `json:"session_id"`
	State        agent.AgentStatus  `json:"state"`
	RunID        string             `json:"run_id,omitempty"`
	QueuedRunIDs []string           `json:"queued_run_ids"`
	Closing      bool               `json:"closing"`
	Error        string             `json:"error,omitempty"`
	Model        ModelInfo          `json:"model"`
	Approvals    []runtime.Approval `json:"approvals"`
	Capabilities Capabilities       `json:"capabilities"`
	Servers      []mcp.ServerStatus `json:"servers,omitempty"`
}
type RunOutput struct {
	SessionID      string              `json:"session_id,omitempty"`
	RunID          string              `json:"run_id,omitempty"`
	State          agent.RunState      `json:"state"`
	Text           string              `json:"text"`
	StopReason     agent.RunStopReason `json:"stop_reason,omitempty"`
	Error          string              `json:"error,omitempty"`
	ExitCode       int                 `json:"exit_code"`
	Steps          int                 `json:"steps"`
	Usage          llm.Usage           `json:"usage"`
	MemoryShutdown string              `json:"memory_shutdown,omitempty"`
}

// Execute owns the services it constructs and always closes them before returning.
// Context cancellation applies to input, current Run, and service admission.
func Execute(ctx context.Context, args []string, streams IO, options Options) int {
	return execute(ctx, args, streams, options)
}
