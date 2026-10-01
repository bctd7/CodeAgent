package agent

import (
	"context"
	"time"

	"CodeAgent/internal/llm"
)

// queuedRun 表示等待执行的排队任务。
type queuedRun struct {
	id      string
	handle  *RunHandle
	ctx     context.Context
	userMsg *llm.UserMessage
}

// droppedTaskInfo 记录在锁内被移除或放弃的排队任务及其原因。
type droppedTaskInfo struct {
	q      *queuedRun
	reason string
}

// dropAllQueuedLocked 清空当前所有排队任务并返回丢弃列表。
// 调用方必须持有 a.mu。
func (a *Agent) dropAllQueuedLocked(reason string) []droppedTaskInfo {
	if len(a.queue) == 0 {
		return nil
	}
	dropped := make([]droppedTaskInfo, 0, len(a.queue))
	for _, q := range a.queue {
		dropped = append(dropped, droppedTaskInfo{q: q, reason: reason})
	}
	a.queue = nil
	return dropped
}

// dropQueuedByIDLocked 从队列中寻找并移除指定 runID 的排队任务。
// 调用方必须持有 a.mu。
func (a *Agent) dropQueuedByIDLocked(runID string, reason string) []droppedTaskInfo {
	for i, q := range a.queue {
		if q.id == runID {
			a.queue = append(a.queue[:i], a.queue[i+1:]...)
			return []droppedTaskInfo{{q: q, reason: reason}}
		}
	}
	return nil
}

// notifyDroppedSteers 对外发布被丢弃的纠正事件。调用方必须在锁外调用。
func (a *Agent) notifyDroppedSteers(runID string, droppedSteers []string) {
	for _, st := range droppedSteers {
		a.dispatcher.emit(Event{
			Kind:      EventInputDropped,
			RunID:     runID,
			Detail:    "任务已取消，未消费纠正被丢弃: " + st,
			Timestamp: time.Now(),
		})
	}
}

// notifyDroppedTasks 解锁并通知被丢弃/取消的排队任务。调用方必须在锁外调用。
func (a *Agent) notifyDroppedTasks(tasks []droppedTaskInfo) {
	a.enqueueDroppedTasks(tasks)
	a.dispatcher.drain()
}

func (a *Agent) enqueueDroppedTasks(tasks []droppedTaskInfo) {
	for _, item := range tasks {
		errVal := item.q.ctx.Err()
		if errVal == nil {
			errVal = context.Canceled
		}
		res := RunResult{
			RunID:        item.q.id,
			State:        RunStateAborted,
			ErrorMessage: item.reason,
			Error:        errVal,
		}
		item.q.handle.setResult(res)
		a.dispatcher.enqueue(Event{
			Kind:      EventInputDropped,
			RunID:     item.q.id,
			Detail:    item.reason,
			Timestamp: time.Now(),
		})
		a.dispatcher.enqueue(Event{
			Kind:      EventRunEnd,
			RunID:     item.q.id,
			Result:    &res,
			Timestamp: time.Now(),
		})
	}
}

// enqueueRunLocked 将新任务加入队列末尾，返回排队位置（从 1 开始）。
// 调用方必须持有 a.mu。
func (a *Agent) enqueueRunLocked(runID string, handle *RunHandle, ctx context.Context, message *llm.UserMessage) int {
	queuedItem := &queuedRun{
		id:      runID,
		handle:  handle,
		ctx:     ctx,
		userMsg: message,
	}
	a.queue = append(a.queue, queuedItem)
	return len(a.queue)
}

// popNextRunLocked 依 FIFO 规则从队列头部取出一个未取消的任务作为下一个活动 Run。
// 在遍历过程中，若遇到 context 已取消的排队任务，将其收集到 skippedTasks 中并移除。
// 若成功取到有效任务，将其构造成 activeRun，同时更新 a.currentRun 与 a.status；
// 若队列为空或所有任务均已取消，置 a.currentRun = nil 且 a.status = AgentStatusIdle。
// 调用方必须持有 a.mu。
func (a *Agent) popNextRunLocked() (*activeRun, []droppedTaskInfo) {
	if a.status == AgentStatusClosed || a.executionErr != nil {
		return nil, nil
	}

	var skipped []droppedTaskInfo
	var nextRun *activeRun

	for len(a.queue) > 0 {
		candidate := a.queue[0]
		a.queue = a.queue[1:]

		if candidate.ctx.Err() != nil {
			skipped = append(skipped, droppedTaskInfo{q: candidate, reason: "排队任务 Context 取消"})
			continue
		}

		runCtx, runCancel := context.WithCancel(candidate.ctx)
		nextRun = &activeRun{
			id:       candidate.id,
			handle:   candidate.handle,
			ctx:      runCtx,
			cancel:   runCancel,
			userMsg:  candidate.userMsg,
			usage:    make(map[string]llm.Usage),
			messages: make([]llm.Message, 0),
		}
		a.currentRun = nextRun
		a.status = AgentStatusRunning
		break
	}

	if nextRun == nil {
		a.currentRun = nil
		a.status = AgentStatusIdle
	}

	return nextRun, skipped
}
