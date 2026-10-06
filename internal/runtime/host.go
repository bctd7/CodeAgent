package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"

	"CodeAgent/internal/agent"
	modelconfig "CodeAgent/internal/config"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/provider"
	runtimeconfig "CodeAgent/internal/runtime/config"
	"CodeAgent/internal/session"
	sessionconfig "CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"

	"github.com/google/uuid"
)

// runtimeEntry 在 Host 注册表中记录初始化中或已打开的 Runtime。
type runtimeEntry struct {
	ready   chan struct{}
	runtime *Runtime
	err     error
}

// Host 的登记锁只保护生命周期与 map，不覆盖 I/O 或等待。
type Host struct {
	mu         sync.Mutex
	config     runtimeconfig.Config // DatabasePath 在 Host 中保存规范化后的路径。
	store      *store.Store
	service    *session.Service
	runtimes   map[string]*runtimeEntry
	closing    bool
	operations sync.WaitGroup
	closeOnce  sync.Once
	done       chan struct{}
	closeErr   error
}

func NewHost(ctx context.Context, cfg runtimeconfig.Config) (*Host, error) {
	if cfg.Models == nil {
		return nil, fmt.Errorf("模型配置不能为空")
	}
	if err := cfg.Models.Validate(); err != nil {
		return nil, err
	}
	if cfg.RecordTimeout < 0 || cfg.MaxParallelTools < 0 {
		return nil, fmt.Errorf("运行限制不能为负数")
	}
	if cfg.RecordTimeout == 0 {
		cfg.RecordTimeout = defaultRecordTimeout
	}
	path, err := canonicalDatabasePath(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	cfg.DatabasePath = path
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := db.Init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if cfg.Stream == nil {
		cfg.Stream = provider.Stream
	}
	return &Host{config: cfg, store: db, service: session.NewService(db), runtimes: make(map[string]*runtimeEntry), done: make(chan struct{})}, nil
}

func (h *Host) beginOperation() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return runtimeconfig.ErrClosing
	}
	h.operations.Add(1)
	return nil
}

func (h *Host) resolveModel(selection sessionconfig.ModelSelection) (modelconfig.ModelEntry, error) {
	if selection.Ref == "" {
		return modelconfig.ModelEntry{}, runtimeconfig.ErrModelRequired
	}
	for _, entry := range h.config.Models.ModelEntries() {
		if entry.Ref != selection.Ref {
			continue
		}
		// 地址会作为来源持久化；凭据仅允许放在 APIKey/Headers 中。
		u, err := url.Parse(entry.Model.BaseURL)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return modelconfig.ModelEntry{}, fmt.Errorf("模型 %s 的地址无效或含不能持久化的凭据参数", selection.Ref)
		}
		return entry, nil
	}
	return modelconfig.ModelEntry{}, fmt.Errorf("模型配置引用 %q 不存在", selection.Ref)
}

func (h *Host) CreateSession(ctx context.Context, opts runtimeconfig.CreateSessionOptions) (sessionconfig.SessionInfo, error) {
	if err := h.beginOperation(); err != nil {
		return sessionconfig.SessionInfo{}, err
	}
	defer h.operations.Done()
	if _, err := h.resolveModel(opts.Model); err != nil {
		return sessionconfig.SessionInfo{}, err
	}
	return h.service.CreateSessionWithModel(ctx, opts.Title, opts.WorkingDir, opts.Model)
}

func (h *Host) ListSessions(ctx context.Context, opts sessionconfig.SessionListOptions) ([]sessionconfig.SessionInfo, error) {
	if err := h.beginOperation(); err != nil {
		return nil, err
	}
	defer h.operations.Done()
	return h.service.ListSessions(ctx, opts)
}

