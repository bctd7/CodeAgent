package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"CodeAgent/internal/hooks"
	"CodeAgent/internal/instructions"
	"CodeAgent/internal/mcp"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/skills"
	"CodeAgent/internal/tools/extend/subagent"
	"CodeAgent/internal/tools/extend/webfetch"
	"CodeAgent/internal/tui"
)

// Only the existing settings file is written. Compare the loaded document before
// replacing it so an external edit cannot silently disappear.
func (a *application) saveSetting(key string, value any) error {
	path := a.settingsFile
	if path == "" {
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		path = filepath.Join(filepath.Dir(exe), "settings.json")
	}
	current, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		return fmt.Errorf("cannot read settings")
	}
	if !bytes.Equal(current, a.settingsBytes) {
		return fmt.Errorf("settings changed externally; restart before saving")
	}
	document := map[string]json.RawMessage{}
	if len(current) > 0 {
		if e = json.Unmarshal(current, &document); e != nil {
			return fmt.Errorf("invalid settings")
		}
	}
	encoded, e := json.Marshal(value)
	if e != nil {
		return e
	}
	document[key] = encoded
	data, e := json.MarshalIndent(document, "", "  ")
	if e != nil {
		return e
	}
	data = append(data, '\n')
	var settings hostSettings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&settings); e != nil {
		return fmt.Errorf("invalid settings update")
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	latest, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if !bytes.Equal(latest, current) {
		return fmt.Errorf("settings changed while saving; no replacement made")
	}
	if e = os.Rename(temp, path); e != nil {
		return e
	}
	a.settingsFile = path
	a.settingsBytes = data
	a.settingsDocument = settings
	return nil
}

func (a *application) settingsView(ctx context.Context) (any, error) {
	result, e := instructions.Load(ctx, a.capabilities.Instructions)
	documents := []map[string]any{}
	for _, d := range result.Documents {
		documents = append(documents, map[string]any{"path": d.Path, "scope": d.Scope})
	}
	diagnostics := []string{}
	for _, d := range result.Diagnostics {
		diagnostics = append(diagnostics, d.Path+": "+d.Err.Error())
	}
	if e != nil {
		diagnostics = append(diagnostics, e.Error())
	}
	info, e := a.service.GetSession(ctx, a.current.SessionID())
	if e != nil {
		return nil, e
	}
	return map[string]any{"model_config": a.configFile, "settings_file": a.settingsFile, "settings_default": "settings.json next to executable", "project": a.capabilities.Instructions.ProjectRoot, "session_memory_enabled": info.MemoryEnabled, "effective": a.capabilities, "memory_directory": a.options.MemoryDirectory, "agents_current_files": documents, "agents_current_diagnostics": diagnostics, "agents_check": "current read; prompt loaded at Runtime initialization", "policy": a.options.Policy, "saved_policy_project": a.settingsDocument.Policy.WorkingDir, "saved_subagents": a.settingsDocument.Subagents, "source_updates": "source changes require restart; current Runtime keeps loaded sources"}, nil
}
func (a *application) toolsView(ctx context.Context) (any, error) {
	catalog, e := a.current.ToolCatalog()
	if e != nil {
		return nil, e
	}
	statuses := []mcp.ServerStatus{}
	if a.manager != nil {
		statuses = a.manager.Statuses()
	}
	builtins, groups, other := groupAppliedTools(catalog, statuses)
	return map[string]any{"builtin_tools": builtins, "mcp_servers": groups, "other_external_tools": other, "applied_tools": catalog, "mcp_actions": "/tools refresh SERVER_ID | /tools reconnect SERVER_ID", "mcp_configuration": "server add/edit/delete/enable and TOML saving deferred", "builtins": "default enabled; descriptions and parameter schemas are the installed definitions"}, nil
}
func toolsCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if arg != "" {
		return mcpCommand(ctx, a, arg)
	}
	value, e := a.toolsView(ctx)
	return commandResult{value: value}, e
}
func modeCommand(mode string) func(context.Context, *application, string) (commandResult, error) {
	return func(ctx context.Context, a *application, arg string) (commandResult, error) {
		if a.busy() {
			return commandResult{}, runtime.ErrBusy
		}
		old := a.options.Policy
		p := old
		p.WorkingDir = a.capabilities.Instructions.ProjectRoot
		p.WorkMode = permission.WorkMode(mode)
		if e := a.current.SetPolicy(ctx, p); e != nil {
			return commandResult{}, e
		}
		if e := a.saveSetting("policy", p); e != nil {
			rollback := a.current.SetPolicy(ctx, old)
			return commandResult{}, fmt.Errorf("mode not saved: %w; rollback: %v", e, rollback)
		}
		a.options.Policy = p
		a.capabilities.WorkMode = p.WorkMode
		return commandResult{value: map[string]any{"work_mode": mode}, prompt: arg}, nil
	}
}
func policyCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if arg == "" {
		return commandResult{value: map[string]any{"project": a.capabilities.Instructions.ProjectRoot, "policy": a.options.Policy, "usage": "/permission default|auto_edit|auto_approve", "temporary_authorizations": "cleared by policy change and Runtime close"}}, nil
	}
	if a.busy() {
		return commandResult{}, runtime.ErrBusy
	}
	old := a.options.Policy
	p := old
	p.WorkingDir = a.capabilities.Instructions.ProjectRoot
	p.ApprovalMode = permission.ApprovalMode(arg)
	if e := a.current.SetPolicy(ctx, p); e != nil {
		return commandResult{}, e
	}
	if e := a.saveSetting("policy", p); e != nil {
		rollback := a.current.SetPolicy(ctx, old)
		return commandResult{}, fmt.Errorf("policy not saved: %w; rollback: %v", e, rollback)
	}
	a.options.Policy = p
	a.capabilities.ApprovalMode = p.ApprovalMode
	return commandResult{value: p}, nil
}

