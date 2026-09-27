package agent

import (
	"context"
	"sync"
)

// RunHandle 代表一次已被 Agent 接受的任务句柄。
type RunHandle struct {
	id     string
	agent  *Agent
	done   chan struct{}
	mu     sync.Mutex
	result *RunResult
}

func newRunHandle(id string, a *Agent) *RunHandle {
	return &RunHandle{
		id:    id,
		agent: a,
		done:  make(chan struct{}),
	}
}

// ID 返回本次任务的全局唯一标识。
func (h *RunHandle) ID() string {
	return h.id
}

// Wait 等待本次任务执行完成并返回最终结果。
// 若传入的 ctx 发生超时或取消，仅表示调用方放弃本次等待，底层任务继续执行。
// 多次调用 Wait 返回相同的最终结果。
func (h *RunHandle) Wait(ctx context.Context) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.result != nil {
			return *h.result, nil
		}
		return RunResult{RunID: h.id, State: RunStateError, ErrorMessage: "结果未就绪"}, nil
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}
}

// cancelHandle 取消指定 Handle 对应的任务。
func (a *Agent) cancelHandle(h *RunHandle) {
	if h == nil || h.id == "" {
		return
	}

	a.mu.Lock()
	if a.status == AgentStatusClosed {
		a.mu.Unlock()
		return
	}

	var droppedTasks []droppedTaskInfo
	var droppedSteers []string
	activeID := ""

	if a.currentRun != nil && a.currentRun.id == h.id {
		a.currentRun.cancel()
		a.status = AgentStatusCancelling
		droppedSteers = a.steerQueue
		a.steerQueue = nil
		activeID = a.currentRun.id
	} else {
		droppedTasks = a.dropQueuedByIDLocked(h.id, "任务被用户取消")
	}
	a.mu.Unlock()

	if len(droppedTasks) > 0 {
		a.notifyDroppedTasks(droppedTasks)
	}
	if len(droppedSteers) > 0 {
		a.notifyDroppedSteers(activeID, droppedSteers)
	}
}

// cancelAllHandles 取消全部现有任务，私有方法，自己管理锁和通知。
func (a *Agent) cancelAllHandles() {
	a.mu.Lock()
	activeID := ""
	if a.currentRun != nil {
		activeID = a.currentRun.id
		a.currentRun.cancel()
		if a.status != AgentStatusClosed {
			a.status = AgentStatusCancelling
		}
	}

	reason := "任务在排队时被用户取消"
	if a.status == AgentStatusClosed {
		reason = "Agent 已关闭"
	}
	droppedTasks := a.dropAllQueuedLocked(reason)

	droppedSteers := a.steerQueue
	a.steerQueue = nil
	a.mu.Unlock()

	// 解锁后唤醒工作循环
	select {
	case a.workNotify <- struct{}{}:
	default:
	}

	// 锁外执行通知
	if len(droppedTasks) > 0 {
		a.notifyDroppedTasks(droppedTasks)
	}
	if len(droppedSteers) > 0 {
		a.notifyDroppedSteers(activeID, droppedSteers)
	}
}

// setResult 写入终态结果并解除所有 Wait 等待。仅限 Agent 内部调用一次。
func (h *RunHandle) setResult(res RunResult) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.result != nil {
		return // 保持初次写入的终态结果不变
	}
	h.result = &res
	close(h.done)
}
