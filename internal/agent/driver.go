package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"CodeAgent/internal/llm"
)

type CheckpointID uint64
type CheckpointKind string

const (
	RunStarting        CheckpointKind = "run_starting"
	InputConsumed      CheckpointKind = "input_consumed"
	ModelRequestReady  CheckpointKind = "model_request_ready"
	ModelResponseReady CheckpointKind = "model_response_ready"
	ToolStarting       CheckpointKind = "tool_starting"
	ToolFinished       CheckpointKind = "tool_finished"
	RunFinishing       CheckpointKind = "run_finishing"
)

// Checkpoint 的内容由驱动者保持只读。ResponseID 关联执行节点，不是数据库 ID。
type Checkpoint struct {
	ID          CheckpointID
	Kind        CheckpointKind
	RunID       string
	Context     context.Context // 任务生命周期，仅供协调者中止可取消的外部工作。
	StepIndex   int
	UserMessage *llm.UserMessage
	Request     *llm.Request
	Response    *llm.AssistantMessage
	Origin      *ModelOrigin
	ResponseID  CheckpointID
	ToolCall    *llm.ContentBlock
	ToolResult  *llm.ToolResultMessage
	Result      *RunResult
}

type DecisionKind string

const (
	Continue DecisionKind = "continue"
	Skip     DecisionKind = "skip"
	Stop     DecisionKind = "stop"
)

// Stop 是通用执行故障；存储故障分类及超时由驱动者处理。
type Decision struct {
	Kind   DecisionKind
	Reason string
	Error  error
}

var (
	ErrDriverClaimed = errors.New("Agent 已有执行驱动者")
	ErrDriverBusy    = errors.New("Next 正在等待，不允许并发读取")
	ErrCheckpoint    = errors.New("节点不存在、尚未交付或已经确认")
	ErrDecision      = errors.New("该节点不接受此决定")
)

type Driver struct {
	agent   *Agent
	reading atomic.Bool
}

type pendingCheckpoint struct {
	checkpoint Checkpoint
	scheduler  *toolScheduler
	delivered  bool
	done       chan Decision
}

// Driver 只领取一次。所有执行都通过同一组 Next/Resolve，没有自动确认路径。
func (a *Agent) Driver() (*Driver, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.driverAttached {
		return nil, ErrDriverClaimed
	}
	a.driverAttached = true
	return &Driver{agent: a}, nil
}

// Next 的 ctx 仅取消本次等待；已交付节点仍须 Resolve，不能放弃事实收尾。
func (d *Driver) Next(ctx context.Context) (Checkpoint, error) {
	if !d.reading.CompareAndSwap(false, true) {
		return Checkpoint{}, ErrDriverBusy
	}
	defer d.reading.Store(false)
	for {
		if err := ctx.Err(); err != nil {
			return Checkpoint{}, err
		}
		a := d.agent
		a.mu.Lock()
		if len(a.checkpoints) > 0 {
			p := a.checkpoints[0]
			a.checkpoints[0] = nil
			a.checkpoints = a.checkpoints[1:]
			// 尚未交付的工具资格已失效时，执行层直接跳过，不伪造启动记录。
			if p.checkpoint.Kind == ToolStarting {
				if reason := p.scheduler.startPermissionLocked(); reason != "" {
					delete(a.pendingCheckpoints, p.checkpoint.ID)
					p.done <- Decision{Kind: Skip, Reason: reason}
					a.mu.Unlock()
					continue
				}
			}
			p.delivered = true
			a.mu.Unlock()
			return p.checkpoint, nil
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return Checkpoint{}, ctx.Err()
		case <-a.checkpointNotify:
		case <-a.done:
			return Checkpoint{}, io.EOF
		}
	}
}

// Resolve 消费一次确认。工具启动在此持锁预约，与 Steer/Cancel 共用状态边界。
// 预约之前的纠正会阻止执行；之后的纠正视工具为在途，不撤销副作用。
func (d *Driver) Resolve(ctx context.Context, id CheckpointID, decision Decision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a := d.agent
	a.mu.Lock()
	p := a.pendingCheckpoints[id]
	if p == nil || !p.delivered {
		a.mu.Unlock()
		return ErrCheckpoint
	}
	valid := decision.Kind == Continue && decision.Error == nil && decision.Reason == "" ||
		decision.Kind == Skip && p.checkpoint.Kind == ToolStarting && decision.Reason != "" && decision.Error == nil ||
		decision.Kind == Stop && decision.Error != nil
	if !valid {
		a.mu.Unlock()
		return ErrDecision
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	if decision.Kind == Stop {
		a.stopLocked(decision.Error)
	}
	if p.checkpoint.Kind == ToolStarting && decision.Kind == Continue {
		if reason := p.scheduler.startPermissionLocked(); reason != "" {
			decision = Decision{Kind: Skip, Reason: reason}
		}
	}
	delete(a.pendingCheckpoints, id)
	p.done <- decision
	a.mu.Unlock()
	return nil
}

// handoff 停在交接处直到驱动者确认；取消不会越过正在进行的外部协调。
func (a *Agent) handoff(run *activeRun, checkpoint Checkpoint, scheduler *toolScheduler) (CheckpointID, Decision) {
	a.mu.Lock()
	a.checkpointCounter++
	checkpoint.ID = a.checkpointCounter
	checkpoint.RunID = run.id
	checkpoint.Context = run.ctx
	p := &pendingCheckpoint{checkpoint: checkpoint, scheduler: scheduler, done: make(chan Decision, 1)}
	a.pendingCheckpoints[checkpoint.ID] = p
	a.checkpoints = append(a.checkpoints, p)
	a.mu.Unlock()
	select {
	case a.checkpointNotify <- struct{}{}:
	default:
	}
	return checkpoint.ID, <-p.done
}

// StopExecution 保留首次执行故障，并取消在途工作。必要结果仍通过交接节点交付。
func (a *Agent) StopExecution(err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	a.stopLocked(err)
	a.mu.Unlock()
	go a.dispatcher.drain()
}

func (a *Agent) stopLocked(err error) {
	if a.executionErr == nil {
		a.executionErr = err
	}
	if a.currentRun != nil {
		a.currentRun.cancel()
	}
	a.enqueueDroppedTasks(a.dropAllQueuedLocked(a.executionErr.Error()))
}

// WithExecutionBoundary 预约出队及请求准备边界；不持状态锁执行外部协调。
func (a *Agent) WithExecutionBoundary(ctx context.Context, idle bool, fn func() error) error {
	select {
	case a.boundary <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-a.boundary }()
	a.mu.Lock()
	closed, active, fault := a.status == AgentStatusClosed, a.currentRun != nil, a.executionErr
	a.mu.Unlock()
	if closed {
		return fmt.Errorf("Agent 已关闭")
	}
	if fault != nil {
		return fault
	}
	if idle && active {
		return fmt.Errorf("存在活动 Run，无法执行空闲管理操作")
	}
	return fn()
}

// canStartWorkLocked 与控制入口共享状态锁，确认模型启动的执行边界。
func (a *Agent) canStartWorkLocked(run *activeRun) bool {
	return a.executionErr == nil && run.ctx.Err() == nil && a.status == AgentStatusRunning
}
