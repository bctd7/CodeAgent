package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"CodeAgent/internal/llm"
)

type toolArgs struct {
	Path      string   `json:"path,omitempty"`
	Query     string   `json:"query,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Type      string   `json:"type,omitempty"`
	Content   string   `json:"content,omitempty"`
	OldText   string   `json:"old_text,omitempty"`
	NewText   string   `json:"new_text,omitempty"`
	SourceIDs []string `json:"source_ids,omitempty"`
}

type toolDefinition struct {
	name, description string
	properties        map[string]string
	required          []string
}

// 同一份定义用于模型目录与执行校验，执行工具时不再重建、序列化、反解 schema。
var toolDefinitions = []toolDefinition{
	{"memory_list", "列出主题文件名", nil, nil},
	{"memory_read", "读取一个记忆文件", map[string]string{"path": "string"}, []string{"path"}},
	{"memory_search", "按字面关键词搜索主题，支持中文", map[string]string{"query": "string", "limit": "integer"}, []string{"query"}},
	{"memory_write", "暂存新主题或替换已经读取的主题正文", map[string]string{"path": "string", "summary": "string", "type": "string", "content": "string", "source_ids": "array"}, []string{"path", "summary", "type", "content", "source_ids"}},
	{"memory_edit", "精确替换已读取正文中的一处文本，重新提供摘要和来源", map[string]string{"path": "string", "summary": "string", "type": "string", "old_text": "string", "new_text": "string", "source_ids": "array"}, []string{"path", "summary", "type", "old_text", "new_text", "source_ids"}},
	{"memory_delete", "遗忘已经读取的主题文件", map[string]string{"path": "string", "source_ids": "array"}, []string{"path", "source_ids"}},
}

func catalog() []llm.Tool {
	tools := make([]llm.Tool, 0, len(toolDefinitions))
	for _, d := range toolDefinitions {
		properties := map[string]any{}
		for k, t := range d.properties {
			p := map[string]any{"type": t}
			if t == "array" {
				p["items"] = map[string]string{"type": "string"}
			}
			properties[k] = p
		}
		required := d.required
		if required == nil {
			required = []string{}
		}
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
		tools = append(tools, llm.Tool{Name: d.name, Description: d.description, Schema: schema})
	}
	return tools
}

func (s *Service) execute(ctx context.Context, call llm.ContentBlock, trusted map[string]bool, staged map[string]*string, readFiles map[string]bool, read func(string) (string, error), manifest func() []string) (any, error) {
	// 先匹配独立工具目录，再严格解码；未知工具没有任何执行路径。
	var definition *toolDefinition
	for i := range toolDefinitions {
		if toolDefinitions[i].name == call.Name {
			definition = &toolDefinitions[i]
			break
		}
	}
	if definition == nil {
		return nil, fmt.Errorf("tool not allowed")
	}
	encoded, err := json.Marshal(call.Arguments)
	if err != nil {
		return nil, err
	}
	for key := range call.Arguments {
		if _, ok := definition.properties[key]; !ok {
			return nil, fmt.Errorf("unknown argument %q", key)
		}
	}
	for _, key := range definition.required {
		if value, ok := call.Arguments[key]; !ok || value == nil {
			return nil, fmt.Errorf("missing argument %q", key)
		}
	}
	var args toolArgs
	if err := json.Unmarshal(encoded, &args); err != nil {
		return nil, err
	}
	switch call.Name {
	case "memory_list":
		return manifest(), nil
	case "memory_read":
		text, err := read(args.Path)
		if err != nil {
			return nil, err
		}
		readFiles[args.Path] = true
		return Document{Path: args.Path, Content: text}, nil
	case "memory_search":
		return search(ctx, manifest(), args.Query, args.Limit, read)
	}
	if err := validPath(args.Path); err != nil {
		return nil, err
	}
	if args.Path == indexName {
		return nil, os.ErrPermission
	}
	if len(args.SourceIDs) == 0 {
		return nil, fmt.Errorf("source_ids required")
	}
	for _, id := range args.SourceIDs {
		if !trusted[id] {
			return nil, fmt.Errorf("untrusted source ID %q", id)
		}
	}
	old, err := read(args.Path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && !readFiles[args.Path] {
		return nil, fmt.Errorf("read existing topic before modifying it")
	}
	if call.Name == "memory_delete" {
		if !readFiles[args.Path] {
			return nil, fmt.Errorf("read topic before deleting it")
		}
		staged[args.Path] = nil
		return "deleted (staged)", nil
	}
	if call.Name == "memory_edit" {
		if err != nil {
			return nil, err
		}
		_, old, _ = splitFrontmatter(old)
		if args.OldText == "" || strings.Count(old, args.OldText) != 1 {
			return nil, fmt.Errorf("old_text must match exactly once in body")
		}
		args.Content = strings.Replace(old, args.OldText, args.NewText, 1)
	}
	content, err := s.prepareEntry(Entry{Path: args.Path, Summary: args.Summary, Type: args.Type, Content: args.Content, Sources: args.SourceIDs})
	if err != nil {
		return nil, err
	}
	staged[args.Path] = &content
	if len(manifest()) > s.options.Limits.Files {
		return nil, fmt.Errorf("%w: topic count", ErrBudget)
	}
	return "written (staged)", nil
}
