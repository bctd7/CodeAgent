package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/memory/longterm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/session"
	sc "CodeAgent/internal/session/config"
	"github.com/google/uuid"
)

// Event 是展示通知；执行和保存不依赖订阅者，不暴露审批凭据或模型选项。
type Event struct {
	// Dropped 表示该观察者缓冲溢出丢弃的通知数；显示端应重新读取 Snapshot。
	Dropped uint64
	Agent          *agent.Event
	Approval       *Approval
	Notices        *hooks.Notices
	Diagnostic     error
	AuxiliaryUsage *llm.Usage
}
type Snapshot struct {
	SessionID string
	Agent     agent.Snapshot
	Closing   bool
	Fault     error
	Approvals []Approval
}

type Runtime struct {
	host           *Host
	id             string
	session        *session.Session
	owner          *os.File
	mu             sync.Mutex
	operation      sync.Mutex // 只保护准入/空闲装配，Driver 与工具等待不取得此锁。
	submitting     int
	submissionDone chan struct{}
	agent          *agent.Agent
	driver         *agent.Driver
	driverDone     chan struct{}
	life           context.Context
	cancel         context.CancelFunc
	closing        bool
	fault          error
	closeOnce      sync.Once
	done           chan struct{}
	closeErr       error
	hooks          *hooks.Engine
	modelHooks     *hooks.ModelHooks
	policy         permission.Policy
	permissions    *permission.Session
	modelRef       string
	bindings       map[string]*toolBinding
	prompt         string
	role           string
	parentOwner    string
	scope          []string
	subscriptions  map[string]*observer
	inputs         map[*llm.UserMessage][]sc.ContextInput
	initial        []sc.ContextInput
	approvals      map[string]*pendingApproval
	records        map[string]*runRecords // 仅 Driver 主协调者使用。
	feedback       map[string][]feedback  // 工具节点可并发准备，按节点 ID 汇总。
	children       map[string]*childExecution
	memory         *longterm.Service
	memoryCancel   context.CancelFunc
	memoryDone     chan struct{}
	memoryModel    ModelConfig
	memoryWake     bool
}

func (h *Host) initialize(ctx context.Context, id, role string, scope []string) (_ *Runtime, err error) {
	owner, err := h.lockFile("sessions", id)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	r := &Runtime{host: h, id: id, owner: owner, life: life, cancel: cancel, done: make(chan struct{}), hooks: h.options.Hooks, modelHooks: h.options.ModelHooks, policy: h.options.Policy, role: role, scope: scope, modelRef: h.options.ModelRef, subscriptions: map[string]*observer{}, inputs: map[*llm.UserMessage][]sc.ContextInput{}, approvals: map[string]*pendingApproval{}, records: map[string]*runRecords{}, feedback: map[string][]feedback{}, children: map[string]*childExecution{}}
	defer func() {
		if err != nil {
			cancel()
			if r.agent != nil {
				r.agent.RequestClose()
				_ = r.agent.Close()
				if r.driverDone != nil {
					<-r.driverDone
				}
			}
			if r.memory != nil {
				_ = r.memory.Close()
			}
			_ = closeLock(owner)
		}
	}()
	r.session, err = h.service.OpenSession(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := r.session.GetInfo(ctx)
	if err != nil {
		return nil, err
	}
	if info.WorkingDir == "" {
		return nil, fmt.Errorf("会话必须绑定项目目录")
	}
	dir, err := realPath(info.WorkingDir)
	if err != nil {
		return nil, err
	}
	if dir != h.options.ProjectDir {
		return nil, fmt.Errorf("会话目录与 Host 项目绑定不一致")
	}
	if info.ModelSelection != nil {
		r.modelRef = info.ModelSelection.Ref
	}
	if _, ok := h.options.Models[r.modelRef]; !ok {
		return nil, fmt.Errorf("模型引用不存在: %s", r.modelRef)
	}
	unfinished, err := r.session.ListUnfinishedRuns(ctx)
	if err != nil {
		return nil, err
	}
	for _, run := range unfinished {
		_, err = r.session.FinishRun(ctx, run.ID, sc.RunCompletion{State: sc.RunStateAborted, ErrorMessage: "前一执行者已释放所有权，运行中断"})
		if err != nil {
			return nil, err
		}
	}
	r.permissions, err = permission.NewSession(id)
	if err != nil {
		return nil, err
	}
	if role == "explore" {
		r.policy.WorkMode = permission.Plan
	}
	r.prompt, err = r.loadPrompt(ctx)
	if err != nil {
		return nil, err
	}
	if h.options.MemoryDirectory != "" {
		r.memory, err = longterm.NewService(h.options.MemoryDirectory, longterm.Options{Limits: h.options.MemoryLimits, Execute: r.executeMemory})
		if err != nil {
			return nil, err
		}
	}
	if err = r.assemble(ctx); err != nil {
		return nil, err
	}
	if r.hooks != nil {
		source:="startup";if info.HeadID!="" {source="resume"}
		out, hookErr := r.hooks.SessionStart(ctx, hooks.SessionStartInput{CommonInput: r.common(), Source: source})
		r.emit(Event{Notices: &out.Notices})
		if hookErr = hookError(out.Notices, hookErr); hookErr != nil {
			return nil, hookErr
		}
		if out.StopProcessing {
			return nil, fmt.Errorf("SessionStart 停止: %s", strings.Join(out.StopReasons, "; "))
		}
		r.initial = contexts("hook_session_start", uuid.NewString(), out.AdditionalContext)
	}
	return r, nil
}

func (r *Runtime) common() hooks.CommonInput {
	return hooks.CommonInput{SessionID: r.id, CWD: r.host.options.ProjectDir, ProjectDir: r.host.options.ProjectDir, PermissionMode: string(r.policy.ApprovalMode)}
}
func (r *Runtime) SessionID() string { return r.id }

func (r *Runtime) admission() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return ErrClosed
	}
	return r.fault
}

