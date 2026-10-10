package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/tools"
	"CodeAgent/internal/tools/basic/find"
	"CodeAgent/internal/tools/basic/grep"
	"CodeAgent/internal/tools/basic/ls"
	"CodeAgent/internal/tools/extend/subagent"
	"CodeAgent/internal/tools/extend/webfetch"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
)

// ToolBinding 的身份、能力和目标提取由可信宿主给定，模型只提供参数。
type ToolBinding struct {
	Tool       agent.Tool
	Identity   permission.Tool
	Capability permission.Capability
	Targets    func(map[string]any) ([]string, error) // 可信宿主必须完整枚举此次调用可能修改的文件。
	HookName   string                                 // MCP 使用真实服务器/工具键形成的协议名，与模型适配名分开。
}
type toolBinding struct {
	ToolBinding
	schema *jsonschema.Resolved
}

func validModel(ref string, m ModelConfig) error {
	if ref == "" || m.Model.ID == "" || m.Model.Provider == "" {
		return fmt.Errorf("模型引用/身份不能为空")
	}
	if m.Model.BaseURL != "" {
		u, err := url.Parse(m.Model.BaseURL)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("模型来源地址不能包含凭据")
		}
	}
	return nil
}

func fileTargets(args map[string]any) ([]string, error) {
	path, ok := args["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("path 必须为非空字符串")
	}
	return []string{path}, nil
}

func (r *Runtime) toolBindings() ([]ToolBinding, error) {
	root := r.host.options.ProjectDir
	bindings := append([]ToolBinding(nil), r.host.options.Tools...)
	if r.host.options.Tools == nil {
		base, err := tools.New(tools.Config{WorkingDir: root})
		if err != nil {
			return nil, err
		}
		base = append(base, grep.New(root), find.New(root), ls.New(root))
		for _, tool := range base {
			cap := permission.ReadOnly
			var targets func(map[string]any) ([]string, error)
			switch tool.Name {
			case "write", "edit":
				cap = permission.FileWrite
				targets = fileTargets
			case "powershell", "bash":
				cap = permission.Shell
			}
			bindings = append(bindings, ToolBinding{Tool: tool, Identity: permission.Tool{Source: permission.Builtin, SourceID: "builtin", ToolID: tool.Name}, Capability: cap, Targets: targets})
		}
	}
	if r.role != "" {
		filtered := bindings[:0]
		for _, binding := range bindings {
			if binding.Capability == permission.Shell {
				continue
			}
			if r.role == "worker" && binding.Capability == permission.Unknown && binding.Targets == nil {
				continue
			}
			if r.role == "explore" && binding.Capability != permission.ReadOnly {
				continue
			}
			filtered = append(filtered, binding)
		}
		bindings = filtered
	}
	if r.memory != nil {
		for _, definition := range r.memory.ReadTools() {
			definition := definition
			bindings = append(bindings, ToolBinding{Identity: permission.Tool{Source: permission.Builtin, SourceID: "project-memory", ToolID: definition.Name}, Capability: permission.ReadOnly, Tool: agent.Tool{Tool: definition, ParallelSafe: true, Execute: func(ctx context.Context, id string, args map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				result, err := r.memory.CallReadTool(ctx, llm.ContentBlock{Type: llm.TypeToolCall, ID: id, Name: definition.Name, Arguments: args})
				if err != nil {
					return nil, err
				}
				if result.IsError {
					return nil, fmt.Errorf("记忆读取失败")
				}
				return result.Content, nil
			}}})
		}
	}
	if r.host.options.WebFetch != nil {
		tool, err := webfetch.New(*r.host.options.WebFetch)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, ToolBinding{Tool: tool, Identity: permission.Tool{Source: permission.Builtin, SourceID: "webfetch", ToolID: tool.Name}, Capability: permission.ReadOnly})
	}
	if r.host.options.Subagents && r.role == "" {
		list, err := subagent.New(r)
		if err != nil {
			return nil, err
		}
		for _, tool := range list {
			tool = parentTaskTool(tool)
			bindings = append(bindings, ToolBinding{Tool: tool, Identity: permission.Tool{Source: permission.Builtin, SourceID: "subagent", ToolID: tool.Name}, Capability: permission.Orchestration})
		}
	}
	return r.mcpBindings(bindings)
}

