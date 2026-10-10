package longterm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/memory/longterm/storage"
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

// 实际读取过的来源用于主题合并；版本读取权限与来源集合分开保存。
type readEvidence struct {
	files   map[string]bool
	sources []string
}

// batchTools 只保存本批的记忆领域状态；执行方按契约串行调用，不监督模型或流。
type batchTools struct {
	ctx         context.Context
	transaction *storage.Transaction
	callLimit   int
	trusted     map[string]bool
	evidence    *readEvidence
	seenCalls   map[string]bool
	calls       int
	failure     error
}

func (b *batchTools) call(ctx context.Context, call llm.ContentBlock) (result *llm.ToolResultMessage, err error) {
	b.calls++ // 实际入口次数，包括参数错误及失败后的尝试。
	if b.failure != nil {
		return nil, b.failure
	}
	defer func() {
		if err != nil {
			b.failure = err // 首个领域失败禁止本批提交，即使执行方忽略它。
		}
	}()
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, fmt.Errorf("longterm: nil tool context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.calls > b.callLimit {
		return nil, fmt.Errorf("%w: tool calls", ErrBudget)
	}
	if call.Type != llm.TypeToolCall || !storage.ValidID(call.ID, 256) || b.seenCalls[call.ID] {
		return nil, fmt.Errorf("longterm: invalid or duplicate call ID")
	}
	b.seenCalls[call.ID] = true
	value, err := b.executeTool(ctx, call)
	if err != nil {
		return nil, fmt.Errorf("longterm: %s: %w", call.Name, err)
	}
	return toolResult(call, value)
}

func toolResult(call llm.ContentBlock, value any) (*llm.ToolResultMessage, error) {
	text, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &llm.ToolResultMessage{Role: llm.RoleTool, ToolCallID: call.ID, ToolName: call.Name,
		Content: []llm.ContentBlock{{Type: llm.TypeText, Text: string(text)}}}, nil
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

func catalog() []llm.Tool { return definitionsCatalog(toolDefinitions) }

func definitionsCatalog(definitions []toolDefinition) []llm.Tool {
	tools := make([]llm.Tool, 0, len(definitions))
	for _, d := range definitions {
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

// decodeToolArguments 共用 schema 对应的白名单与参数解析。
func decodeToolArguments(call llm.ContentBlock, definitions []toolDefinition) (toolArgs, error) {
	// 先匹配独立工具目录，再严格解码；未知工具没有任何执行路径。
	var definition *toolDefinition
	for i := range definitions {
		if toolDefinitions[i].name == call.Name {
			definition = &definitions[i]
			break
		}
	}
	if definition == nil {
		return toolArgs{}, fmt.Errorf("tool not allowed")
	}
	encoded, err := json.Marshal(call.Arguments)
	if err != nil {
		return toolArgs{}, err
	}
	for key := range call.Arguments {
		if _, ok := definition.properties[key]; !ok {
			return toolArgs{}, fmt.Errorf("unknown argument %q", key)
		}
		if call.Arguments[key] == nil {
			return toolArgs{}, fmt.Errorf("null argument %q", key)
		}
	}
	for _, key := range definition.required {
		if value, ok := call.Arguments[key]; !ok || value == nil {
			return toolArgs{}, fmt.Errorf("missing argument %q", key)
		}
	}
	var args toolArgs
	if err := json.Unmarshal(encoded, &args); err != nil {
		return toolArgs{}, err
	}
	return args, nil
}

func (b *batchTools) executeTool(ctx context.Context, call llm.ContentBlock) (any, error) {
	readFiles := b.evidence.files
	args, err := decodeToolArguments(call, toolDefinitions)
	if err != nil {
		return nil, err
	}
	switch call.Name {
	case "memory_list":
		return b.transaction.Files()
	case "memory_read":
		document, err := b.transaction.Read(args.Path)
		if err != nil {
			return nil, err
		}
		if args.Path == storage.IndexName {
			return document, nil
		}
		topic, err := storage.ParseTopic(document.Content)
		if err != nil {
			return nil, err
		}
		b.evidence.sources = append(b.evidence.sources, topic.Sources...)
		readFiles[args.Path] = true
		return document, nil
	case "memory_search":
		return b.transaction.Search(ctx, args.Query, args.Limit)
	}
	if err := storage.ValidPath(args.Path); err != nil {
		return nil, err
	}
	if args.Path == storage.IndexName {
		return nil, os.ErrPermission
	}
	if len(args.SourceIDs) == 0 {
		return nil, fmt.Errorf("source_ids required")
	}
	seenSources := map[string]bool{}
	for _, id := range args.SourceIDs {
		if !b.trusted[id] || seenSources[id] {
			return nil, fmt.Errorf("untrusted source ID %q", id)
		}
		seenSources[id] = true
	}
	old, err := b.transaction.Read(args.Path)
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
		if err := b.transaction.Forget(args.Path); err != nil {
			return nil, err
		}
		delete(readFiles, args.Path)
		return "deleted (staged)", nil
	}
	var oldTopic Entry
	if err == nil {
		var parseErr error
		oldTopic, parseErr = storage.ParseTopic(old.Content)
		if parseErr != nil {
			return nil, parseErr
		}
		merged := make([]string, 0, len(oldTopic.Sources)+len(b.evidence.sources)+len(args.SourceIDs))
		seen := map[string]bool{}
		for _, id := range append(append(oldTopic.Sources, b.evidence.sources...), args.SourceIDs...) {
			if !seen[id] {
				merged = append(merged, id)
				seen[id] = true
			}
		}
		args.SourceIDs = merged
	}
	if call.Name == "memory_edit" {
		if err != nil {
			return nil, err
		}
		body := oldTopic.Content
		if args.OldText == "" || strings.Count(body, args.OldText) != 1 {
			return nil, fmt.Errorf("old_text must match exactly once in body")
		}
		args.Content = strings.Replace(body, args.OldText, args.NewText, 1)
	}
	if err := b.transaction.Put(Entry{Path: args.Path, Summary: args.Summary, Type: args.Type, Content: args.Content, Sources: args.SourceIDs}); err != nil {
		return nil, err
	}
	delete(readFiles, args.Path) // 再次修改须读取当前暂存版本。
	return "written (staged)", nil
}
