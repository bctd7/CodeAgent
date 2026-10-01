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

type instance struct {
	ready   chan struct{}
	runtime *Runtime
	err     error
}

// Manager 的登记锁只保护生命周期与 map，不覆盖 I/O 或等待。
type Manager struct {
	mu         sync.Mutex
	config     runtimeconfig.Config
	database   string
	store      *store.Store
	service    *session.Service
	instances  map[string]*instance
	closing    bool
	operations sync.WaitGroup
	closeOnce  sync.Once
	done       chan struct{}
	closeErr   error
}

func NewManager(ctx context.Context, cfg runtimeconfig.Config) (*Manager, error) {
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
	db, err := store.Open(ctx, path)
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
	return &Manager{config: cfg, database: path, store: db, service: session.NewService(db), instances: make(map[string]*instance), done: make(chan struct{})}, nil
}

func (m *Manager) beginOperation() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return runtimeconfig.ErrClosing
	}
	m.operations.Add(1)
	return nil
}

func (m *Manager) resolveModel(selection sessionconfig.ModelSelection) (modelconfig.ModelEntry, error) {
	if selection.Ref == "" {
		return modelconfig.ModelEntry{}, runtimeconfig.ErrModelRequired
	}
	for _, entry := range m.config.Models.ModelEntries() {
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

func (m *Manager) CreateSession(ctx context.Context, opts runtimeconfig.CreateSessionOptions) (sessionconfig.SessionInfo, error) {
	if err := m.beginOperation(); err != nil {
		return sessionconfig.SessionInfo{}, err
	}
	defer m.operations.Done()
	if _, err := m.resolveModel(opts.Model); err != nil {
		return sessionconfig.SessionInfo{}, err
	}
	return m.service.CreateSessionWithModel(ctx, opts.Title, opts.WorkingDir, opts.Model)
}

func (m *Manager) ListSessions(ctx context.Context, opts sessionconfig.SessionListOptions) ([]sessionconfig.SessionInfo, error) {
	if err := m.beginOperation(); err != nil {
		return nil, err
	}
	defer m.operations.Done()
	return m.service.ListSessions(ctx, opts)
}

func (m *Manager) OpenSession(ctx context.Context, id string, opts runtimeconfig.OpenSessionOptions) (*Runtime, error) {
	if err := m.beginOperation(); err != nil {
		return nil, err
	}
	defer m.operations.Done()
	m.mu.Lock()
	if old := m.instances[id]; old != nil {
		m.mu.Unlock()
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
			m.mu.Lock()
			if m.instances[id] == old {
				delete(m.instances, id)
			}
			m.mu.Unlock()
			return m.OpenSession(ctx, id, opts)
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
	slot := &instance{ready: make(chan struct{})}
	m.instances[id] = slot
	m.mu.Unlock()

	r, err := m.initialize(ctx, id, opts)
	m.mu.Lock()
	slot.runtime, slot.err = r, err
	if err != nil {
		delete(m.instances, id)
	}
	close(slot.ready)
	m.mu.Unlock()
	return r, err
}

func (m *Manager) initialize(ctx context.Context, id string, opts runtimeconfig.OpenSessionOptions) (_ *Runtime, err error) {
	owner, err := acquireOwnership(ctx, m.database, id)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = owner.release()
		}
	}()
	info, err := m.service.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if info.ArchivedAt != nil {
		return nil, sessionconfig.ErrSessionArchived
	}
	if err := session.ValidateSessionWorkingDir(info); err != nil {
		return nil, err
	}
	unfinished, err := m.service.ListUnfinishedRuns(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, run := range unfinished {
		if err := m.service.MarkRunAborted(ctx, run.ID, "程序中断"); err != nil {
			return nil, err
		}
	}
	s, err := m.service.OpenSession(ctx, id)
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
	entry, err := m.resolveModel(*selection)
	if err != nil {
		return nil, err
	}
	if opts.Model != nil {
		if err := m.service.SetModelSelection(ctx, id, *selection); err != nil {
			return nil, err
		}
		v := *selection
		info.ModelSelection = &v
	}
	r := &Runtime{manager: m, session: s, owner: owner, info: info, state: runtimeconfig.StateInitializing, control: make(chan struct{}, 1), done: make(chan struct{})}
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
	r.agent, err = agent.New(agent.Config{Model: entry.Model, Options: entry.Options, SystemPrompt: m.config.SystemPrompt,
		InitialHistory: messages, InitialOrigins: origins, NewRunID: uuid.NewString,
		Stream: m.config.Stream, Tools: tools, MaxParallelTools: m.config.MaxParallelTools,
		OnEvent: func(event agent.Event) {
			if m.config.OnEvent != nil {
				m.config.OnEvent(id, event)
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
func (m *Manager) manageSession(ctx context.Context, id string, idle bool, fn func() error, archive bool) error {
	if err := m.beginOperation(); err != nil {
		return err
	}
	defer m.operations.Done()
	m.mu.Lock()
	slot := m.instances[id]
	m.mu.Unlock()
	if slot != nil {
		select {
		case <-slot.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if slot.err != nil {
			return slot.err
		}
		if slot.runtime.Snapshot().State != runtimeconfig.StateClosed {
			r := slot.runtime
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
	owner, err := acquireOwnership(ctx, m.database, id)
	if err != nil {
		return err
	}
	defer owner.release()
	return fn()
}

func (m *Manager) RenameSession(ctx context.Context, id, title string) error {
	return m.manageSession(ctx, id, false, func() error { return m.service.RenameSession(ctx, id, title) }, false)
}
func (m *Manager) ArchiveSession(ctx context.Context, id string) error {
	return m.manageSession(ctx, id, true, func() error { return m.service.ArchiveSession(ctx, id) }, true)
}
func (m *Manager) UnarchiveSession(ctx context.Context, id string) error {
	return m.manageSession(ctx, id, false, func() error { return m.service.UnarchiveSession(ctx, id) }, false)
}

func (m *Manager) CloseSession(ctx context.Context, id string) error {
	if err := m.beginOperation(); err != nil {
		return err
	}
	m.mu.Lock()
	slot := m.instances[id]
	m.mu.Unlock()
	// 等待实例收尾不阻挡 Manager 向其他实例发出关闭请求。
	m.operations.Done()
	if slot == nil {
		return nil
	}
	select {
	case <-slot.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	if slot.err != nil {
		return slot.err
	}
	return slot.runtime.Close(ctx)
}

// Close 并行请求全部实例收尾；超时只结束本次等待，数据库保持可用直至所有收尾退出。
func (m *Manager) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closing = true
		existing := make([]*Runtime, 0, len(m.instances))
		for _, slot := range m.instances {
			if slot.runtime != nil {
				existing = append(existing, slot.runtime)
			}
		}
		m.mu.Unlock()
		go func() {
			// 已有实例先停止执行，不被其他会话仍在进行的初始化或管理操作阻挡。
			for _, r := range existing {
				r.startClose()
			}
			m.operations.Wait()
			m.mu.Lock()
			instances := make([]*Runtime, 0, len(m.instances))
			for _, slot := range m.instances {
				if slot.runtime != nil {
					instances = append(instances, slot.runtime)
				}
			}
			m.mu.Unlock()
			var wg sync.WaitGroup
			var errorMu sync.Mutex
			var closeErr error
			for _, r := range instances {
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
			closeErr = errors.Join(closeErr, m.store.Close())
			m.mu.Lock()
			m.closeErr = closeErr
			m.mu.Unlock()
			close(m.done)
		}()
	})
	select {
	case <-m.done:
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