func (h *Host) OpenSession(ctx context.Context, id string, opts runtimeconfig.OpenSessionOptions) (*Runtime, error) {
	if err := h.beginOperation(); err != nil {
		return nil, err
	}
	defer h.operations.Done()
	h.mu.Lock()
	if old := h.runtimes[id]; old != nil {
		h.mu.Unlock()
		select {
		case <-old.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if old.err != nil {
			return nil, old.err
		}
		state := old.runtime.Snapshot().State
		if state == runtimeconfig.StateClosed {
			h.mu.Lock()
			if h.runtimes[id] == old {
				delete(h.runtimes, id)
			}
			h.mu.Unlock()
			return h.OpenSession(ctx, id, opts)
		}
		if err := old.runtime.admission(); err != nil {
			return nil, err
		}
		if opts.Model != nil {
			if err := old.runtime.SetModel(ctx, *opts.Model); err != nil {
				return nil, err
			}
		}
		return old.runtime, nil
	}
	entry := &runtimeEntry{ready: make(chan struct{})}
	h.runtimes[id] = entry
	h.mu.Unlock()

	r, err := h.initialize(ctx, id, opts)
	h.mu.Lock()
	entry.runtime, entry.err = r, err
	if err != nil {
		delete(h.runtimes, id)
	}
	close(entry.ready)
	h.mu.Unlock()
	return r, err
}

func (h *Host) initialize(ctx context.Context, id string, opts runtimeconfig.OpenSessionOptions) (_ *Runtime, err error) {
	owner, err := acquireOwnership(ctx, h.config.DatabasePath, id)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = owner.release()
		}
	}()
	info, err := h.service.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if info.ArchivedAt != nil {
		return nil, sessionconfig.ErrSessionArchived
	}
	if err := session.ValidateSessionWorkingDir(info); err != nil {
		return nil, err
	}
	unfinished, err := h.service.ListUnfinishedRuns(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, run := range unfinished {
		if err := h.service.MarkRunAborted(ctx, run.ID, "程序中断"); err != nil {
			return nil, err
		}
	}
	s, err := h.service.OpenSession(ctx, id)
	if err != nil {
		return nil, err
	}
	history, err := s.GetCurrentHistory(ctx)
	if err != nil {
		return nil, err
	}
	selection := info.ModelSelection
	if opts.Model != nil {
		selection = opts.Model
	}
	if selection == nil {
		return nil, runtimeconfig.ErrModelRequired
	}
	entry, err := h.resolveModel(*selection)
	if err != nil {
		return nil, err
	}
	if opts.Model != nil {
		if err := h.service.SetModelSelection(ctx, id, *selection); err != nil {
			return nil, err
		}
		v := *selection
		info.ModelSelection = &v
	}
	r := &Runtime{host: h, session: s, owner: owner, info: info, state: runtimeconfig.StateInitializing, control: make(chan struct{}, 1), done: make(chan struct{})}
	tools, err := r.assembleTools()
	if err != nil {
		return nil, err
	}
	messages := make([]llm.Message, 0, len(history))
	origins := make(map[*llm.AssistantMessage]agent.ModelOrigin)
	for _, e := range history {
		messages = append(messages, e.Message)
		if assistant, ok := e.Message.(*llm.AssistantMessage); ok && e.Origin != nil {
			origins[assistant] = agent.ModelOrigin{Provider: e.Origin.Provider, API: e.Origin.API, ModelID: e.Origin.ModelID, BaseURL: e.Origin.BaseURL}
		}
	}
	r.agent, err = agent.New(agent.Config{Model: entry.Model, Options: entry.Options, SystemPrompt: h.config.SystemPrompt,
		InitialHistory: messages, InitialOrigins: origins, NewRunID: uuid.NewString,
		Stream: h.config.Stream, Tools: tools, MaxParallelTools: h.config.MaxParallelTools,
		OnEvent: func(event agent.Event) {
			if h.config.OnEvent != nil {
				h.config.OnEvent(id, event)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		r.agent.Close()
		return nil, err
	}
	r.driver, err = r.agent.Driver()
	if err != nil {
		r.agent.Close()
		return nil, err
	}
	r.driverDone = make(chan struct{})
	r.state = runtimeconfig.StateReady
	go r.drive()
	ok = true
	return r, nil
}

// 管理未打开的会话只取得短锁；已有拥有者协调同一个实例。
func (h *Host) manageSession(ctx context.Context, id string, idle bool, fn func() error, archive bool) error {
	if err := h.beginOperation(); err != nil {
		return err
	}
	defer h.operations.Done()
	h.mu.Lock()
	entry := h.runtimes[id]
	h.mu.Unlock()
	if entry != nil {
		select {
		case <-entry.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if entry.err != nil {
			return entry.err
		}
		if entry.runtime.Snapshot().State != runtimeconfig.StateClosed {
			r := entry.runtime
			err := r.manage(ctx, idle, func() error {
				if err := fn(); err != nil {
					return err
				}
				if archive {
					r.startClose()
				}
				return nil
			})
			if err != nil {
				return err
			}
			if archive {
				return r.Close(ctx)
			}
			return nil
		}
	}
	owner, err := acquireOwnership(ctx, h.config.DatabasePath, id)
	if err != nil {
		return err
	}
	defer owner.release()
	return fn()
}

func (h *Host) RenameSession(ctx context.Context, id, title string) error {
	return h.manageSession(ctx, id, false, func() error { return h.service.RenameSession(ctx, id, title) }, false)
}
func (h *Host) ArchiveSession(ctx context.Context, id string) error {
	return h.manageSession(ctx, id, true, func() error { return h.service.ArchiveSession(ctx, id) }, true)
}
func (h *Host) UnarchiveSession(ctx context.Context, id string) error {
	return h.manageSession(ctx, id, false, func() error { return h.service.UnarchiveSession(ctx, id) }, false)
}

func (h *Host) CloseSession(ctx context.Context, id string) error {
	if err := h.beginOperation(); err != nil {
		return err
	}
	h.mu.Lock()
	entry := h.runtimes[id]
	h.mu.Unlock()
	// 等待实例收尾不阻挡 Host 向其他实例发出关闭请求。
	h.operations.Done()
	if entry == nil {
		return nil
	}
	select {
	case <-entry.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	if entry.err != nil {
		return entry.err
	}
	return entry.runtime.Close(ctx)
}

// Close 并行请求全部实例收尾；超时只结束本次等待，数据库保持可用直至所有收尾退出。
func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closing = true
		existing := make([]*Runtime, 0, len(h.runtimes))
		for _, entry := range h.runtimes {
			if entry.runtime != nil {
				existing = append(existing, entry.runtime)
			}
		}
		h.mu.Unlock()
		go func() {
			// 已有实例先停止执行，不被其他会话仍在进行的初始化或管理操作阻挡。
			for _, r := range existing {
				r.startClose()
			}
			h.operations.Wait()
			h.mu.Lock()
			runtimes := make([]*Runtime, 0, len(h.runtimes))
			for _, entry := range h.runtimes {
				if entry.runtime != nil {
					runtimes = append(runtimes, entry.runtime)
				}
			}
			h.mu.Unlock()
			var wg sync.WaitGroup
			var errorMu sync.Mutex
			var closeErr error
			for _, r := range runtimes {
				wg.Add(1)
				go func(r *Runtime) {
					defer wg.Done()
					err := r.Close(context.Background())
					errorMu.Lock()
					closeErr = errors.Join(closeErr, err)
					errorMu.Unlock()
				}(r)
			}
			wg.Wait()
			closeErr = errors.Join(closeErr, h.store.Close())
			h.mu.Lock()
			h.closeErr = closeErr
			h.mu.Unlock()
			close(h.done)
		}()
	})
	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