type newSessionOptions struct {
	Project       string `json:"project"`
	Model         string `json:"model"`
	MemoryEnabled *bool  `json:"memory_enabled"`
}

func newCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if arg == "" {
		return commandResult{value: "/new {\"project\":\"DIR\",\"model\":\"REF\",\"memory_enabled\":true}", form: &tui.FormEvent{Title: "新会话", Command: "/new", Fields: []tui.FormField{{Name: "project", Label: "项目目录", Value: a.capabilities.Instructions.ProjectRoot}, {Name: "model", Label: "模型 REF（" + strings.Join(a.modelRefs, ", ") + "）", Value: a.selectedRef}, {Name: "memory_enabled", Label: "长期记忆 true/false", Value: "true", Boolean: true}}}}, nil
	}
	var options newSessionOptions
	d := json.NewDecoder(strings.NewReader(arg))
	d.DisallowUnknownFields()
	if e := d.Decode(&options); e != nil {
		return commandResult{}, fmt.Errorf("/new expects JSON project/model/memory_enabled")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return commandResult{}, fmt.Errorf("/new expects one JSON object")
	}
	root := options.Project
	if root == "" {
		root = a.capabilities.Instructions.ProjectRoot
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(a.capabilities.Instructions.ProjectRoot, root)
	}
	model := options.Model
	if model == "" {
		model = a.selectedRef
	}
	valid := false
	for _, ref := range a.modelRefs {
		if ref == model {
			valid = true
		}
	}
	if !valid {
		return commandResult{}, fmt.Errorf("model REF not configured")
	}
	enabled := true
	if options.MemoryEnabled != nil {
		enabled = *options.MemoryEnabled
	}
	oldModel, oldDisabled := a.selectedRef, a.invocation.memoryDisabled
	a.selectedRef = model
	a.invocation.memoryDisabled = !enabled
	result, e := switchCommand(ctx, a, root, "")
	if e != nil {
		a.selectedRef = oldModel
		a.invocation.memoryDisabled = oldDisabled
	}
	return result, e
}

func sessionsCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if arg == "" {
		return viewCommand("sessions")(ctx, a, "")
	}
	operation, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	if operation == "search" || operation == "all" || operation == "archived" {
		filter := sc.ArchiveFilterActive
		if operation == "all" {
			filter = sc.ArchiveFilterAll
		}
		if operation == "archived" {
			filter = sc.ArchiveFilterArchived
		}
		items := []sc.SessionInfo{}
		for offset := 0; ; offset += 200 {
			page, e := a.service.ListSessions(ctx, sc.SessionListOptions{ArchiveFilter: filter, TitleSearch: rest, Limit: 200, Offset: offset})
			if e != nil {
				return commandResult{}, e
			}
			for _, s := range page {
				child, e := a.isChild(ctx, s.ID)
				if e != nil {
					return commandResult{}, e
				}
				if !child {
					items = append(items, s)
				}
			}
			if len(page) < 200 {
				break
			}
		}
		return commandResult{value: items}, nil
	}
	id, tail, _ := strings.Cut(rest, " ")
	tail = strings.TrimSpace(tail)
	if id == "" {
		return commandResult{}, fmt.Errorf("session ID required")
	}
	child, e := a.isChild(ctx, id)
	if e != nil {
		return commandResult{}, e
	}
	if child {
		return commandResult{}, fmt.Errorf("manage child sessions through /tasks")
	}
	switch operation {
	case "rename":
		if tail == "" {
			return commandResult{form: &tui.FormEvent{Title: "重命名会话", Command: "/sessions rename " + id, Plain: true, Fields: []tui.FormField{{Name: "title", Label: "标题"}}}}, nil
		}
		e = a.service.RenameSession(ctx, id, tail)
	case "archive":
		if id == a.current.SessionID() {
			return commandResult{}, fmt.Errorf("switch to another session before archiving current session")
		}
		e = a.service.ArchiveSession(ctx, id)
	case "unarchive":
		e = a.service.UnarchiveSession(ctx, id)
	case "history", "export":
		s, e := a.service.OpenSession(ctx, id)
		if e != nil {
			return commandResult{}, fmt.Errorf("history requires an active session; unarchive first: %w", e)
		}
		entries, e := s.GetCurrentHistory(ctx)
		if e != nil {
			return commandResult{}, e
		}
		info, e := s.GetInfo(ctx)
		if e != nil {
			return commandResult{}, e
		}
		runs := []sc.RunRecord{}
		for offset := 0; ; offset += 200 {
			page, e := s.ListRuns(ctx, 200, offset)
			if e != nil {
				return commandResult{}, e
			}
			runs = append(runs, page...)
			if len(page) < 200 {
				break
			}
		}
		value := map[string]any{"session": info, "history": entries, "runs": runs}
		if operation == "history" {
			return commandResult{value: value}, nil
		}
		if tail == "" {
			return commandResult{form: &tui.FormEvent{Title: "导出会话", Command: "/sessions export " + id, Plain: true, Fields: []tui.FormField{{Name: "path", Label: "JSON 文件绝对路径"}}}}, nil
		}
		path := commandPath(tail)
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.capabilities.Instructions.ProjectRoot, path)
		}
		data, e := json.MarshalIndent(value, "", "  ")
		if e != nil {
			return commandResult{}, e
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return commandResult{}, e
		}
		_, writeErr := f.Write([]byte(a.safe(string(data))))
		closeErr := f.Close()
		if writeErr != nil {
			return commandResult{}, writeErr
		}
		if closeErr != nil {
			return commandResult{}, closeErr
		}
		return commandResult{value: map[string]string{"exported": path}}, nil
	default:
		return commandResult{}, fmt.Errorf("use /sessions search TEXT | all | archived | rename ID TITLE | archive ID | unarchive ID | history ID | export ID FILE")
	}
	if e != nil {
		return commandResult{}, e
	}
	return commandResult{value: map[string]string{"action": operation, "session": id}}, nil
}
func tasksCommand(ctx context.Context, a *application, arg string) (commandResult, error) {
	if arg == "" {
		return viewCommand("tasks")(ctx, a, "")
	}
	parts := strings.Fields(arg)
	capability, mode := "subagents", arg
	if len(parts) == 2 && (parts[0] == "subagents" || parts[0] == "web_fetch") {
		capability, mode = parts[0], parts[1]
	}
	if mode == "enable" || mode == "disable" {
		if a.busy() {
			return commandResult{}, runtime.ErrBusy
		}
		enabled := mode == "enable"
		if err := a.saveSetting(capability, enabled); err != nil {
			return commandResult{}, err
		}
		if capability == "subagents" {
			a.options.Subagents = enabled
		} else if enabled {
			if a.options.WebFetch == nil {
				a.options.WebFetch = &webfetch.WebFetchOptions{}
			}
		} else {
			a.options.WebFetch = nil
		}
		return commandResult{value: "已保存能力设置；新建会话、切到另一会话再返回，或重启后装配生效。直接选择同一会话不生效。"}, nil
	}

	operation, id, _ := strings.Cut(arg, " ")
	id = strings.TrimSpace(id)
	if operation == "cancel" {
		task, e := a.current.Cancel(ctx, subagent.CancelRequest{ID: id})
		return commandResult{value: task}, e
	}
	if operation != "result" {
		return commandResult{}, fmt.Errorf("use /tasks result ID | cancel ID | enable | disable | web_fetch enable | web_fetch disable")
	}
	s, e := a.service.OpenSession(ctx, a.current.SessionID())
	if e != nil {
		return commandResult{}, e
	}
	task, e := s.GetChildTask(ctx, id)
	if e != nil {
		return commandResult{}, e
	}
	child, e := a.service.OpenSession(ctx, task.ChildSessionID)
	if e != nil {
		return commandResult{}, e
	}
	runs, e := child.ListRuns(ctx, 1, 0)
	if e != nil {
		return commandResult{}, e
	}
	if len(runs) == 0 {
		return commandResult{value: map[string]any{"task": task, "result": "not started"}}, nil
	}
	result, e := child.GetRun(ctx, runs[0].ID)
	return commandResult{value: map[string]any{"task": task, "latest_run": result}}, e
}