func (r *Runtime) assemble(ctx context.Context) error {
	bindings, err := r.toolBindings()
	if err != nil {
		return err
	}
	compiled := map[string]*toolBinding{}
	var tools []agent.Tool
	for _, binding := range bindings {
		if binding.Identity.SourceID == "" || binding.Identity.ToolID == "" || binding.Capability == "" || binding.Tool.Execute == nil || strings.TrimSpace(binding.Tool.Name) == "" {
			return fmt.Errorf("工具可信绑定不完整")
		}
		if compiled[binding.Tool.Name] != nil {
			return fmt.Errorf("工具名称冲突: %s", binding.Tool.Name)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(binding.Tool.Schema, &schema); err != nil {
			return fmt.Errorf("%s schema: %w", binding.Tool.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("%s schema: %w", binding.Tool.Name, err)
		}
		compiled[binding.Tool.Name] = &toolBinding{binding, resolved}
		tools = append(tools, binding.Tool)
	}
	records, err := r.session.GetContext(ctx)
	if err != nil {
		return err
	}
	var history []llm.Message
	origins := map[*llm.AssistantMessage]agent.ModelOrigin{}
	for _, record := range records {
		if record.Message == nil {
			r.emit(Event{Diagnostic: fmt.Errorf("上下文 %s 为 pending，正文不可用", record.EntryID)})
			continue
		}
		history = append(history, record.Message)
		if assistant, ok := record.Message.(*llm.AssistantMessage); ok && record.Origin != nil {
			o := record.Origin
			origins[assistant] = agent.ModelOrigin{Provider: o.Provider, API: o.API, ModelID: o.ModelID, BaseURL: o.BaseURL}
		}
	}
	model := r.host.options.Models[r.modelRef]
	a, err := agent.New(agent.Config{Model: model.Model, Options: model.Options, SystemPrompt: r.prompt, InitialHistory: history, InitialOrigins: origins, NewRunID: uuid.NewString, Stream: r.host.options.Stream, Tools: tools, MaxParallelTools: r.host.options.MaxParallelTools, OnEvent: r.onAgentEvent})
	if err != nil {
		return err
	}
	driver, err := a.Driver()
	if err != nil {
		a.RequestClose()
		_ = a.Close()
		return err
	}
	r.mu.Lock()
	r.agent = a
	r.driver = driver
	r.bindings = compiled
	r.driverDone = make(chan struct{})
	done := r.driverDone
	r.mu.Unlock()
	go r.drive(driver, done)
	return nil
}

func (r *Runtime) targets(binding *toolBinding, args map[string]any) ([]string, error) {
	if binding.Targets == nil {
		if binding.Capability == permission.FileWrite {
			return nil, fmt.Errorf("文件修改工具缺少可信目标提取")
		}
		return nil, nil
	}
	paths, err := binding.Targets(args)
	if err != nil {
		return nil, err
	}
	if r.role == "worker" && binding.Capability == permission.Unknown && len(paths) == 0 {
		return nil, fmt.Errorf("worker 未知能力工具必须提供完整非空修改目标")
	}
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.host.options.ProjectDir, path)
		}
		paths[i], err = realPath(path)
		if err != nil {
			return nil, err
		}
	}
	if r.role == "worker" && len(r.scope) > 0 {
		for _, path := range paths {
			found := false
			for _, allowed := range r.scope {
				if path == allowed {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("worker 修改超出声明文件范围: %s", path)
			}
		}
	}
	return paths, nil
}

// ToolInfo is a detached view of the tools currently installed in this Runtime.
type ToolInfo struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Parameters  json.RawMessage       `json:"parameters"`
	Identity    permission.Tool       `json:"identity"`
	Capability  permission.Capability `json:"capability"`
}

func (r *Runtime) ToolCatalog() ([]ToolInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return nil, ErrClosed
	}
	out := make([]ToolInfo, 0, len(r.bindings))
	for _, binding := range r.bindings {
		data := append(json.RawMessage(nil), binding.Tool.Schema...)
		out = append(out, ToolInfo{Name: binding.Tool.Name, Description: binding.Tool.Description, Parameters: data, Identity: binding.Identity, Capability: binding.Capability})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
