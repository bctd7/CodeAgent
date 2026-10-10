package agent

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"CodeAgent/internal/llm"
)

type CheckpointID uint64
type CheckpointKind string

const (
	RunStarting       CheckpointKind = "run_starting"
	InputConsumed     CheckpointKind = "input_consumed"
	ModelRequestReady CheckpointKind = "model_request_ready"
	// 确认后必定调用 Stream（即使随后取消）；协调者在此保存尝试调用事实。
	ModelCallStarting  CheckpointKind = "model_call_starting"
	ModelResponseReady CheckpointKind = "model_response_ready"
	ToolStarting       CheckpointKind = "tool_starting"
	ToolFinished       CheckpointKind = "tool_finished"
	ToolBatchFinished  CheckpointKind = "tool_batch_finished"
	RunEnding          CheckpointKind = "run_ending"
	RunFinishing       CheckpointKind = "run_finishing"
)

// Checkpoint 的内容及其嵌套切片、map 和消息由驱动者递归只读。
// ResponseID 关联执行节点，不是数据库 ID。
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
	ToolOutcome *ToolOutcome
	ToolBatch   []ToolOutcome
	// Result 在 RunEnding 为字段齐全的结束候选，在 RunFinishing 为待提交终态。
	// 候选不保证成为终态；Repeat、纠正、取消或故障仍可改变后续结果。
	Result *RunResult
}

type DecisionKind string

const (
	Continue DecisionKind = "continue"
	Skip     DecisionKind = "skip"
	Stop     DecisionKind = "stop"
	Repeat   DecisionKind = "repeat"
	DeferRun DecisionKind = "defer"
)

// RequestContext 只承载本次请求可替换的系统提示词和消息，不提供复制或隔离。
type RequestContext struct {
	SystemPrompt string
	Messages     []llm.Message
}

// Stop 终止本轮；持久故障由驱动者另行调用 StopExecution 关闭准入。
// 替换数据在 Resolve 成功交接后不得再由返回者修改。
// 替换值与输入共享的未修改嵌套数据仍须只读；交接不做深复制。
type Decision struct {
	// Disposition 仅用于 Skip，区分策略拒绝与取消/纠正造成的未执行。
	Disposition ToolStatus
	Kind        DecisionKind
	Reason      string
	Error       error
	// RequestContext 仅用于 ModelRequestReady + Continue，替换本次模型请求上下文。
	RequestContext *RequestContext
	// ToolArguments 仅用于 ToolStarting；Continue 替换执行参数，Skip/DeferRun 记录未执行的最终参数。
	ToolArguments *map[string]any
	// ToolContent 仅用于 ToolFinished + Continue，替换模型可见内容，保留原始结果。
	ToolContent *[]llm.ContentBlock
	// ContextMessages 是来源明确的宿主反馈，不冒充已消费的用户输入。
	// 仅用于 RunEnding + Repeat 或 ToolBatchFinished + Continue，按给定顺序追加。
	ContextMessages []llm.Message
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
					p.done <- Decision{Kind: Skip, Reason: reason, Disposition: ToolSkipped}
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
		decision.Kind == Stop && decision.Error != nil ||
		decision.Kind == Repeat && p.checkpoint.Kind == RunEnding && decision.Error == nil && len(decision.ContextMessages) > 0 ||
		decision.Kind == DeferRun && p.checkpoint.Kind == ToolStarting && decision.Error == nil && decision.Reason != ""
	valid = valid && (decision.RequestContext == nil || decision.Kind == Continue && p.checkpoint.Kind == ModelRequestReady) &&
		(decision.ToolArguments == nil || (decision.Kind == Continue || decision.Kind == Skip || decision.Kind == DeferRun) && p.checkpoint.Kind == ToolStarting) &&
		(decision.ToolContent == nil || decision.Kind == Continue && p.checkpoint.Kind == ToolFinished) &&
		(len(decision.ContextMessages) == 0 || decision.Kind == Repeat && p.checkpoint.Kind == RunEnding || decision.Kind == Continue && p.checkpoint.Kind == ToolBatchFinished)
	valid = valid && (decision.Disposition == "" || decision.Kind == Skip && (decision.Disposition == ToolDenied || decision.Disposition == ToolSkipped || decision.Disposition == ToolCanceled))
	if !valid {
		a.mu.Unlock()
		return ErrDecision
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	if decision.Kind == Stop {
		// 只有 StopExecution 会登记永久执行故障。
		// 已接受的取消也优先于随后返回的业务错误。
		if a.currentRun.err == nil && a.currentRun.ctx.Err() == nil {
			a.currentRun.err = decision.Error
		}
		a.currentRun.cancel()
	}
	if decision.Kind == DeferRun && a.currentRun.ctx.Err() == nil {
		a.currentRun.deferred = decision.Reason
		a.currentRun.cancel()
	}
	if decision.Kind == Repeat && a.currentRun.ctx.Err() != nil {
		decision = Decision{Kind: Continue}
	}
	if p.checkpoint.Kind == ToolStarting && decision.Kind == Continue {
		if reason := p.scheduler.startPermissionLocked(); reason != "" {
			decision = Decision{Kind: Skip, Reason: reason, Disposition: ToolSkipped, ToolArguments: decision.ToolArguments}
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