func sourcesCommand(kind string) func(context.Context, *application, string) (commandResult, error) {
	return func(ctx context.Context, a *application, arg string) (commandResult, error) {
		if arg == "" {
			value, e := a.commandView(ctx, kind)
			return commandResult{value: map[string]any{"loaded": value, "saved_skills": a.settingsDocument.Skills, "saved_hook_files": a.settingsDocument.HookFiles, "usage": "/" + kind + " add PATH | edit INDEX PATH | remove INDEX; source changes require restart"}}, e
		}
		if a.busy() {
			return commandResult{}, runtime.ErrBusy
		}
		op, rest, _ := strings.Cut(arg, " ")
		rest = strings.TrimSpace(rest)
		index := -1
		if op == "edit" || op == "remove" {
			number, tail, _ := strings.Cut(rest, " ")
			n, e := strconv.Atoi(number)
			if e != nil || n < 1 {
				return commandResult{}, fmt.Errorf("source index starts at 1")
			}
			index = n - 1
			rest = strings.TrimSpace(tail)
		}
		if op != "add" && op != "edit" && op != "remove" {
			return commandResult{}, fmt.Errorf("use add PATH | edit INDEX PATH | remove INDEX")
		}
		path := ""
		if op != "remove" {
			path = commandPath(rest)
			if path == "" {
				command := "/" + kind + " " + op
				if op == "edit" {
					command += " " + strconv.Itoa(index+1)
				}
				return commandResult{form: &tui.FormEvent{Title: "来源目录/文件", Command: command, Plain: true, Fields: []tui.FormField{{Name: "path", Label: "路径"}}}}, nil
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(a.capabilities.Instructions.ProjectRoot, path)
			}
			var e error
			path, e = filepath.Abs(path)
			if e != nil {
				return commandResult{}, e
			}
		}
		if kind == "skills" {
			sources := append([]skills.Source(nil), a.settingsDocument.Skills...)
			if op != "add" && index >= len(sources) {
				return commandResult{}, fmt.Errorf("source index out of range")
			}
			if op != "remove" {
				info, e := os.Stat(path)
				if e != nil || !info.IsDir() {
					return commandResult{}, fmt.Errorf("skill source must be an existing directory")
				}
			}
			source := skills.Source{Path: path, Name: filepath.Base(path)}
			switch op {
			case "add":
				sources = append(sources, source)
			case "edit":
				sources[index] = source
			case "remove":
				sources = append(sources[:index], sources[index+1:]...)
			}
			result := skills.Load(sources)
			if e := a.saveSetting("skills", sources); e != nil {
				return commandResult{}, e
			}
			return commandResult{value: map[string]any{"saved": sources, "diagnostics": result.Diagnostics, "apply": "restart required to replace loaded sources"}}, nil
		}
		files := append([]string(nil), a.settingsDocument.HookFiles...)
		if op != "add" && index >= len(files) {
			return commandResult{}, fmt.Errorf("source index out of range")
		}
		switch op {
		case "add":
			files = append(files, path)
		case "edit":
			files[index] = path
		case "remove":
			files = append(files[:index], files[index+1:]...)
		}
		sources := []hooks.Source{}
		for _, file := range files {
			if !filepath.IsAbs(file) {
				file = filepath.Join(filepath.Dir(a.settingsFile), file)
			}
			data, e := os.ReadFile(file)
			if e != nil || len(data) > 1<<20 {
				return commandResult{}, fmt.Errorf("hooks source unreadable or too large")
			}
			sources = append(sources, hooks.Source{ID: file, Kind: hooks.SettingsSource, JSON: data})
		}
		if len(sources) > 0 {
			if _, e := hooks.New(sources, a.options.HookOptions); e != nil {
				return commandResult{}, e
			}
		}
		if e := a.saveSetting("hook_files", files); e != nil {
			return commandResult{}, e
		}
		return commandResult{value: map[string]any{"saved": files, "apply": "restart required to replace loaded sources"}}, nil
	}
}

