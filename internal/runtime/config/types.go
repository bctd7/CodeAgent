// Package config 保存 Runtime 的装配参数，不读取配置文件。
package config

import (
	"errors"
	"time"

	"CodeAgent/internal/agent"
	modelconfig "CodeAgent/internal/config"
	"CodeAgent/internal/hooks"
	sessionconfig "CodeAgent/internal/session/config"
)

var (
	ErrSessionOwned  = errors.New("session is owned by another runtime")
	ErrClosed        = errors.New("runtime is closed")
	ErrClosing       = errors.New("runtime is closing")
	ErrFaulted       = errors.New("runtime persistence fault")
	ErrModelRequired = errors.New("explicit model selection is required")
)

type State string

const (
	StateInitializing State = "initializing"
	StateReady        State = "ready"
	StateFaulted      State = "faulted"
	StateClosing      State = "closing"
	StateClosed       State = "closed"
)

// Config 中 Models 及其共享字段由调用方保持只读；凭据不进入对外快照。
type Config struct {
	DatabasePath     string
	Models           *modelconfig.Config
	Stream           agent.StreamFunc // nil 使用现有 Provider 调度入口。
	SystemPrompt     string
	MaxParallelTools int
	RecordTimeout    time.Duration // 默认 5 秒，用于取消后的保存及故障收尾。
	Hooks            *hooks.Registry
	Tools            []agent.Tool // nil 装配当前平台的七个基础工具；供嵌入和本地验证注入。
	// 回调应快速返回；不能同步等待当前 Run 或同步调用等待式 Close。
	OnEvent func(sessionID string, event agent.Event)
}

type CreateSessionOptions struct {
	Title      string
	WorkingDir string
	Model      sessionconfig.ModelSelection
}

type OpenSessionOptions struct {
	// 显式替换已有选择，或为旧会话补充选择；nil 沿用持久化值。
	Model *sessionconfig.ModelSelection
}

type Snapshot struct {
	SessionID    string                       `json:"session_id"`
	State        State                        `json:"state"`
	ErrorMessage string                       `json:"error_message,omitempty"`
	Model        sessionconfig.ModelSelection `json:"model"`
	WorkingDir   string                       `json:"working_dir"`
	Agent        agent.Snapshot               `json:"agent"`
}