func (r *Runtime) Submit(ctx context.Context, message *llm.UserMessage) (*agent.RunHandle, error) {
	a, finish, err := r.beginInput()
	if err != nil {
		return nil, err
	}
	defer finish()
	if message == nil {
		return nil, fmt.Errorf("用户输入不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx := ctx
	ctx, cancel := linkedContext(ctx, r.life)
	defer cancel()
	var additions []sc.ContextInput
	if r.hooks != nil {
		var text []string
		for _, block := range message.Content {
			if block.Type == llm.TypeText {
				text = append(text, block.Text)
			}
		}
		out, err := r.hooks.UserPromptSubmit(ctx, hooks.UserPromptSubmitInput{CommonInput: r.common(), Prompt: strings.Join(text, "\n")})
		r.emit(Event{Notices: &out.Notices})
		if err = hookError(out.Notices, err); err != nil {
			return nil, err
		}
		if out.StopProcessing || out.Rejected {
			return nil, fmt.Errorf("输入被 Hook 拒绝: %s", strings.Join(append(out.Reasons, out.StopReasons...), "; "))
		}
		additions = contexts("hook_prompt", uuid.NewString(), out.AdditionalContext)
	}
	// 消息指针只关联尚未消费输入，不用正文猜测身份，也不另设输入队列。
	r.mu.Lock()
	if _, exists := r.inputs[message]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("同一输入对象仍在等待消费")
	}
	r.inputs[message] = additions
	r.mu.Unlock()
	if err := r.admission(); err != nil {
		r.mu.Lock()
		delete(r.inputs, message)
		r.mu.Unlock()
		return nil, err
	}
	handle, err := a.Submit(runCtx, message)
	if err != nil {
		r.mu.Lock()
		delete(r.inputs, message)
		r.mu.Unlock()
		return nil, err
	}
	// Submit 的生命周期不能被此函数返回时取消；只解除上面临时 Hook context。
	return handle, nil
}

func (r *Runtime) Steer(text string) error {
	a, finish, err := r.beginInput()
	if err != nil {
		return err
	}
	defer finish()
	return a.Steer(text)
}

// 准入计数只保护检查到提交的交接，不保存输入或复制 Agent 队列；Hook 在锁外执行。
func (r *Runtime) beginInput() (*agent.Agent, func(), error) {
	r.operation.Lock()
	defer r.operation.Unlock()
	if err := r.admission(); err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	if r.submitting == 0 {
		r.submissionDone = make(chan struct{})
	}
	r.submitting++
	a := r.agent
	r.mu.Unlock()
	return a, func() {
		r.mu.Lock()
		r.submitting--
		if r.submitting == 0 {
			close(r.submissionDone)
		}
		r.mu.Unlock()
	}, nil
}
func (r *Runtime) CancelRun(handle *agent.RunHandle) {
	if handle != nil {
		handle.Cancel()
	}
}

func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	a := r.agent
	out := Snapshot{SessionID: r.id, Closing: r.closing, Fault: r.fault}
	for _, p := range r.approvals {
		out.Approvals = append(out.Approvals, p.view())
	}
	r.mu.Unlock()
	if a != nil {
		out.Agent = a.Snapshot()
	}
	return out
}

// Subscribe 使用独立的有界展示缓冲；慢观察者不阻塞执行。事件只读，溢出可用 Snapshot 恢复。
func (r *Runtime) Subscribe(callback func(Event)) func() {
	if callback==nil{return func(){}}
	id := uuid.NewString()
	r.mu.Lock()
	if r.closing {r.mu.Unlock();return func(){}}
	listener:=newObserver(func(_ string,event Event){callback(event)})
	r.subscriptions[id]=listener
	r.mu.Unlock()
	return func() { r.mu.Lock(); delete(r.subscriptions, id); r.mu.Unlock();listener.stop() }
}
func (r *Runtime) emit(event Event) {
	r.mu.Lock()
	callbacks := make([]*observer, 0, len(r.subscriptions))
	for _, fn := range r.subscriptions {
		callbacks = append(callbacks, fn)
	}
	r.mu.Unlock()
	r.host.emit(r.id, event)
	for _, fn := range callbacks {
		fn.publish(r.id,event)
	}
}

