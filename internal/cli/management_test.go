package cli

import (
	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/mcp"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/tools/extend/subagent"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func productApp(t *testing.T, memorySettings ...string) *application {
	t.Helper()
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	document := "{}"
	if len(memorySettings) > 0 {
		document = memorySettings[0]
	}
	if e := os.WriteFile(settings, []byte(document), 0600); e != nil {
		t.Fatal(e)
	}
	a := &application{invocation: invocation{directory: root, settingsPath: settings}, options: Options{DataDir: filepath.Join(root, "data"), ModelEntries: []config.ModelEntry{{Ref: "fake/main", Model: llm.Model{ID: "fake", Provider: "fake", API: llm.APIKindOpenAI, ContextWindow: 8192, MaxTokens: 1024}}}, Stream: func(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}, hint: make(chan struct{}, 1), notices: make(chan string, 32)}
	if e := a.openStore(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := a.assemble(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := a.closeResources(); e != nil {
			t.Error(e)
		}
	})
	return a
}
func TestProductCommandsMemoryAndSessions(t *testing.T) {
	a := productApp(t)
	ctx := context.Background()
	if !a.capabilities.Memory {
		t.Fatal("project memory default disabled")
	}
	result, e := a.command(ctx, "/new")
	if e != nil || result.form == nil {
		t.Fatal("missing form", e)
	}
	result, e = a.command(ctx, `/new {"memory_enabled":false}`)
	if e != nil || !result.reset {
		t.Fatal(e)
	}
	disabled := a.current.SessionID()
	if a.capabilities.Memory {
		t.Fatal("new memory flag ignored")
	}
	_, e = a.command(ctx, `/new {}`)
	if e != nil || !a.capabilities.Memory {
		t.Fatal("new default", e)
	}
	_, e = a.command(ctx, "/session "+disabled)
	if e != nil || a.capabilities.Memory {
		t.Fatal("restore", e)
	}
	for _, command := range []string{"/settings", "/tools", "/sessions rename " + disabled + " 中文会话", "/sessions search 中文"} {
		if _, e = a.command(ctx, command); e != nil {
			t.Fatal(command, e)
		}
	}
	search, e := a.command(ctx, "/sessions search 中文")
	if e != nil {
		t.Fatal(e)
	}
	rows := search.value.([]sc.SessionInfo)
	if len(rows) != 1 || rows[0].ID != disabled {
		t.Fatal("search returned wrong sessions")
	}
	path := filepath.Join(t.TempDir(), "export.json")
	if _, e = a.command(ctx, "/sessions export "+disabled+" "+path); e != nil {
		t.Fatal(e)
	}
	if _, e = a.command(ctx, "/sessions export "+disabled+" "+path); e == nil {
		t.Fatal("overwritten export")
	}
	if _, e = a.command(ctx, `/new {}`); e != nil {
		t.Fatal(e)
	}
	if _, e = a.command(ctx, "/sessions archive "+disabled); e != nil {
		t.Fatal(e)
	}
	if _, e = a.command(ctx, "/sessions history "+disabled); e == nil {
		t.Fatal("fabricated archive history")
	}
	if _, e = a.command(ctx, "/sessions unarchive "+disabled); e != nil {
		t.Fatal(e)
	}
	if _, e = a.command(ctx, "/sessions history "+disabled); e != nil {
		t.Fatal(e)
	}
}
func TestProductModeAndSettingsConflict(t *testing.T) {
	a := productApp(t)
	ctx := context.Background()
	result, e := a.command(ctx, "/plan 先写计划")
	if e != nil || result.prompt != "先写计划" || a.options.Policy.WorkMode != permission.Plan {
		t.Fatal("mode order", e)
	}
	if _, e = a.command(ctx, "/permission auto_edit"); e != nil {
		t.Fatal(e)
	}
	run, e := a.current.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake"}}, Timestamp: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	result, e = a.command(ctx, "/build must not send")
	if e == nil || result.prompt != "" || a.options.Policy.WorkMode != permission.Plan {
		t.Fatal("busy command emitted prompt")
	}
	a.current.CancelRun(run)
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run.Wait(wait)
	external := []byte(`{"default_model":"external"}`)
	if e = os.WriteFile(a.settingsFile, external, 0600); e != nil {
		t.Fatal(e)
	}
	_, e = a.command(ctx, "/permission auto_approve")
	if e == nil {
		t.Fatal("settings conflict ignored")
	}
	after, _ := os.ReadFile(a.settingsFile)
	if string(after) != string(external) {
		t.Fatal("external edit lost")
	}
	if a.options.Policy.ApprovalMode == permission.AutoApprove {
		t.Fatal("policy rollback failed")
	}
}
func TestProductSourcesAndSavedCapability(t *testing.T) {
	a := productApp(t)
	ctx := context.Background()
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("---\nname: sample\ndescription: sample\n---\nbody"), 0600)
	if _, e := a.command(ctx, "/skills add "+source); e != nil {
		t.Fatal(e)
	}
	if len(a.settingsDocument.Skills) != 1 {
		t.Fatal("not saved")
	}
	if _, e := a.command(ctx, "/skills remove 1"); e != nil {
		t.Fatal(e)
	}
	hook := filepath.Join(t.TempDir(), "hooks.json")
	os.WriteFile(hook, []byte(`{"hooks":{}}`), 0600)
	if _, e := a.command(ctx, "/hooks add "+hook); e != nil {
		t.Fatal(e)
	}
	if _, e := a.command(ctx, "/tasks enable"); e != nil {
		t.Fatal(e)
	}
	if a.capabilities.Subagents {
		t.Fatal("saved capability falsely loaded")
	}
	if raw, e := os.ReadFile(a.settingsFile); e != nil || !strings.Contains(string(raw), "subagents") {
		t.Fatal("not persisted", e)
	}
}

func TestProductProjectSwitchIsolationAndPolicyReload(t *testing.T) {
	a := productApp(t, `{"memory_directory":"custom-memory","memory_scope":"custom-project"}`)
	ctx := context.Background()
	if _, e := a.command(ctx, "/permission auto_edit"); e != nil {
		t.Fatal(e)
	}
	original := a.capabilities.Instructions.ProjectRoot
	first := a.options.MemoryDirectory
	firstScope := a.options.MemoryScope
	if !strings.Contains(first, "custom-memory") || !strings.HasPrefix(firstScope, "custom-project:") {
		t.Fatal("custom base not project-bound")
	}
	other := t.TempDir()
	if _, e := newCommand(ctx, a, `{"project":"`+strings.ReplaceAll(other, `\`, `\\`)+`"}`); e != nil {
		t.Fatal(e)
	}
	if a.options.MemoryDirectory == first || a.options.MemoryScope == firstScope || a.options.Policy.ApprovalMode != permission.Default {
		t.Fatal("project memory/policy leaked")
	}
	// Reload saved policy through the real loader, not a second policy layer.
	b := &application{options: a.options, invocation: invocation{directory: original, settingsPath: a.settingsFile}, hint: make(chan struct{}, 1), notices: make(chan string, 32)}
	b.options.MemoryDirectory = ""
	b.options.MemoryScope = ""
	if e := b.openStore(ctx); e != nil {
		t.Fatal(e)
	}
	if e := b.assemble(ctx); e != nil {
		t.Fatal(e)
	}
	defer b.closeResources()
	if b.options.MemoryDirectory != first || b.options.MemoryScope != firstScope {
		t.Fatal("project memory not restored consistently")
	}
	if b.options.Policy.ApprovalMode != permission.AutoEdit {
		t.Fatal("bound policy not restored")
	}
}

func TestProductTasksEnableResultCancel(t *testing.T) {
	a := productApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, e := a.command(ctx, "/tasks enable"); e != nil {
		t.Fatal(e)
	}
	if _, e := a.command(ctx, `/new {"memory_enabled":false}`); e != nil {
		t.Fatal(e)
	}
	if !a.capabilities.Subagents {
		t.Fatal("saved enable not applied to new Host")
	}
	handle, e := a.current.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake parent"}}, Timestamp: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	session, e := a.service.OpenSession(ctx, a.current.SessionID())
	if e != nil {
		t.Fatal(e)
	}
	for {
		runs, e := session.ListUnfinishedRuns(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if len(runs) > 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	task, e := a.current.Spawn(ctx, subagent.SpawnRequest{Role: subagent.Explore, Task: "fake child"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e := a.command(ctx, "/tasks"); e != nil {
		t.Fatal(e)
	}
	if _, e := a.command(ctx, "/tasks result "+task.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := a.command(ctx, "/tasks cancel "+task.ID); e != nil {
		t.Fatal(e)
	}
	result, e := a.current.Wait(ctx, subagent.WaitRequest{IDs: []string{task.ID}, Mode: subagent.All, TimeoutMS: 2000})
	if e != nil || result.TimedOut || result.Tasks[0].State != subagent.Cancelled {
		t.Fatal("cancel did not finish", e)
	}
	if _, e := a.command(ctx, "/tasks result "+task.ID); e != nil {
		t.Fatal(e)
	}
	a.current.CancelRun(handle)
	handle.Wait(ctx)
}
func TestProductSessionExportBeyondFirstPage(t *testing.T) {
	a := productApp(t)
	ctx := context.Background()
	if _, e := a.command(ctx, `/new {"memory_enabled":false}`); e != nil {
		t.Fatal(e)
	}
	id := a.current.SessionID()
	session, e := a.service.OpenSession(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 201; i++ {
		run := fmt.Sprintf("export-run-%d", i)
		if _, e = session.StartRun(ctx, run, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake history"}}, Timestamp: time.Now()}); e != nil {
			t.Fatal(e)
		}
		if _, e = session.FinishRun(ctx, run, sc.RunCompletion{State: sc.RunStateSuccess, StopReason: llm.StopStop}); e != nil {
			t.Fatal(e)
		}
	}
	path := filepath.Join(t.TempDir(), "full.json")
	if _, e := a.command(ctx, "/sessions export "+id+" "+path); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var value map[string]json.RawMessage
	if e = json.Unmarshal(raw, &value); e != nil {
		t.Fatal(e)
	}
	var runs []sc.RunRecord
	if e = json.Unmarshal(value["runs"], &runs); e != nil || len(runs) != 201 {
		t.Fatal("export dropped later Run pages", len(runs), e)
	}
}

func TestProductHeadlessModeTextActuallySubmitted(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	if e := os.WriteFile(settings, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	var out, diag bytes.Buffer
	var submitted []string
	model := llm.Model{ID: "fake", Provider: "fake", API: llm.APIKindOpenAI, ContextWindow: 8192, MaxTokens: 1024}
	stream := func(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
		text := ""
		for _, message := range request.Messages {
			if user, ok := message.(*llm.UserMessage); ok {
				for _, block := range user.Content {
					if block.Type == llm.TypeText {
						text = block.Text
					}
				}
			}
		}
		submitted = append(submitted, text)
		raw, e := os.ReadFile(settings)
		if e != nil {
			return nil, e
		}
		var saved hostSettings
		if e = json.Unmarshal(raw, &saved); e != nil {
			return nil, e
		}
		expected := permission.Plan
		if len(submitted) == 2 {
			expected = permission.Build
		}
		if saved.Policy.WorkMode != expected {
			t.Errorf("model called before mode saved: %s", saved.Policy.WorkMode)
		}
		message := llm.AssistantMessage{Role: llm.RoleAssistant, Provider: model.Provider, API: model.API, Model: model.ID, State: llm.StateSuccess, StopReason: llm.StopStop, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake final"}}, Timestamp: time.Now()}
		reader, writer := llm.NewEventStream(ctx, message)
		writer.Finish(message)
		return reader, nil
	}
	code := Execute(context.Background(), []string{"chat", "--stdin", "--no-memory", "--directory", root, "--settings", settings, "--data-dir", filepath.Join(root, "data")}, IO{In: strings.NewReader("/plan first\n/build second\n/exit\n"), Out: &out, Err: &diag}, Options{ModelEntries: []config.ModelEntry{{Ref: "fake/main", Model: model}}, Stream: stream})
	if code != ExitOK || len(submitted) != 2 || submitted[0] != "first" || submitted[1] != "second" {
		t.Fatalf("code=%d submitted=%v diagnostic=%s", code, submitted, diag.String())
	}
}

func TestProductToolsGroupInstalledSnapshot(t *testing.T) {
	tool := runtime.ToolInfo{Name: "installed-old", Parameters: json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object"}}}`), Identity: permission.Tool{Source: permission.External, SourceID: "server/2", ToolID: "real-name"}}
	builtins, groups, other := groupAppliedTools([]runtime.ToolInfo{tool}, []mcp.ServerStatus{{ID: "server"}, {ID: "disconnected"}})
	if len(builtins) != 0 || len(other) != 0 || len(groups) != 2 || len(groups[0].AppliedTools) != 1 || groups[0].AppliedTools[0].Name != "installed-old" || len(groups[1].AppliedTools) != 0 {
		t.Fatal("server grouping invented or lost installed tools")
	}
}

func TestProductExecutableDefaultSettingsAndInvalidFile(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasSuffix(filepath.Base(exe), ".test.exe") && !strings.HasSuffix(filepath.Base(exe), ".test") {
		t.Skip("only writes beside a temporary Go test executable")
	}
	rel, e := filepath.Rel(os.TempDir(), filepath.Dir(exe))
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Skip("test executable is outside temporary directory")
	}
	path := filepath.Join(filepath.Dir(exe), "settings.json")
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Skip("existing executable-side settings are preserved")
	}
	defer os.Remove(path)
	if _, e = f.Write([]byte(`{"subagents":true}`)); e != nil {
		f.Close()
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	a := &application{}
	if e = a.loadSettings(); e != nil || !a.options.Subagents || a.settingsFile != path {
		t.Fatal("default settings not loaded", e)
	}
	if e = a.loadSettings(); e != nil {
		t.Fatal("loaded settings reloaded", e)
	}
	if e = os.WriteFile(path, []byte(`{"unknown":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	b := &application{}
	if e = b.loadSettings(); e == nil {
		t.Fatal("invalid existing default silently ignored")
	}
}
