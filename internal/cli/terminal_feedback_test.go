package cli

import (
	"CodeAgent/internal/llm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/tui"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToolsOnlyExternalDisplayLeavesCatalogUnchanged(t *testing.T) {
	a := productApp(t, `{"subagents":true}`)
	before, err := a.current.ToolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(before)
	if len(toolMenuItems(before)) != 0 {
		t.Fatal("builtin tools still shown")
	}
	after, err := a.current.ToolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	encodedAfter, _ := json.Marshal(after)
	if !bytes.Equal(encoded, encodedAfter) || len(before) == 0 {
		t.Fatal("display changed actual model catalog")
	}
	tools := []runtime.ToolInfo{{Name: "read", Description: "External original English", Parameters: json.RawMessage(`{"description":"original external"}`), Identity: permission.Tool{Source: permission.External, SourceID: "project-memory"}}, {Name: "custom", Identity: permission.Tool{Source: permission.Builtin, SourceID: "custom-source"}}}
	items := toolMenuItems(tools)
	if len(items) != 1 || items[0].Detail != "External original English" || items[0].Children[1].Text != string(tools[0].Parameters) {
		t.Fatal("external content changed or builtin not hidden")
	}
	b := &terminalBackend{app: a}
	events, _, err := b.page(context.Background(), "/tools")
	if err != nil {
		t.Fatal(err)
	}
	menu := events[0].(tui.MenuEvent)
	if !strings.Contains(menu.Text, "未配置 MCP") || len(menu.Items) != 1 || menu.Items[0].Label != "MCP 服务器" {
		t.Fatal("empty MCP unclear", menu)
	}
}
func TestReadOnlyMenuResultNotSwallowedAndMemoryEntryRemoved(t *testing.T) {
	b := pageFixture(t)
	ctx := context.Background()
	pages, _, err := b.page(ctx, "/help")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pages[0].(tui.MenuEvent).Items {
		if item.Command == "/memory" {
			t.Fatal("memory browser advertised")
		}
	}
	events, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/memory", Page: "/help"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if panel, ok := event.(tui.PanelEvent); ok {
			found = panel.Title == "/memory"
		}
		if _, ok := event.(tui.MenuEvent); ok {
			t.Fatal("query replaced with help")
		}
	}
	if !found {
		t.Fatal("read-only result missing")
	}
	for _, text := range []string{"/memory", "/sessions history s", "/tasks result t", "/sessions search text"} {
		if refreshAfterMenuCommand(text) {
			t.Fatal("read-only command refreshes", text)
		}
	}
	for _, text := range []string{"/permission auto_edit", "/skills add dir", "/tasks enable", "/sessions archive s", "/tools refresh server"} {
		if !refreshAfterMenuCommand(text) {
			t.Fatal("mutation does not refresh", text)
		}
	}
}
func TestTaskCapabilityPendingSameSessionDoesNotApply(t *testing.T) {
	b := pageFixture(t)
	ctx := context.Background()
	id := b.app.current.SessionID()
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/tasks enable", Page: "/tasks"}); err != nil {
		t.Fatal(err)
	}
	events, _, err := b.page(ctx, "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	menu := events[0].(tui.MenuEvent)
	text := menu.Text + menu.Items[0].Children[0].Detail
	if !strings.Contains(text, "当前实际：关闭") || !strings.Contains(text, "已保存：开启（待生效）") || !strings.Contains(text, "同一会话不会重新装配") {
		t.Fatal(text)
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/session " + id}); err != nil {
		t.Fatal(err)
	}
	if b.app.capabilities.Subagents {
		t.Fatal("same session falsely applied")
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: `/new {"memory_enabled":false}`}); err != nil {
		t.Fatal(err)
	}
	if !b.app.capabilities.Subagents {
		t.Fatal("new host did not apply saved setting")
	}
}

func TestHostContextDisplayUsesStoredSource(t *testing.T) {
	a := productApp(t)
	ctx := context.Background()
	s, err := a.service.OpenSession(ctx, a.current.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	user := &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "子任务结果：this is actual user text"}}, Timestamp: time.Now()}
	if _, err = s.StartRun(ctx, "source-display", user); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendContext(ctx, "source-display", sc.ContextInput{SourceKind: "test_host", SourceID: "source-event", Message: &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "host feedback"}}, Timestamp: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(ctx, "source-display", sc.RunCompletion{State: sc.RunStateSuccess, StopReason: llm.StopStop}); err != nil {
		t.Fatal(err)
	}
	id := a.current.SessionID()
	if err = a.current.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a.current, err = a.host.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b := &terminalBackend{app: a, ctx: ctx}
	snap := b.snapshot()
	users, notices := 0, 0
	for _, item := range snap.Items {
		if strings.Contains(item.Text, "actual user text") {
			if item.Kind != tui.UserItem {
				t.Fatal("user prefix mistaken for context")
			}
			users++
		}
		if item.Text == "host feedback" {
			if item.Kind == tui.UserItem {
				t.Fatal("host context labelled as user")
			}
			notices++
		}
	}
	if users != 1 || notices != 1 {
		t.Fatal("source projection missing", users, notices)
	}
}

func TestTaskPanelsAndOldChildContextDoNotExposeInternalBlocks(t *testing.T) {
	content := []llm.ContentBlock{{Type: llm.TypeThinking, Thinking: "private-thinking", ThinkingSignature: "private-signature"}, {Type: llm.TypeText, Text: "public child result", TextSignature: "private-text-signature"}}
	detail := sc.RunDetail{Record: sc.RunRecord{ID: "run", SessionID: "child", State: sc.RunStateSuccess}, Entries: []sc.Entry{{Message: &llm.AssistantMessage{Role: llm.RoleAssistant, Content: content}}}}
	panel, err := formatCommandValue(map[string]any{"task": sc.ChildTask{ChildTaskInput: sc.ChildTaskInput{ID: "task", Role: "explore"}}, "latest_run": detail})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": "task", "state": "completed", "run_id": "run", "session_id": "child", "result": content})
	legacy := formatChildContext("子任务结果：" + string(raw))
	for _, text := range []string{panel, legacy} {
		if strings.Contains(text, "private-") || strings.Contains(text, "Thinking") || !strings.Contains(text, "public child result") || !strings.Contains(text, "run") {
			t.Fatal("unsafe or incomplete task display", text)
		}
	}
}

func TestChildContextHistoryLabelUsesMetadataNotPrefix(t *testing.T) {
	a := productApp(t)
	raw, _ := json.Marshal(map[string]any{"id": "task", "state": "completed", "run_id": "run", "session_id": "child", "result": []llm.ContentBlock{{Type: llm.TypeThinking, Thinking: "private-thinking"}, {Type: llm.TypeText, Text: "public"}}})
	user := &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "子任务结果：actual user input"}}}
	host := &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "子任务结果：" + string(raw)}}}
	records := []sc.ContextRecord{{Kind: sc.MessageEntry, Message: user}, {Kind: sc.ContextEntry, SourceKind: "child_run", SourceID: "run", Message: host}}
	items := historyItems(a, []llm.Message{user, host}, records)
	if len(items) != 2 || items[0].Kind != tui.UserItem || items[1].Kind != tui.NoticeItem {
		t.Fatal("source kinds lost", items)
	}
	if strings.Contains(items[1].Text, "private-thinking") || !strings.Contains(items[1].Text, "public") {
		t.Fatal("child context not projected")
	}
}
