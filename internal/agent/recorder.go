package agent

import (
	"context"
	"fmt"
	"time"

	"CodeAgent/internal/llm"
)

// Recorder 是执行节点的同步记录契约。实现不能同步等待当前 Run 或关闭 Agent。
// 消息标识只用于关联随后工具记录，Agent 不解释其存储格式。
type Recorder interface {
	StartRun(context.Context, string, *llm.UserMessage) error
	RecordStep(context.Context, string) error
	AppendMessage(context.Context, string, llm.Message, *ModelOrigin) (string, error)
	RecordToolStart(context.Context, string, string) error
	AppendToolResult(context.Context, string, *llm.ToolResultMessage) error
	FinishRun(context.Context, RunResult) error
}

// WithIdleOperation 将管理操作与任务出队互斥；fn 执行时不持状态锁。
// 不限制排队输入，操作结束后仍由原有循环执行它们。
func (a *Agent) WithIdleOperation(ctx context.Context, fn func() error) error {
	select {
	case a.boundary <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-a.boundary }()
	a.mu.Lock()
	closed, active := a.status == AgentStatusClosed, a.currentRun != nil
	a.mu.Unlock()
	if closed {
		return fmt.Errorf("Agent 已关闭")
	}
	if active {
		return fmt.Errorf("存在活动 Run，无法执行空闲管理操作")
	}
	return fn()
}

// recordingFailure 保留首个保存错误。用于停止执行的取消不能覆盖该原因。
func (a *Agent) recordingFailure(run *activeRun, err error) {
	a.mu.Lock()
	if a.recordingErr == nil {
		a.recordingErr = fmt.Errorf("持久化失败: %w", err)
	}
	run.cancel()
	a.mu.Unlock()
}

func (a *Agent) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.recordTimeout)
}

// appendRecordedMessage 保存最终响应或已消费纠正，不复用已取消的任务 context。
func (a *Agent) appendRecordedMessage(run *activeRun, msg llm.Message, origin *ModelOrigin) (string, error) {
	if a.recorder == nil {
		return "", nil
	}
	ctx, cancel := a.cleanupContext()
	defer cancel()
	id, err := a.recorder.AppendMessage(ctx, run.id, msg, origin)
	if err != nil {
		a.recordingFailure(run, err)
	}
	return id, err
}

const defaultRecordTimeout = 5 * time.Second

// FailPersistence 停止新增执行并保留故障原因；不关闭整个会话实例。
func (a *Agent) FailPersistence(err error) {
	a.mu.Lock()
	if a.recordingErr == nil {
		a.recordingErr = fmt.Errorf("持久化失败: %w", err)
	}
	if a.currentRun != nil {
		a.currentRun.cancel()
	}
	dropped := a.dropAllQueuedLocked(a.recordingErr.Error())
	a.enqueueDroppedTasks(dropped)
	a.mu.Unlock()
	go a.dispatcher.drain()
}