type observation struct { sessionID string; event Event }
// observer 仅传展示通知，不参与审批或执行交接。每个观察者最多一个回调在运行。
type observer struct { mu sync.Mutex; items chan observation; stopped bool; dropped uint64 }
func newObserver(callback func(string,Event)) *observer {
	if callback==nil{return nil};o:=&observer{items:make(chan observation,64)}
	go func(){for item:=range o.items {o.mu.Lock();stopped:=o.stopped;o.mu.Unlock();if stopped{return};func(){defer func(){if value:=recover();value!=nil {log.Printf("Runtime 观察者 panic: %v",value)}}();callback(item.sessionID,item.event)}()}}()
	return o
}
func (o *observer) publish(id string,event Event) {if o==nil{return};o.mu.Lock();defer o.mu.Unlock();if o.stopped{return};select{case o.items<-observation{id,event}:return;default:};select{case old:=<-o.items:o.dropped+=1+old.event.Dropped;default:};event.Dropped+=o.dropped;o.dropped=0;select{case o.items<-observation{id,event}:default:o.dropped+=1+event.Dropped}}
// 不等待用户回调：Go 无法强制终止回调，关闭不得因此扣留数据库或 OS 锁。
func (o *observer) stop() {if o==nil{return};o.mu.Lock();defer o.mu.Unlock();if !o.stopped{o.stopped=true;close(o.items)}}

func linkedContext(ctx, lifetime context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	return ctx, func() { stop(); cancel() }
}
func hookError(notices hooks.Notices, err error) error {
	failures := []error{err}
	for _, report := range notices.Reports {
		if report.Error != nil {
			failures = append(failures, fmt.Errorf("hook %s: %w", report.HookID, report.Error))
		}
	}
	return errors.Join(failures...)
}

func (r *Runtime) persistFault(err error) {
	r.mu.Lock()
	if r.fault == nil {
		r.fault = err
	}
	r.mu.Unlock()
	r.agent.StopExecution(err)
	r.emit(Event{Diagnostic: err})
}
func (r *Runtime) recordContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), r.host.options.RecordTimeout)
}

func (r *Runtime) beginClose() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closing = true
		r.mu.Unlock()
		r.cancel()
		go func() {
			r.operation.Lock()
			r.agent.RequestClose()
			r.operation.Unlock()
			r.cancelChildren()
			r.stopMemory()
			var failures []error
			failures = append(failures, r.agent.Close())
			<-r.driverDone
			r.mu.Lock()
			submissionDone := r.submissionDone
			r.mu.Unlock()
			if submissionDone != nil {
				<-submissionDone
			}
			if r.hooks != nil {
				ctx, cancel := r.recordContext()
				out, err := r.hooks.SessionEnd(ctx, hooks.SessionEndInput{CommonInput: r.common(), Reason: "other"})
				cancel()
				r.emit(Event{Notices: &out.Notices})
				failures = append(failures, hookError(out.Notices, err))
			}
			r.permissions.Clear()
			if r.memory != nil {
				failures = append(failures, r.memory.Close())
			}
			failures = append(failures, closeLock(r.owner))
			r.mu.Lock()
			for _,listener:=range r.subscriptions {listener.stop()};clear(r.subscriptions)
			clear(r.children)
			clear(r.inputs)
			clear(r.feedback)
			r.closeErr = errors.Join(failures...)
			r.mu.Unlock()
			r.host.mu.Lock()
			if r.host.runtimes[r.id] == r {
				delete(r.host.runtimes, r.id)
			}
			r.host.mu.Unlock()
			close(r.done)
		}()
	})
}
func (r *Runtime) Close(ctx context.Context) error {
	r.beginClose()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.closeErr
	}
}

// SetModel 在 Agent 的原子空闲边界保存选择，辅助任务继续使用触发时快照。
func (r *Runtime) SetModel(ctx context.Context, ref string) error {
	r.operation.Lock()
	defer r.operation.Unlock()
	if err := r.admission(); err != nil {
		return err
	}
	model, ok := r.host.options.Models[ref]
	if !ok {
		return fmt.Errorf("模型引用不存在")
	}
	return r.idle(ctx, func() error {
		if r.hasActiveChildren() {
			return ErrBusy
		}
		if err := r.host.service.SetModelSelection(ctx, r.id, sc.ModelSelection{Ref: ref}); err != nil {
			return err
		}
		if err := r.agent.SetModel(model.Model, model.Options); err != nil {
			return err
		}
		r.modelRef = ref
		return nil
	})
}

func contexts(kind, id string, values []string) []sc.ContextInput {
	var out []sc.ContextInput
	for i, value := range values {
		if value != "" {
			out = append(out, sc.ContextInput{SourceKind: kind, SourceID: fmt.Sprintf("%s/%d", id, i), Message: &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: value}}, Timestamp: time.Now()}})
		}
	}
	return out
}
