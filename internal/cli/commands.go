package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"CodeAgent/internal/agent"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/tui"
)

type commandResult struct {
	value               any
	reset, exit, cancel bool
	choices             tui.ActionKind
	prompt              string
	form                *tui.FormEvent
}
type slashCommand struct {
	name, usage, help string
	handle            func(context.Context, *application, string) (commandResult, error)
}

// This registry owns names, help, completion and dispatch for both frontends.
func commandRegistry() []slashCommand {
	return []slashCommand{
		{"/help", "/help", "显示会话内命令", func(_ context.Context, _ *application, arg string) (commandResult, error) {
			if arg != "" {
				return commandResult{}, fmt.Errorf("/help takes no arguments")
			}
			return commandResult{value: chatHelp()}, nil
		}},
		{"/model", "/model [REF]", "选择或切换模型", modelCommand},
		{"/status", "/status", "当前会话、任务、审批与能力", viewCommand("status")},
		{"/settings", "/settings", "只读有效设置、路径及 AGENTS 状态", viewCommand("settings")},
		{"/permission", "/permission [default|auto_edit|auto_approve]", "当前项目唯一审批策略", policyCommand},
		{"/plan", "/plan [内容]", "切换计划模式并可发送内容", modeCommand("plan")},
		{"/build", "/build [内容]", "切换构建模式并可发送内容", modeCommand("build")},
		{"/config", "/config", "实际配置路径与重启要求", viewCommand("config")},
		{"/open", "/open PATH", "切换项目并创建新会话", func(ctx context.Context, a *application, arg string) (commandResult, error) {
			if arg == "" {
				return commandResult{}, fmt.Errorf("/open requires a directory")
			}
			arg = commandPath(arg)
			if !filepath.IsAbs(arg) {
				arg = filepath.Join(a.capabilities.Instructions.ProjectRoot, arg)
			}
			return switchCommand(ctx, a, arg, "")
		}},
		{"/new", "/new [JSON options]", "选择项目、模型与长期记忆，创建新会话", newCommand},
		{"/sessions", "/sessions [search|rename|archive|unarchive|history|export ...]", "会话列表、搜索、恢复与管理", sessionsCommand},
		{"/session", "/session ID", "恢复主会话与保存目录", func(ctx context.Context, a *application, arg string) (commandResult, error) {
			if arg == "" {
				return commandResult{}, fmt.Errorf("/session requires an ID")
			}
			return switchCommand(ctx, a, "", arg)
		}},
		{"/cancel", "/cancel", "请求取消当前任务", func(_ context.Context, _ *application, arg string) (commandResult, error) {
			if arg != "" {
				return commandResult{}, fmt.Errorf("/cancel takes no arguments")
			}
			return commandResult{cancel: true}, nil
		}},
		{"/exit", "/exit", "取消并等待资源安全关闭", func(_ context.Context, _ *application, arg string) (commandResult, error) {
			if arg != "" {
				return commandResult{}, fmt.Errorf("/exit takes no arguments")
			}
			return commandResult{exit: true}, nil
		}},
		{"/skills", "/skills [add|edit|remove ...]", "技能来源管理及加载错误", sourcesCommand("skills")},
		{"/tools", "/tools [refresh|reconnect SERVER_ID]", "实际内置工具、参数与 MCP 服务器", toolsCommand},
		{"/mcp", "/mcp [refresh|reconnect SERVER_ID]", "MCP 状态、目录或空闲刷新", mcpCommand},
		{"/hooks", "/hooks [add|edit|remove ...]", "Hook 来源管理及加载错误", sourcesCommand("hooks")},
		{"/memory", "/memory", "公开持久化进度；不推断后台完成", viewCommand("memory")},
		{"/tasks", "/tasks [result ID|cancel ID|enable|disable|web_fetch enable|disable]", "子任务状态、结果与子代理/网页抓取能力设置", tasksCommand},
	}
}

func chatHelp() string {
	var b strings.Builder
	b.WriteString("会话内直接输入任务；斜杠命令不需要 codeagent 前缀。\n")
	for _, c := range commandRegistry() {
		fmt.Fprintf(&b, "%-38s %s\n", c.usage, c.help)
	}
	b.WriteString("TUI: Enter 发送 · Alt+Enter/Ctrl+J 换行 · Ctrl+C 取消任务 · Ctrl+Q 退出。\n配置修改后重启；/mcp refresh 不是 TOML 热更新。\n")
	return b.String()
}
func tuiCommands() []tui.Command {
	out := make([]tui.Command, 0, len(commandRegistry()))
	for _, c := range commandRegistry() {
		out = append(out, tui.Command{Name: c.name, Description: c.help})
	}
	return out
}
func (a *application) command(ctx context.Context, text string) (commandResult, error) {
	name, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	arg = strings.TrimSpace(arg)
	for _, c := range commandRegistry() {
		if c.name == name {
			return c.handle(ctx, a, arg)
		}
	}
	return commandResult{}, fmt.Errorf("unknown chat command %q; use /help", name)
}
func modelCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if arg == "" {
		return commandResult{value: struct {
			Current string   `json:"current"`
			Models  []string `json:"models"`
		}{a.selectedRef, append([]string{}, a.modelRefs...)}, choices: tui.ListModels}, nil
	}
	if err := a.current.SetModel(ctx, arg); err != nil {
		return commandResult{}, err
	}
	a.selectedRef = arg
	return commandResult{value: struct {
		Model string `json:"model"`
	}{arg}}, nil
}
func viewCommand(kind string) func(context.Context, *application, string) (commandResult, error) {
	return func(ctx context.Context, a *application, arg string) (commandResult, error) {
		if arg != "" {
			return commandResult{}, fmt.Errorf("/%s takes no arguments", kind)
		}
		v, e := a.commandView(ctx, kind)
		result := commandResult{value: v}
		if kind == "sessions" {
			result.choices = tui.ListSessions
		}
		return result, e
	}
}

