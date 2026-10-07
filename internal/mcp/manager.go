package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type ServerState string

const (
	ServerDisabled     ServerState = "disabled"
	ServerConnecting   ServerState = "connecting"
	ServerReady        ServerState = "ready"
	ServerFailed       ServerState = "failed"
	ServerDisconnected ServerState = "disconnected"
	ServerClosed       ServerState = "closed"
)

// ServerStatus 不包含端点、请求头、环境变量和原始错误。
type ServerStatus struct {
	ID                   string
	State                ServerState
	Generation, Revision uint64
	ToolCount            int
	Error                string
}

// Definition 将复制的工具定义绑定到连接代次和目录版本。
// 重连或定义变化后应重建适配器；调用期间不能修改定义。
type Definition struct {
	Key                  ToolKey
	Tool                 *Tool
	Generation, Revision uint64
	UnsupportedReason    string
}

type serverEntry struct {
	spec        ServerConfig
	opGate      chan struct{} // 串行连接/刷新，等待可取消
	ctx         context.Context
	cancel      context.CancelFunc
	client      *Client
	watchDone   chan struct{}
	status      ServerStatus
	fingerprint [32]byte
	headerTools map[string]bool // 整代保留扩展历史，SDK 过期页可能晚于刷新失效
}

// Manager 只管理明确配置的服务；Start/Reconnect 不重放工具调用。
// 管理器独占消费各 Client.ToolsChanged 通道；Catalog 仅负责本地存储。
type Manager struct {
	mu              sync.RWMutex
	entries         map[string]*serverEntry
	catalog         Catalog
	ctx             context.Context
	cancel          context.CancelFunc
	started, closed bool
	wg              sync.WaitGroup
	closeOnce       sync.Once
	closeErr        error
}

// NewManager 先校验再补默认值；共享配置在管理器使用期间须由双方保持只读。
func NewManager(specs []ServerConfig) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{entries: make(map[string]*serverEntry), ctx: ctx, cancel: cancel}
	for _, spec := range specs {
		if err := spec.Validate(); err != nil {
			cancel()
			return nil, err
		}
		if _, exists := m.entries[spec.ID]; exists {
			cancel()
			return nil, fmt.Errorf("duplicate MCP server ID %q", spec.ID)
		}
		life, stop := context.WithCancel(ctx)
		state := ServerDisabled
		if spec.Enabled {
			state = ServerDisconnected
		}
		m.entries[spec.ID] = &serverEntry{spec: spec.withDefaults(), opGate: make(chan struct{}, 1), ctx: life, cancel: stop, status: ServerStatus{ID: spec.ID, State: state}}
	}
	return m, nil
}

// Start 独立尝试所有启用服务，保留部分成功的连接。
// 返回聚合错误时应检查 Statuses，已成功服务仍可使用。
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if m.started {
		m.mu.Unlock()
		return errors.New("MCP manager already started; use explicit Reconnect")
	}
	m.started = true
	var entries []*serverEntry
	for _, e := range m.entries {
		if e.spec.Enabled && e.status.State != ServerClosed {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].spec.ID < entries[j].spec.ID })
	m.wg.Add(len(entries))
	m.mu.Unlock()
	results := make([]error, len(entries))
	var starts sync.WaitGroup
	starts.Add(len(entries))
	for i, e := range entries {
		go func(i int, e *serverEntry) {
			defer starts.Done()
			defer m.wg.Done()
			results[i] = m.connectServer(ctx, e, false)
		}(i, e)
	}
	starts.Wait()
	return errors.Join(results...)
}

func (m *Manager) beginOperation(id string) (*serverEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	e := m.entries[id]
	if e == nil || !e.spec.Enabled {
		return nil, ErrServerUnavailable
	}
	m.wg.Add(1)
	return e, nil
}

// Reconnect 显式替换指定服务的连接，不重放任何未完成调用。
func (m *Manager) Reconnect(ctx context.Context, id string) error {
	e, err := m.beginOperation(id)
	if err != nil {
		return err
	}
	defer m.wg.Done()
	return m.connectServer(ctx, e, true)
}