func sessionDetail(s sc.SessionInfo) string {
	state := "active"
	if s.ArchivedAt != nil {
		state = "archived"
	}
	return state + " · " + s.WorkingDir
}
func (a *application) listMainSessions(ctx context.Context, search, filter string) ([]sc.SessionInfo, error) {
	var out []sc.SessionInfo
	for offset := 0; ; offset += 200 {
		page, e := a.service.ListSessions(ctx, sc.SessionListOptions{ArchiveFilter: sc.ArchiveFilter(filter), TitleSearch: search, Limit: 200, Offset: offset})
		if e != nil {
			return nil, e
		}
		for _, s := range page {
			child, e := a.isChild(ctx, s.ID)
			if e != nil {
				return nil, e
			}
			if !child {
				out = append(out, s)
			}
		}
		if len(page) < 200 {
			break
		}
	}
	return out, nil
}

type serverToolGroup struct {
	Status       mcp.ServerStatus   `json:"status"`
	AppliedTools []runtime.ToolInfo `json:"applied_tools"`
}

// Server status may advance before a Runtime refresh; only its installed snapshot
// contributes tool names and schemas to these groups.
func groupAppliedTools(catalog []runtime.ToolInfo, statuses []mcp.ServerStatus) ([]runtime.ToolInfo, []serverToolGroup, []runtime.ToolInfo) {
	builtins := []runtime.ToolInfo{}
	other := []runtime.ToolInfo{}
	groups := make([]serverToolGroup, len(statuses))
	indices := map[string]int{}
	for i, status := range statuses {
		groups[i] = serverToolGroup{Status: status, AppliedTools: []runtime.ToolInfo{}}
		indices[status.ID] = i
	}
	for _, tool := range catalog {
		if tool.Identity.Source == permission.Builtin {
			builtins = append(builtins, tool)
			continue
		}
		source := tool.Identity.SourceID
		last := strings.LastIndex(source, "/")
		if last >= 0 {
			source = source[:last]
		}
		if i, ok := indices[source]; ok {
			groups[i].AppliedTools = append(groups[i].AppliedTools, tool)
		} else {
			other = append(other, tool)
		}
	}
	return builtins, groups, other
}

// Strip only a matching wrapper, preserving apostrophes inside or at the end
// of paths passed by the TUI forms.
func commandPath(value string) string {
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '"' || value[0] == 39) {
		return value[1 : len(value)-1]
	}
	return value
}
