// Manager 的工具发现与刷新。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strings"
)

// publish 在持有 m.mu 且检查代次后替换该服务器目录。
func (m *Manager) publish(e *serverEntry, tools []*Tool) error {
	ordered := append([]*Tool(nil), tools...)
	for _, t := range ordered {
		if t == nil {
			return protocolError(e.spec.ID, "discover", "nil MCP tool")
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return operationError(e.spec.ID, "discover", err)
	}
	marked := make([]string, 0)
	for _, tool := range ordered {
		has, err := schemaHasHeader(tool.InputSchema)
		if err != nil {
			return operationError(e.spec.ID, "discover", err)
		}
		if has {
			marked = append(marked, tool.Name)
		}
	}
	directory, err := newToolDirectory(e.spec.ID, ordered)
	if err != nil {
		return err
	}
	e.tools = directory
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
				e.tools = nil
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
		return operationError(e.spec.ID, "refresh", err)
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
		e.tools = nil
		return operationError(e.spec.ID, "refresh", err)
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
	defs := make([]Definition, 0)
	for serverID, e := range m.entries {
		if e.status.State != ServerReady {
			continue
		}
		for name := range e.tools {
			tool, err := e.tools.get(name)
			if err != nil {
				return nil, operationError(serverID, "discover", err)
			}
			reason := ""
			if e.headerTools[name] {
				reason = ErrUnsupportedTool.Error()
			}
			defs = append(defs, Definition{Key: ToolKey{serverID, name}, Tool: tool, Generation: e.status.Generation, Revision: e.status.Revision, UnsupportedReason: reason})
		}
	}
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].Key.ServerID != defs[j].Key.ServerID {
			return defs[i].Key.ServerID < defs[j].Key.ServerID
		}
		return defs[i].Key.Name < defs[j].Key.Name
	})
	return defs, nil
}

// Search 汇总就绪服务器的本地目录，不请求远端或复制另一份目录。
// 只检索名称、简介和来源，忽略大小写；limit 必须为 1..MaxCatalogResults。
// 空查询列出目录；非空查询按关键词任一命中选取，连续中文支持子串匹配。
// 名称精确、前缀、分词、子串依次优先，随后是简介、来源；同分按来源和原名排序。
// 候选简介最多 200 个 Unicode 字符，索引仍使用完整简介，结果不含 schema。
func (m *Manager) Search(query string, limit int) ([]Candidate, error) {
	if limit < 1 || limit > MaxCatalogResults {
		return nil, inputError("", "catalog/search", "MCP 目录 limit 必须为 1..%d", MaxCatalogResults)
	}
	query = strings.ToLower(strings.TrimSpace(query))
	words := catalogWords(query)
	candidates := make([]Candidate, 0)
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, ErrManagerClosed
	}
	for serverID, e := range m.entries {
		if e.status.State == ServerReady {
			candidates = e.tools.candidates(candidates, serverID, query, words)
		}
	}
	m.mu.RUnlock()
	sortCandidates(candidates)
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}