func (m *Manager) connectServer(ctx context.Context, e *serverEntry, reopen bool) error {
	if err := m.lockOperation(ctx, e); err != nil {
		return &ServerError{e.spec.ID, "connect", err}
	}
	defer func() { <-e.opGate }()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if e.status.State == ServerClosed && !reopen {
		m.mu.Unlock()
		return ErrServerUnavailable
	}
	old := e.client
	e.cancel()
	e.client = nil
	e.watchDone = nil
	e.status.Generation++
	generation := e.status.Generation
	e.status.State = ServerConnecting
	e.status.ToolCount = 0
	e.status.Error = ""
	e.headerTools = make(map[string]bool)
	m.catalog.Remove(e.spec.ID)
	e.ctx, e.cancel = context.WithCancel(m.ctx)
	life := e.ctx
	m.mu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			m.failAttempt(e, generation, err)
			return &ServerError{e.spec.ID, "close previous connection", err}
		}
	}
	// 此处不等待旧 watcher，避免它等待操作门产生死锁；代次检查使其退出。
	operation, cleanup := requestContext(ctx, life)
	defer cleanup()
	dialCtx, stop := context.WithTimeout(operation, e.spec.ConnectTimeout)
	var c *Client
	var err error
	if e.spec.Transport == "stdio" {
		c, err = ConnectStdio(dialCtx, *e.spec.Stdio)
	} else {
		c, err = ConnectHTTP(dialCtx, *e.spec.HTTP)
	}
	stop()
	if err != nil {
		m.failAttempt(e, generation, err)
		return &ServerError{e.spec.ID, "connect", err}
	}
	listCtx, stopList := context.WithTimeout(operation, e.spec.RequestTimeout)
	tools, err := c.ListTools(listCtx)
	stopList()
	if err == nil {
		err = operation.Err()
	}
	m.mu.Lock()
	if m.closed || e.status.Generation != generation || life.Err() != nil {
		m.mu.Unlock()
		_ = c.Close()
		return &ServerError{e.spec.ID, "connect", context.Canceled}
	}
	if err == nil {
		err = m.publish(e, tools)
	}
	if err != nil {
		e.status.State = ServerFailed
		e.status.Error = safeError(err)
		m.mu.Unlock()
		_ = c.Close()
		return &ServerError{e.spec.ID, "discover", err}
	}
	e.client = c
	e.status.State = ServerReady
	done := make(chan struct{})
	e.watchDone = done
	m.wg.Add(1)
	go m.watch(e, c, generation, life, done)
	m.mu.Unlock()
	return nil
}

func (m *Manager) failAttempt(e *serverEntry, generation uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed && e.status.Generation == generation && e.ctx.Err() == nil {
		e.status.State = ServerFailed
		e.status.Error = safeError(err)
	}
}

func (m *Manager) Statuses() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	states := make([]ServerStatus, 0, len(m.entries))
	for _, e := range m.entries {
		states = append(states, e.status)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states
}

func (m *Manager) CallTool(ctx context.Context, definition Definition, args json.RawMessage) (*CallToolResult, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, ErrManagerClosed
	}
	e := m.entries[definition.Key.ServerID]
	if e == nil || e.client == nil || e.status.State != ServerReady {
		m.mu.RUnlock()
		return nil, ErrServerUnavailable
	}
	if definition.Generation != e.status.Generation || definition.Revision != e.status.Revision {
		m.mu.RUnlock()
		return nil, ErrStaleDefinition
	}
	if e.headerTools[definition.Key.Name] {
		m.mu.RUnlock()
		return nil, ErrUnsupportedTool
	}
	if _, err := m.catalog.Get(definition.Key); err != nil {
		m.mu.RUnlock()
		return nil, err
	}
	c, life, timeout := e.client, e.ctx, e.spec.RequestTimeout
	m.wg.Add(1)
	m.mu.RUnlock()
	defer m.wg.Done()
	operation, cleanup := requestContext(ctx, life)
	defer cleanup()
	operation, stop := context.WithTimeout(operation, timeout)
	defer stop()
	result, err := c.CallTool(operation, definition.Key.Name, args)
	if err != nil {
		return nil, &ServerError{definition.Key.ServerID, "tools/call", err}
	}
	return result, nil
}

func (m *Manager) CloseServer(id string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	e := m.entries[id]
	if e == nil {
		m.mu.Unlock()
		return ErrServerUnavailable
	}
	m.wg.Add(1)
	defer m.wg.Done()
	c, done := e.client, e.watchDone
	e.cancel()
	e.status.Generation++
	e.status.State = ServerClosed
	e.status.ToolCount = 0
	e.status.Error = ""
	e.client = nil
	e.watchDone = nil
	m.catalog.Remove(id)
	m.mu.Unlock()
	var err error
	if c != nil {
		err = c.Close()
	}
	e.opGate <- struct{}{}
	<-e.opGate
	if done != nil {
		<-done
	}
	if err != nil {
		return &ServerError{id, "close", err}
	}
	return nil
}

func (m *Manager) lockOperation(ctx context.Context, e *serverEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.ctx.Err(); err != nil {
		return ErrManagerClosed
	}
	select {
	case e.opGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return ErrManagerClosed
	}
}

func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		type closing struct {
			id     string
			client *Client
		}
		var clients []closing
		for _, e := range m.entries {
			e.cancel()
			e.status.Generation++
			e.status.State = ServerClosed
			e.status.ToolCount = 0
			e.status.Error = ""
			m.catalog.Remove(e.spec.ID)
			if e.client != nil {
				clients = append(clients, closing{e.spec.ID, e.client})
			}
			e.client = nil
		}
		m.mu.Unlock()
		var errs []error
		for _, c := range clients {
			if err := c.client.Close(); err != nil {
				errs = append(errs, &ServerError{c.id, "close", err})
			}
		}
		m.wg.Wait()
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}