func (a *application) mainSessions(ctx context.Context) ([]sc.SessionInfo, error) {
	out := []sc.SessionInfo{}
	for offset := 0; ; offset += 200 {
		page, err := a.service.ListSessions(ctx, sc.SessionListOptions{ArchiveFilter: sc.ArchiveFilter("active"), Limit: 200, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, s := range page {
			child, err := a.isChild(ctx, s.ID)
			if err != nil {
				return nil, err
			}
			if !child {
				out = append(out, s)
			}
		}
		if len(page) < 200 {
			return out, nil
		}
	}
}
func (a *application) busy() bool {
	for _, r := range a.host.Opened() {
		s := r.Snapshot()
		if s.Agent.Status == agent.AgentStatusRunning || s.Agent.Status == agent.AgentStatusCancelling || s.Agent.QueueLength > 0 || len(s.Approvals) > 0 {
			return true
		}
	}
	return false
}
func switchCommand(ctx context.Context, a *application, root, id string) (commandResult, error) {
	if a.busy() {
		return commandResult{}, fmt.Errorf("任务或审批仍活动；先 /cancel 并等待结束")
	}
	if id == a.current.SessionID() {
		return commandResult{value: map[string]string{"session_id": id}}, nil
	}
	if id != "" {
		child, e := a.isChild(ctx, id)
		if e != nil {
			return commandResult{}, e
		}
		if child {
			return commandResult{}, fmt.Errorf("child sessions cannot be opened as main sessions")
		}
		s, e := a.service.GetSession(ctx, id)
		if e != nil {
			return commandResult{}, e
		}
		if s.ArchivedAt != nil {
			return commandResult{}, fmt.Errorf("archived session cannot be opened")
		}
		root = s.WorkingDir
	}
	canonical, err := canonicalDir(root)
	if err != nil {
		return commandResult{}, err
	}
	options := a.options
	options.ModelEntries = a.entries
	options.MCP = a.manager
	inv := a.invocation
	inv.directory = canonical
	inv.sessionID = id
	inv.modelRef = ""
	inv.settingsPath = ""
	inv.skills = nil
	inv.hooks = nil
	if id == "" {
		inv.modelRef = a.selectedRef
	}
	next := &application{settingsBytes: append([]byte(nil), a.settingsBytes...), settingsDocument: a.settingsDocument, memoryDefault: a.memoryDefault, memoryBaseDirectory: a.memoryBaseDirectory, memoryBaseScope: a.memoryBaseScope, options: options, invocation: inv, streams: a.streams, hint: a.hint, notices: a.notices, rawEvents: a.rawEvents, rawOverflow: a.rawOverflow, configFile: a.configFile, settingsFile: a.settingsFile, settingsLoaded: a.settingsLoaded, defaultModel: a.defaultModel}
	if err = next.openStore(ctx); err == nil {
		err = next.assemble(ctx)
	}
	if err != nil {
		_ = next.closeResources()
		return commandResult{}, err
	}
	owner := a.ownsManager
	// Manager is transferred, not closed under the candidate Host. The candidate borrows
	// its already-started connection; CLI still owns shutdown and refresh coordination.
	a.ownsManager = false
	if err = a.closeResources(); err != nil {
		a.ownsManager = owner
		_ = next.closeResources()
		return commandResult{}, fmt.Errorf("旧 Host 收尾未完成，未切换: %w", err)
	}
	next.ownsManager = owner
	next.input = a.input
	*a = *next
	return commandResult{reset: true, value: map[string]string{"session_id": a.current.SessionID(), "project": canonical, "model": a.selectedRef, "notice": "旧 Host 已关闭；后台工作取消并等待结束。旧聊天已保留，新会话不继承聊天。"}}, nil
}

func (a *application) commandView(ctx context.Context, kind string) (any, error) {
	switch kind {
	case "settings":
		return a.settingsView(ctx)
	case "status":
		return a.status(), nil
	case "config":
		return map[string]any{"model_config": a.configFile, "host_settings": a.settingsFile, "project": a.capabilities.Instructions.ProjectRoot, "selected_model": a.selectedRef, "default_model": a.defaultModel, "model_config_injected": a.options.ModelEntries != nil && a.configFile == "", "reload": "restart required for TOML/settings; MCP refresh only existing connection/catalog", "relative_config": "explicit paths use startup cwd; default config is next to executable; project unchanged"}, nil
	case "sessions":
		return a.mainSessions(ctx)
	case "skills":
		items := []map[string]any{}
		for _, s := range a.skillCatalog.Skills {
			items = append(items, map[string]any{"name": s.Name, "description": s.Description, "file": s.FilePath, "source": s.Source.Name, "model_invocation": !s.DisableModelInvocation})
		}
		return map[string]any{"sources": a.capabilities.Skills, "discovered_metadata": items, "diagnostics": a.skillCatalog.Diagnostics, "body_loading": "progressive read; not eagerly loaded or executed"}, nil
	case "hooks":
		sources := []map[string]string{}
		for _, source := range a.options.HookSources {
			sources = append(sources, map[string]string{"id": source.ID, "kind": string(source.Kind)})
		}
		return map[string]any{"sources": sources, "script_hooks": a.capabilities.ScriptHooks, "source_count": len(a.options.HookSources), "native_model_hooks": a.capabilities.ModelHooks, "reload": "restart"}, nil
	case "tools":
		return a.toolsView(ctx)
	case "legacy_tools":
		names := []string{}
		for _, t := range a.options.Tools {
			names = append(names, t.Tool.Name)
		}
		return map[string]any{"default_builtins": a.capabilities.DefaultTools, "custom_configured": names, "memory_tools": a.capabilities.Memory, "subagent_tools": a.capabilities.Subagents, "web_fetch": a.capabilities.WebFetch, "mcp": a.capabilities.MCP, "notice": "configuration view; Runtime has no complete active-tool catalog accessor. F4 expands observed tool records."}, nil
	case "memory":
		v := map[string]any{"enabled": a.capabilities.Memory, "scope": a.capabilities.MemoryScope, "shutdown": a.capabilities.MemoryShutdown, "background_completion": "not exposed; not inferred from Run completion"}
		if a.capabilities.Memory {
			s, e := a.service.OpenSession(ctx, a.current.SessionID())
			if e != nil {
				return nil, e
			}
			p, e := s.GetMemoryProgress(ctx, a.capabilities.MemoryScope)
			if e != nil {
				return nil, e
			}
			v["persisted_progress"] = p
		}
		return v, nil
	case "tasks":
		s, e := a.service.OpenSession(ctx, a.current.SessionID())
		if e != nil {
			return nil, e
		}
		tasks, e := s.ListChildTasks(ctx)
		if e != nil {
			return nil, e
		}
		states := []map[string]any{}
		for _, r := range a.host.Opened() {
			if r.SessionID() != a.current.SessionID() {
				snap := r.Snapshot()
				states = append(states, map[string]any{"session_id": r.SessionID(), "state": snap.Agent.Status, "run_id": snap.Agent.CurrentRunID, "pending_approvals": len(snap.Approvals)})
			}
		}
		return map[string]any{"enabled": a.capabilities.Subagents, "records": tasks, "opened_states": states}, nil
	}
	return nil, fmt.Errorf("unsupported view")
}
func mcpCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if a.manager == nil {
		return commandResult{value: map[string]any{"configured": false, "notice": "edit [mcp.SERVER_ID] in TOML then restart"}}, nil
	}
	if arg != "" {
		parts := strings.Fields(arg)
		if len(parts) != 2 || (parts[0] != "refresh" && parts[0] != "reconnect") {
			return commandResult{}, fmt.Errorf("use /mcp [refresh|reconnect SERVER_ID]")
		}
		if a.busy() {
			return commandResult{}, fmt.Errorf("MCP refresh requires every opened Runtime idle")
		}
		var err error
		if a.options.MCP != nil {
			if !a.ownsManager {
				return commandResult{}, fmt.Errorf("borrowed MCP Manager refresh belongs to its external owner")
			}
			if parts[0] == "reconnect" {
				err = a.manager.Reconnect(ctx, parts[1])
			} else {
				err = a.manager.Refresh(ctx, parts[1])
			}
			applyErr := a.current.RefreshMCP(ctx, "", false)
			if err == nil {
				err = applyErr
			}
		} else {
			err = a.current.RefreshMCP(ctx, parts[1], parts[0] == "reconnect")
		}
		if err != nil {
			return commandResult{}, err
		}
	}
	defs, err := a.manager.Definitions()
	tools := []map[string]string{}
	if err == nil {
		for _, d := range defs {
			tools = append(tools, map[string]string{"server": d.Key.ServerID, "name": d.Key.Name})
		}
	}
	return commandResult{value: map[string]any{"configured": true, "servers": a.manager.Statuses(), "available_definitions": tools, "catalog_available": err == nil, "reload": "refresh/reconnect use saved Manager configuration; new servers or changed parameters require restart"}}, nil
}
