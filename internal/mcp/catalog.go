package mcp

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolKey 用服务器来源和原工具名共同标识工具，不拼接名称。
type ToolKey struct {
	ServerID string `json:"serverID"`
	Name     string `json:"name"`
}

// Definition 将复制的工具定义绑定到连接代次和目录版本。
// 重连或定义变化后应重建适配器；调用期间不能修改定义。
type Definition struct {
	Key                  ToolKey
	Tool                 *Tool
	Generation, Revision uint64
	UnsupportedReason    string
}

// Tool 和 CallToolResult 保留 SDK 原生模型，包括 schema、注解、多类型内容、
// StructuredContent、IsError 和元数据；业务层自行决定如何适配。
type Tool = sdk.Tool

type CallToolResult = sdk.CallToolResult

// Candidate 只提供发现所需的简短信息；完整定义由 Manager.Definitions 取得。
type Candidate struct {
	Key         ToolKey `json:"key"`
	Description string  `json:"description,omitempty"`
	Score       int     `json:"score"`
}

// MaxCatalogResults 是 Manager.Search 的结果上限；不设置隐式默认值。
const MaxCatalogResults = 100

type catalogEntry struct {
	definition  []byte
	description string
	nameLower   string
	nameWords   []string
	descLower   string
}

// toolDirectory 是 serverEntry 的单服务器目录，由 Manager 的锁保护，仅以工具名索引。
type toolDirectory map[string]catalogEntry

// get 返回独立工具定义；调用方修改返回值不会改变目录。
func (d toolDirectory) get(name string) (*Tool, error) {
	entry, exists := d[name]
	if !exists {
		return nil, ErrToolNotFound
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

// newToolDirectory 构建独立 JSON 快照，拒绝空名、重名或无法序列化的定义。
// 发布前验证可按 SDK 模型读回；构建失败不改变现有目录。
func newToolDirectory(serverID string, tools []*Tool) (toolDirectory, error) {
	snapshot := make(toolDirectory, len(tools))
	for i, tool := range tools {
		if tool == nil || strings.TrimSpace(tool.Name) == "" || !utf8.ValidString(tool.Name) {
			return nil, inputError(serverID, "catalog/update", "MCP 目录服务器 %q 的第 %d 个工具为空或原名不是非空 UTF-8 字符串", serverID, i)
		}
		if _, exists := snapshot[tool.Name]; exists {
			return nil, inputError(serverID, "catalog/update", "MCP 目录服务器 %q 重复工具原名 %q", serverID, tool.Name)
		}
		definition, err := json.Marshal(tool)
		if err != nil {
			return nil, inputError(serverID, "catalog/update", "复制 MCP 工具 %q/%q: %w", serverID, tool.Name, err)
		}
		// 发布前确认定义能够按 SDK 原生模型读回，失败不影响旧快照。
		copied, err := decodeCatalogTool(definition)
		if err != nil {
			return nil, inputError(serverID, "catalog/update", "读取 MCP 工具 %q/%q 副本: %w", serverID, tool.Name, err)
		}
		nameLower := strings.ToLower(copied.Name)
		snapshot[copied.Name] = catalogEntry{
			definition: definition, description: copied.Description,
			nameLower: nameLower, nameWords: catalogWords(nameLower),
			descLower: strings.ToLower(copied.Description),
		}
	}
	return snapshot, nil
}

func (d toolDirectory) candidates(candidates []Candidate, serverID, query string, words []string) []Candidate {
	source := strings.ToLower(serverID)
	for name, entry := range d {
		score := catalogScore(entry, source, query, words)
		if query != "" && score == 0 {
			continue
		}
		candidates = append(candidates, Candidate{
			Key:         ToolKey{ServerID: serverID, Name: name},
			Description: catalogDescription(entry.description), Score: score,
		})
	}
	return candidates
}

func sortCandidates(candidates []Candidate) {
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
}
