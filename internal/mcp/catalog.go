package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ToolKey 用服务器来源和原工具名共同标识工具，不拼接名称。
type ToolKey struct {
	ServerID string `json:"serverID"`
	Name     string `json:"name"`
}

// Candidate 只提供发现所需的简短信息；完整定义由 Get 单独取得。
type Candidate struct {
	Key         ToolKey `json:"key"`
	Description string  `json:"description,omitempty"`
	Score       int     `json:"score"`
}

// MaxCatalogResults 是单次目录检索的结果上限；Search 不设置隐式默认值。
const MaxCatalogResults = 100

// ErrToolNotFound 表示当前目录中不存在指定的结构化键。
var ErrToolNotFound = errors.New("MCP 目录中不存在指定工具")

type catalogEntry struct {
	definition  []byte
	description string
	nameLower   string
	nameWords   []string
	descLower   string
}

// Catalog 保存调用方显式提交的工具快照，不联网、不执行工具、不管理连接。
// 零值可用，方法可并发调用；使用后不得复制。调用 Update 时不能并发修改输入。
type Catalog struct {
	mu      sync.RWMutex
	servers map[string]map[string]catalogEntry
}

// Update 原子替换一个服务器的整份快照，空列表表示清空该服务器工具。
// 空来源、空工具名、同服务器重名或不能序列化的定义都会拒绝整次更新。
// 完整定义保存为独立 JSON 副本，不保留输入指针或自定义 Go schema 类型。
func (c *Catalog) Update(serverID string, tools []*Tool) error {
	if strings.TrimSpace(serverID) == "" || !utf8.ValidString(serverID) {
		return errors.New("MCP 目录服务器 ID 必须为非空 UTF-8 字符串")
	}
	snapshot := make(map[string]catalogEntry, len(tools))
	for i, tool := range tools {
		if tool == nil || strings.TrimSpace(tool.Name) == "" || !utf8.ValidString(tool.Name) {
			return fmt.Errorf("MCP 目录服务器 %q 的第 %d 个工具为空或原名不是非空 UTF-8 字符串", serverID, i)
		}
		if _, exists := snapshot[tool.Name]; exists {
			return fmt.Errorf("MCP 目录服务器 %q 重复工具原名 %q", serverID, tool.Name)
		}
		definition, err := json.Marshal(tool)
		if err != nil {
			return fmt.Errorf("复制 MCP 工具 %q/%q: %w", serverID, tool.Name, err)
		}
		// 发布前确认定义能够按 SDK 原生模型读回，失败不影响旧快照。
		copied, err := decodeCatalogTool(definition)
		if err != nil {
			return fmt.Errorf("读取 MCP 工具 %q/%q 副本: %w", serverID, tool.Name, err)
		}
		nameLower := strings.ToLower(copied.Name)
		snapshot[copied.Name] = catalogEntry{
			definition: definition, description: copied.Description,
			nameLower: nameLower, nameWords: catalogWords(nameLower),
			descLower: strings.ToLower(copied.Description),
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.servers == nil {
		c.servers = make(map[string]map[string]catalogEntry)
	}
	c.servers[serverID] = snapshot
	return nil
}

// Remove 移除服务器快照；不存在的来源不产生错误。
func (c *Catalog) Remove(serverID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.servers, serverID)
}

// Get 按稳定键返回独立的 SDK 工具定义，包括 schema、注解、图标和元数据。
// schema 和元数据采用 JSON 原生值，数字使用 json.Number 避免额外精度损失。
// 未找到时返回 ErrToolNotFound；调用方修改返回值不会改变目录。
func (c *Catalog) Get(key ToolKey) (*Tool, error) {
	c.mu.RLock()
	entry, exists := c.servers[key.ServerID][key.Name]
	c.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("MCP 工具 %q/%q: %w", key.ServerID, key.Name, ErrToolNotFound)
	}
	return decodeCatalogTool(entry.definition)
}

func decodeCatalogTool(definition []byte) (*Tool, error) {
	var tool Tool
	decoder := json.NewDecoder(bytes.NewReader(definition))
	decoder.UseNumber()
	if err := decoder.Decode(&tool); err != nil {
		return nil, err
	}
	return &tool, nil
}

// Search 只检索名称、简介和来源，忽略大小写；limit 必须为 1..MaxCatalogResults。
// 空查询列出目录；非空查询按关键词任一命中选取，连续中文支持子串匹配。
// 名称精确、前缀、分词、子串依次优先，随后是简介、来源；同分按来源和原名排序。
// 候选简介最多 200 个 Unicode 字符，索引仍使用完整简介，结果不含 schema。
func (c *Catalog) Search(query string, limit int) ([]Candidate, error) {
	if limit < 1 || limit > MaxCatalogResults {
		return nil, fmt.Errorf("MCP 目录 limit 必须为 1..%d", MaxCatalogResults)
	}
	query = strings.ToLower(strings.TrimSpace(query))
	words := catalogWords(query)
	candidates := make([]Candidate, 0)
	c.mu.RLock()
	for serverID, snapshot := range c.servers {
		source := strings.ToLower(serverID)
		for name, entry := range snapshot {
			score := catalogScore(entry, source, query, words)
			if query != "" && score == 0 {
				continue
			}
			candidates = append(candidates, Candidate{
				Key:         ToolKey{ServerID: serverID, Name: name},
				Description: catalogDescription(entry.description), Score: score,
			})
		}
	}
	c.mu.RUnlock()
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if left.Key.ServerID != right.Key.ServerID {
			return left.Key.ServerID < right.Key.ServerID
		}
		return left.Key.Name < right.Key.Name
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

func catalogWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// 只取最高命中等级，避免长简介或重复关键词淹没名称的优先级。
func catalogScore(entry catalogEntry, source, query string, words []string) int {
	if query == "" {
		return 0
	}
	if entry.nameLower == query {
		return 600
	}
	if strings.HasPrefix(entry.nameLower, query) {
		return 500
	}
	score := 0
	for _, word := range words {
		for _, nameWord := range entry.nameWords {
			if nameWord == word {
				return 400
			}
		}
		if strings.Contains(entry.nameLower, word) {
			score = max(score, 300)
		} else if strings.Contains(entry.descLower, word) {
			score = max(score, 200)
		} else if strings.Contains(source, word) {
			score = max(score, 100)
		}
	}
	return score
}

func catalogDescription(description string) string {
	characters := []rune(description)
	if len(characters) > 200 {
		return string(characters[:199]) + "…"
	}
	return description
}

// Keys 枚举完整本地目录，不受 Search 的结果数量限制。
// 不发现远程工具，也不返回工具定义。
func (c *Catalog) Keys() []ToolKey {
	c.mu.RLock()
	keys := make([]ToolKey, 0)
	for serverID, snapshot := range c.servers {
		for name := range snapshot {
			keys = append(keys, ToolKey{serverID, name})
		}
	}
	c.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ServerID != keys[j].ServerID {
			return keys[i].ServerID < keys[j].ServerID
		}
		return keys[i].Name < keys[j].Name
	})
	return keys
}
