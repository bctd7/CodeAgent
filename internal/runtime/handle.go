package runtime

import (
	"CodeAgent/internal/agent"
	"context"
)

// RunHandle 是 Runtime 对外句柄。RunFinishing 被协调确认后结果才就绪。
// 排队取消不产生持久化 Run，直接沿用执行层取消结果。
type RunHandle struct{ execution *agent.RunHandle }

func (h *RunHandle) ID() string                                        { return h.execution.ID() }
func (h *RunHandle) Cancel()                                           { h.execution.Cancel() }
func (h *RunHandle) Wait(ctx context.Context) (agent.RunResult, error) { return h.execution.Wait(ctx) }
