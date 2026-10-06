// Manager 的工具发现与刷新。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
)

// publish 在持有 m.mu 且检查代次后调用；Catalog 不承担连接职责。
func (m *Manager) publish(e *serverEntry, tools []*Tool) error {
	ordered := append([]*Tool(nil), tools...)
	for _, t := range ordered {
		if t == nil {
			return errors.New("nil MCP tool")
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return err
	}
	marked := make([]string, 0)
	for _, tool := range ordered {
		has, err := schemaHasHeader(tool.InputSchema)
		if err != nil {
			return err
		}
		if has {
			marked = append(marked, tool.Name)
		}
	}
	if err := m.catalog.Update(e.spec.ID, ordered); err != nil {
		return err
	}
	for _, name := range marked {
		e.headerTools[name] = true
	}
	fingerprint := sha256.Sum256(encoded)
	if e.status.Revision == 0 || fingerprint != e.fingerprint {
		e.status.Revision++
		e.fingerprint = fingerprint
	}
	e.status.ToolCount = len(tools)
	e.status.Error = ""
	return nil
}

func (m *Manager) watch(e *serverEntry, c *Client, generation uint64, life context.Context, done chan struct{}) {
	defer m.wg.Done()
	defer close(done)
	wait := make(chan error, 1)
	go func() { wait <- c.Wait() }()
	// 所有退出路径均等待 Wait 结束；显式关闭/重连会关闭管理的连接。
	for {
		select {
		case err := <-wait:
			m.mu.Lock()
			if !m.closed && e.status.Generation == generation && e.client == c {
				e.status.State = ServerDisconnected
				e.status.Error = safeError(err)
				e.status.ToolCount = 0
				m.catalog.Remove(e.spec.ID)
			}
			m.mu.Unlock()
			_ = c.Close()
			return
		case <-life.Done():
			<-wait
			return
		case <-c.Done():
			<-wait
			return
		case <-c.ToolsChanged():
			// 获取刷新操作锁前先检查代次，防止旧 watcher 刷新新客户端。
			_ = m.refresh(life, e, generation)
		}
	}
}

func (m *Manager) Refresh(ctx context.Context, id string) error {
	e, err := m.beginOperation(id)
	if err != nil {
		return err
	}
	defer m.wg.Done()
	return m.refresh(ctx, e, 0)
}

func (m *Manager) refresh(ctx context.Context, e *serverEntry, expected uint64) error {
	if err := m.lockOperation(ctx, e); err != nil {
		return &ServerError{e.spec.ID, "refresh", err}
	}
	defer func() { <-e.opGate }()
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return ErrManagerClosed
	}
	c, generation, life := e.client, e.status.Generation, e.ctx
	if c == nil || life.Err() != nil || expected != 0 && expected != generation || e.status.State == ServerDisconnected || e.status.State == ServerClosed {
		m.mu.RUnlock()
		return ErrServerUnavailable
	}
	m.mu.RUnlock()
	operation, cleanup := requestContext(ctx, life)
	defer cleanup()
	operation, stop := context.WithTimeout(operation, e.spec.RequestTimeout)
	defer stop()
	tools, err := c.ListTools(operation)
	if err == nil {
		err = operation.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || e.client != c || e.status.Generation != generation || life.Err() != nil {
		return ErrStaleDefinition
	}
	if err == nil {
		err = m.publish(e, tools)
	}
	if err != nil {
		e.status.State = ServerFailed
		e.status.Error = safeError(err)
		e.status.ToolCount = 0
		m.catalog.Remove(e.spec.ID)
		return &ServerError{e.spec.ID, "refresh", err}
	}
	e.status.State = ServerReady
	return nil
}

func (m *Manager) Definitions() ([]Definition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	keys := m.catalog.Keys()
	defs := make([]Definition, 0, len(keys))
	for _, key := range keys {
		e := m.entries[key.ServerID]
		if e.status.State != ServerReady {
			continue
		}
		tool, err := m.catalog.Get(key)
		if err != nil {
			return nil, err
		}
		reason := ""
		if e.headerTools[key.Name] {
			reason = ErrUnsupportedTool.Error()
		}
		defs = append(defs, Definition{Key: key, Tool: tool, Generation: e.status.Generation, Revision: e.status.Revision, UnsupportedReason: reason})
	}
	return defs, nil
}
