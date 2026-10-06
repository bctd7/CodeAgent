package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	runtimeconfig "CodeAgent/internal/runtime/config"
	"CodeAgent/internal/session"
	sessionconfig "CodeAgent/internal/session/config"
)

// Runtime 协调一个持锁会话。任务队列和执行状态仍由 Agent 维护。
type Runtime struct {
	mu         sync.Mutex
	control    chan struct{}
	host       *Host
	session    *session.Session
	agent      *agent.Agent
	driver     *agent.Driver
	driverDone chan struct{}
	owner      *ownership
	info       sessionconfig.SessionInfo
	state      runtimeconfig.State
	fault      error
	closeOnce  sync.Once
	done       chan struct{}
	closeErr   error
}

func (r *Runtime) SessionID() string { return r.info.ID }

func (r *Runtime) admission() error {
	r.host.mu.Lock()
	closing := r.host.closing
	r.host.mu.Unlock()
	if closing {
		return runtimeconfig.ErrClosing
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case runtimeconfig.StateReady:
		return nil
	case runtimeconfig.StateFaulted:
		return errors.Join(runtimeconfig.ErrFaulted, r.fault)
	case runtimeconfig.StateClosing:
		return runtimeconfig.ErrClosing
	default:
		return runtimeconfig.ErrClosed
	}
}

func (r *Runtime) Submit(ctx context.Context, message *llm.UserMessage) (*RunHandle, error) {
	if err := r.admission(); err != nil {
		return nil, err
	}
	execution, err := r.agent.Submit(ctx, message)
	if err != nil {
		return nil, err
	}
	return &RunHandle{execution: execution}, nil
}

func (r *Runtime) Steer(message string) error {
	if err := r.admission(); err != nil {
		return err
	}
	return r.agent.Steer(message)
}

func (r *Runtime) SetModel(ctx context.Context, selection sessionconfig.ModelSelection) error {
	entry, err := r.host.resolveModel(selection)
	if err != nil {
		return err
	}
	if err := r.admission(); err != nil {
		return err
	}
	saved := false
	err = r.agent.WithExecutionBoundary(ctx, false, func() error {
		return r.manage(ctx, false, func() error {
			if err := r.host.service.SetModelSelection(ctx, r.SessionID(), selection); err != nil {
				return err
			}
			r.mu.Lock()
			r.info.ModelSelection = &selection
			r.mu.Unlock()
			saved = true
			return r.agent.SetModel(entry.Model, entry.Options)
		})
	})
	if err != nil && saved {
		r.persistenceFault(err)
	}
	return err
}

func (r *Runtime) BindWorkingDir(ctx context.Context, path string) error {
	return r.manage(ctx, true, func() error {
		if err := r.host.service.BindWorkingDir(ctx, r.SessionID(), path); err != nil {
			return err
		}
		info, err := r.host.service.GetSession(ctx, r.SessionID())
		if err != nil {
			r.persistenceFault(err)
			return err
		}
		r.mu.Lock()
		r.info.WorkingDir = info.WorkingDir
		r.mu.Unlock()
		return nil
	})
}

// manage 串行管理操作；idle 操作另外预约 Agent 出队边界，不持 Agent 状态锁写库。
func (r *Runtime) manage(ctx context.Context, idle bool, fn func() error) error {
	if idle {
		return r.agent.WithExecutionBoundary(ctx, true, func() error { return r.manage(ctx, false, fn) })
	}
	select {
	case r.control <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.control }()
	if err := r.admission(); err != nil {
		return err
	}
	return fn()
}

// 读取允许故障及关闭中的实例观察已提交事实；完整关闭后停止使用共享存储。
func (r *Runtime) query(ctx context.Context, fn func() error) error {
	select {
	case r.control <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.control }()
	r.mu.Lock()
	closed := r.state == runtimeconfig.StateClosed
	r.mu.Unlock()
	if closed {
		return runtimeconfig.ErrClosed
	}
	return fn()
}

func (r *Runtime) GetCurrentHistory(ctx context.Context) (entries []sessionconfig.Entry, err error) {
	err = r.query(ctx, func() error { entries, err = r.session.GetCurrentHistory(ctx); return err })
	return
}
func (r *Runtime) GetRun(ctx context.Context, id string) (detail sessionconfig.RunDetail, err error) {
	err = r.query(ctx, func() error { detail, err = r.session.GetRun(ctx, id); return err })
	return
}
func (r *Runtime) ListRuns(ctx context.Context, limit, offset int) (runs []sessionconfig.RunRecord, err error) {
	err = r.query(ctx, func() error { runs, err = r.session.ListRuns(ctx, limit, offset); return err })
	return
}

func (r *Runtime) Snapshot() runtimeconfig.Snapshot {
	r.mu.Lock()
	s := runtimeconfig.Snapshot{SessionID: r.info.ID, State: r.state, WorkingDir: r.info.WorkingDir}
	if r.info.ModelSelection != nil {
		s.Model = *r.info.ModelSelection
	}
	if r.fault != nil {
		s.ErrorMessage = r.fault.Error()
	}
	r.mu.Unlock()
	s.Agent = r.agent.Snapshot()
	return s
}

func (r *Runtime) firstFault() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fault
}

func (r *Runtime) persistenceFault(err error) {
	r.mu.Lock()
	if r.fault == nil {
		r.fault = err
	}
	if r.state == runtimeconfig.StateReady {
		r.state = runtimeconfig.StateFaulted
	}
	r.mu.Unlock()
	if r.agent != nil {
		r.agent.StopExecution(fmt.Errorf("持久化失败: %w", r.firstFault()))
	}
}

// startClose 保留登记和所有权，直到执行、收尾及管理操作全部退出。
func (r *Runtime) startClose() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.state = runtimeconfig.StateClosing
		r.mu.Unlock()
		r.agent.RequestClose()
		go func() {
			err := r.agent.Close()
			<-r.driverDone
			// 等待仍在读取或管理的调用退出后释放会话所有权。
			r.control <- struct{}{}
			err = errors.Join(err, r.owner.release())
			r.mu.Lock()
			r.closeErr = err
			r.state = runtimeconfig.StateClosed
			r.mu.Unlock()
			<-r.control
			close(r.done)
		}()
	})
}

// Close 的 context 只控制等待，不取消后台必要收尾。
func (r *Runtime) Close(ctx context.Context) error {
	r.startClose()
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
