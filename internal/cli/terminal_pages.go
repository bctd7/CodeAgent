package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"CodeAgent/internal/hooks"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/mcp"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	"CodeAgent/internal/tui"
)

func pageDetail(label, text string) tui.MenuItem { return tui.MenuItem{Label: label, Text: text} }
func pageCommand(label, detail, command string) tui.MenuItem {
	return tui.MenuItem{Label: label, Detail: detail, Command: command}
}
func sourceForm(kind, op string, index int, path string) *tui.FormEvent {
	command := "/" + kind + " " + op
	if index > 0 {
		command += fmt.Sprintf(" %d", index)
	}
	return &tui.FormEvent{Title: "来源路径 · 保存后重启生效", Command: command, Page: "/" + kind, Plain: true, Fields: []tui.FormField{{Name: "path", Label: "来源绝对路径（可含空格）", Value: path}}}
}
func (b *terminalBackend) modelOptions() []tui.Choice {
	a := b.app
	out := []tui.Choice{}
	for _, ref := range a.modelRefs {
		info := a.modelInfo(ref)
		detail := info.Provider
		if ref == a.selectedRef {
			detail += " · 当前"
		}
		out = append(out, tui.Choice{ID: ref, Label: info.DisplayName, Detail: detail})
	}
	return out
}
func (b *terminalBackend) page(ctx context.Context, name string) ([]tui.Event, bool, error) {
	a := b.app
	event := tui.MenuEvent{Scope: tui.Scope{SessionID: a.current.SessionID()}, Page: name, Search: true}
	switch name {
	case "/new":
		result, e := newCommand(ctx, a, "")
		if e != nil {
			return nil, true, e
		}
		form := *result.form
		form.Title = "新会话 · 确认字段后 Enter 创建"
		form.Fields[1].Label = "模型"
		form.Fields[1].Options = b.modelOptions()
		form.Fields[2].Label = "长期记忆"
		return []tui.Event{form}, true, nil
	case "/model":
		event.Title = "模型"
		for _, option := range b.modelOptions() {
			info := a.modelInfo(option.ID)
			event.Items = append(event.Items, tui.MenuItem{Label: option.Label, Detail: option.Detail, Children: []tui.MenuItem{pageCommand("使用此模型", "空闲时切换", "/model "+option.ID), pageDetail("模型详情", fmt.Sprintf("平台：%s\n名称：%s\nID：%s\nREF：%s", info.Provider, info.DisplayName, info.ID, info.Ref))}})
		}
	case "/permission":
		event.Title = "审批模式"
		event.Text = "当前项目：" + a.capabilities.Instructions.ProjectRoot + "\n保存到唯一 settings.json；仅保存一个项目绑定。"
		modes := []struct{ key, label, detail string }{{"default", "默认审批", "按现有规则请求审批"}, {"auto_edit", "自动编辑", "按现有规则自动批准可编辑目标"}, {"auto_approve", "自动审批", "仍受权限规则约束，不会全放外部 MCP"}}
		for _, mode := range modes {
			label := mode.label
			if string(a.options.Policy.ApprovalMode) == mode.key {
				label += " ✓ 当前"
			}
			item := pageCommand(label, mode.detail, "/permission "+mode.key)
			if a.busy() {
				item.Disabled = "任务或审批活动中，结束后再修改"
			}
			event.Items = append(event.Items, item)
		}
	case "/sessions":
		event.Title = "会话"
		event.Filters = []string{"全部", "活动", "归档"}
		sessions, e := a.listMainSessions(ctx, "", "all")
		if e != nil {
			return nil, true, e
		}
		for _, s := range sessions {
			state := "活动"
			if s.ArchivedAt != nil {
				state = "归档"
			}
			title := s.Title
			if title == "" {
				title = "未命名会话"
			}
			if s.ID == a.current.SessionID() {
				title += " ✓ 当前"
			}
			restore := pageCommand("继续会话", "切换并恢复", "/session "+s.ID)
			hist := pageCommand("查看历史", "", "/sessions history "+s.ID)
			export := tui.MenuItem{Label: "导出 JSON", Detail: "拒绝覆盖已有文件", Form: &tui.FormEvent{Title: "导出会话", Command: "/sessions export " + s.ID, Page: name, Plain: true, Fields: []tui.FormField{{Name: "path", Label: "目标文件绝对路径"}}}}
			arch := pageCommand("归档", "不删除记录", "/sessions archive "+s.ID)
			if s.ArchivedAt != nil {
				restore.Disabled = "先恢复归档"
				hist.Disabled = "Session 接口要求先恢复归档"
				export.Disabled = "先恢复归档后导出"
				arch = pageCommand("恢复归档", "恢复后可继续/查看/导出", "/sessions unarchive "+s.ID)
			} else if s.ID == a.current.SessionID() {
				arch.Disabled = "先切换到其他会话"
			}
			rename := tui.MenuItem{Label: "重命名", Form: &tui.FormEvent{Title: "会话标题", Command: "/sessions rename " + s.ID, Page: name, Plain: true, Fields: []tui.FormField{{Name: "title", Label: "标题", Value: s.Title}}}}
			event.Items = append(event.Items, tui.MenuItem{Label: title, Detail: state + " · " + s.WorkingDir, Filter: state, Children: []tui.MenuItem{restore, rename, hist, export, arch, pageDetail("会话详情", "ID："+s.ID+"\n项目："+s.WorkingDir)}})
		}
	case "/tools", "/mcp":
		event.Page = "/tools"
		event.Title = "MCP 工具"
		event.Text = "这里只展示 MCP。模型实际内置工具保持不变。MCP 配置增删改/启停保存暂未提供。"
		catalog, e := a.current.ToolCatalog()
		if e != nil {
			return nil, true, e
		}
		statuses := []mcp.ServerStatus{}
		if a.manager != nil {
			statuses = a.manager.Statuses()
		}
		_, groups, _ := groupAppliedTools(catalog, statuses)
		servers := []tui.MenuItem{}
		for _, g := range groups {
			refresh := pageCommand("刷新工具目录", "使用现有配置", "/tools refresh "+g.Status.ID)
			reconnect := pageCommand("重新连接", "使用现有配置", "/tools reconnect "+g.Status.ID)
			reason := ""
			if a.busy() {
				reason = "所有会话须空闲"
			}
			if a.options.MCP != nil && !a.ownsManager {
				reason = "共享 Manager 由外部所有者管理"
			}
			refresh.Disabled = reason
			reconnect.Disabled = reason
			servers = append(servers, tui.MenuItem{Label: g.Status.ID, Detail: fmt.Sprintf("%s · 已装配 %d 项", g.Status.State, len(g.AppliedTools)), Children: []tui.MenuItem{{Label: "查看工具", Children: toolMenuItems(g.AppliedTools)}, refresh, reconnect, pageDetail("状态详情", fmt.Sprintf("连接目录 %d 项\n错误：%s", g.Status.ToolCount, a.safe(g.Status.Error)))}})
		}
		event.Items = append(event.Items, tui.MenuItem{Label: "MCP 服务器", Detail: fmt.Sprintf("%d 个", len(servers)), Children: servers})
		if len(servers) == 0 {
			event.Text += "\n未配置 MCP 服务器；这不代表内置工具被禁用。"
		}
	case "/skills", "/hooks":
		kind := strings.TrimPrefix(name, "/")
		event.Title = map[string]string{"skills": "技能", "hooks": "钩子"}[kind]
		event.Text = "来源变更保存后须重启。移除只修改配置，不删除文件。"
		sources := []tui.MenuItem{{Label: "添加来源", Form: sourceForm(kind, "add", 0, "")}}
		paths := []string{}
		if kind == "skills" {
			for _, source := range a.settingsDocument.Skills {
				paths = append(paths, source.Path)
			}
			loaded := []tui.MenuItem{}
			for _, skill := range a.skillCatalog.Skills {
				status := "已发现"
				if skill.DisableModelInvocation {
					status += " · 禁止模型调用"
				}
				loaded = append(loaded, tui.MenuItem{Label: skill.Name, Detail: status + " · " + shortText(skill.Description), Text: "状态：" + status + "\n来源：" + skill.Source.Name + "\n文件：" + skill.FilePath})
			}
			event.Items = append(event.Items, tui.MenuItem{Label: "已加载技能", Detail: fmt.Sprintf("%d 项", len(loaded)), Children: loaded})
			problems := []tui.MenuItem{}
			for _, d := range a.skillCatalog.Diagnostics {
				problems = append(problems, pageDetail(d.Code, a.safe(d.Message)+"\n"+d.Path))
			}
			event.Items = append(event.Items, tui.MenuItem{Label: "加载诊断", Detail: fmt.Sprintf("%d 项", len(problems)), Children: problems})
		} else {
			paths = append(paths, a.settingsDocument.HookFiles...)
			events := map[string][]tui.MenuItem{}
			for _, source := range a.options.HookSources {
				_, e := hooks.New([]hooks.Source{source}, a.options.HookOptions)
				if e != nil {
					event.Items = append(event.Items, pageDetail("来源错误 · "+source.ID, a.safe(e.Error())))
					continue
				}
				var doc struct {
					Hooks map[string][]struct {
						Hooks []json.RawMessage `json:"hooks"`
					} `json:"hooks"`
				}
				if json.Unmarshal(source.JSON, &doc) != nil {
					continue
				}
				for key, groups := range doc.Hooks {
					count := 0
					for _, group := range groups {
						count += len(group.Hooks)
					}
					events[key] = append(events[key], pageDetail(source.ID, fmt.Sprintf("已加载配置：%d 个钩子\n类型：%s\n不显示脚本正文或参数。", count, source.Kind)))
				}
			}
			keys := []string{}
			for key := range events {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				event.Items = append(event.Items, tui.MenuItem{Label: key, Detail: "已加载事件配置", Children: events[key]})
			}
			if len(keys) == 0 {
				event.Items = append(event.Items, pageDetail("脚本钩子状态", fmt.Sprintf("已启用：%t\n模型原生 Hook：%t", a.capabilities.ScriptHooks, a.capabilities.ModelHooks)))
			}
		}
		for i, path := range paths {
			remove := pageCommand("移除来源", "仅移除配置，不删文件", fmt.Sprintf("/%s remove %d", kind, i+1))
			sources = append(sources, tui.MenuItem{Label: filepath.Base(path), Detail: path, Children: []tui.MenuItem{{Label: "编辑来源路径", Form: sourceForm(kind, "edit", i+1, path)}, {Label: "确认移除", Text: "仅移除配置，文件保留；重启生效。", Children: []tui.MenuItem{remove}}}})
		}
		event.Items = append(event.Items, tui.MenuItem{Label: "来源管理", Detail: fmt.Sprintf("已保存 %d 个", len(paths)), Children: sources})
	case "/tasks":
		event.Title = "子任务"
		event.Text = "能力设置仅影响后续装配；网页抓取不是子任务。\n生效方式：新建会话、切到另一会话再返回，或重启。直接选择当前同一会话不会重新装配。"
		event.Items = []tui.MenuItem{{Label: "能力设置", Children: []tui.MenuItem{capabilityMenu("子代理", "subagents", a.capabilities.Subagents, a.settingsDocument.Subagents), capabilityMenu("网页抓取", "web_fetch", a.capabilities.WebFetch, a.settingsDocument.WebFetch)}}}
		taskItems := []tui.MenuItem{}

		s, e := a.service.OpenSession(ctx, a.current.SessionID())
		if e != nil {
			return nil, true, e
		}
		tasks, e := s.ListChildTasks(ctx)
		if e != nil {
			return nil, true, e
		}
		for _, task := range tasks {
			state, summary := "未启动", ""
			cancelReason := "没有运行中的子任务"
			child, e := a.service.OpenSession(ctx, task.ChildSessionID)
			if e != nil {
				state = "不可打开"
				summary = a.safe(e.Error())
			} else {
				runs, e := child.ListRuns(ctx, 1, 0)
				if e != nil {
					return nil, true, e
				}
				if len(runs) > 0 {
					state = string(runs[0].State)
					summary = shortText(runs[0].ErrorMessage)
					detail, e := child.GetRun(ctx, runs[0].ID)
					if e != nil {
						return nil, true, e
					}
					for _, entry := range detail.Entries {
						if msg, ok := entry.Message.(*llm.AssistantMessage); ok {
							summary = shortText(textBlocks(msg.Content))
						}
					}
				}
			}
			for _, opened := range a.host.Opened() {
				if opened.SessionID() == task.ChildSessionID {
					snap := opened.Snapshot()
					if string(snap.Agent.Status) == "running" {
						cancelReason = ""
					}
				}
			}
			cancel := pageCommand("取消任务", "请求取消后等待完成", "/tasks cancel "+task.ID)
			cancel.Disabled = cancelReason
			taskItems = append(taskItems, tui.MenuItem{Label: task.Role, Detail: state + " · " + a.safe(summary), Children: []tui.MenuItem{pageCommand("查看结果", "最新 Run 及交付内容", "/tasks result "+task.ID), cancel, pageDetail("任务详情", "ID："+task.ID+"\n子会话："+task.ChildSessionID)}})
		}
		event.Items = append(event.Items, tui.MenuItem{Label: "子任务列表", Detail: fmt.Sprintf("%d 项", len(taskItems)), Children: taskItems})
	case "/settings":
		event.Title = "设置 · 只读"
		settings := a.settingsFile
		if settings == "" {
			settings = "EXE 同目录 settings.json（首次保存创建）"
		}
		event.Items = []tui.MenuItem{pageDetail("配置来源", "模型配置："+a.configFile+"\n运行设置："+settings+"\n数据目录："+a.options.DataDir), pageDetail("项目与指令", "项目："+a.capabilities.Instructions.ProjectRoot+"\nAGENTS 在 Runtime 初始化时加载；当前读取见详情。"), pageDetail("能力", fmt.Sprintf("长期记忆：%t\n子代理：%t\n技能来源：%d\n脚本 Hook：%t\nMCP：%t\n网页抓取：%t", a.capabilities.Memory, a.capabilities.Subagents, len(a.capabilities.Skills), a.capabilities.ScriptHooks, a.capabilities.MCP, a.capabilities.WebFetch)), pageDetail("保存与生效", "唯一 settings.json。策略只保存一个项目绑定。技能/Hook 来源重启生效。")}
		view, e := a.settingsView(ctx)
		if e != nil {
			return nil, true, e
		}
		v := view.(map[string]any)
		detail, e := formatCommandValue(map[string]any{"files": v["agents_current_files"], "diagnostics": v["agents_current_diagnostics"]})
		if e != nil {
			return nil, true, e
		}
		event.Items[1].Text += "\n" + a.safe(detail)
	case "/status":
		event.Title = "当前状态 · 只读"
		s := a.status()
		event.Text = fmt.Sprintf("模型：%s\n项目：%s\n会话：%s\n状态：%s\n模式：%s / %s", s.Model.DisplayName, s.Capabilities.Instructions.ProjectRoot, b.status().SessionTitle, s.State, s.Capabilities.WorkMode, s.Capabilities.ApprovalMode)
		event.Items = []tui.MenuItem{pageDetail("运行详情", fmt.Sprintf("会话 ID：%s\nRun：%s\n排队：%d\n待审批：%d\n错误：%s", s.SessionID, s.RunID, len(s.QueuedRunIDs), len(s.Approvals), a.safe(s.Error)))}
	case "/help":
		event.Title = "帮助 · 选择页面"
		for _, cmd := range commandRegistry() {
			if cmd.name == "/memory" || cmd.name == "/mcp" || cmd.name == "/open" || cmd.name == "/session" || cmd.name == "/config" || cmd.name == "/exit" || cmd.name == "/cancel" || cmd.name == "/plan" || cmd.name == "/build" || cmd.name == "/help" {
				continue
			}
			event.Items = append(event.Items, pageCommand(cmd.name, cmd.help, cmd.name))
		}
		event.Items = append(event.Items, pageDetail("快捷命令与键盘", chatHelp()))
	default:
		return nil, false, nil
	}
	// These projections deliberately exclude credentials and script bodies.
	var sanitize func([]tui.MenuItem)
	sanitize = func(items []tui.MenuItem) {
		for i := range items {
			items[i].Label = a.safe(items[i].Label)
			items[i].Detail = a.safe(items[i].Detail)
			items[i].Text = a.safe(items[i].Text)
			sanitize(items[i].Children)
		}
	}
	sanitize(event.Items)
	event.Text = a.safe(event.Text)
	return []tui.Event{event}, true, nil
}
func toolMenuItems(tools []runtime.ToolInfo) []tui.MenuItem {
	items := []tui.MenuItem{}
	for _, tool := range tools {
		if tool.Identity.Source == permission.Builtin {
			continue
		}
		description := strings.Split(strings.TrimSpace(tool.Description), "\n")[0]
		items = append(items, tui.MenuItem{Label: tool.Name, Detail: shortText(description), Children: []tui.MenuItem{pageDetail("用途", tool.Description), pageDetail("参数详情", string(tool.Parameters))}})
	}
	return items
}
func capabilityMenu(label, key string, current bool, saved *bool) tui.MenuItem {
	value := current
	if saved != nil {
		value = *saved
	}
	pending := "与当前一致"
	if value != current {
		pending = "待生效"
	}
	onOff := func(value bool) string {
		if value {
			return "开启"
		}
		return "关闭"
	}
	text := fmt.Sprintf("当前实际：%s · 已保存：%s（%s）", onOff(current), onOff(value), pending)
	onLabel, offLabel := "保存为开启", "保存为关闭"
	if value {
		onLabel += " ✓ 已保存"
	} else {
		offLabel += " ✓ 已保存"
	}
	return tui.MenuItem{Label: label, Detail: text, Text: text + "\n当前会话不热切换；沿用现有权限规则。", Children: []tui.MenuItem{pageCommand(onLabel, "后续 Host 装配生效", "/tasks "+key+" enable"), pageCommand(offLabel, "后续 Host 装配生效", "/tasks "+key+" disable")}}
}

// Only mutations refresh their originating menu. Read-only command values must
// reach their result panel even when invoked from /help or another menu.
func refreshAfterMenuCommand(text string) bool {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return false
	}
	switch parts[0] {
	case "/model", "/permission", "/plan", "/build":
		return true
	case "/skills", "/hooks":
		return parts[1] == "add" || parts[1] == "edit" || parts[1] == "remove"
	case "/tasks":
		return parts[1] == "enable" || parts[1] == "disable" || parts[1] == "cancel" || (len(parts) == 3 && (parts[1] == "subagents" || parts[1] == "web_fetch") && (parts[2] == "enable" || parts[2] == "disable"))
	case "/tools", "/mcp":
		return parts[1] == "refresh" || parts[1] == "reconnect"
	case "/sessions":
		return parts[1] == "rename" || parts[1] == "archive" || parts[1] == "unarchive" || parts[1] == "export"
	}
	return false
}
