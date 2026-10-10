package cli

import (
	"CodeAgent/internal/tui"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pageFixture(t *testing.T) *terminalBackend {
	return &terminalBackend{app: productApp(t), ctx: context.Background(), published: map[string]tui.Scope{}, answered: map[string]bool{}}
}
func TestTerminalManagementPagesAndActions(t *testing.T) {
	b := pageFixture(t)
	ctx := context.Background()
	for _, name := range []string{"/help", "/model", "/sessions", "/tools", "/permission", "/skills", "/hooks", "/tasks", "/settings", "/status"} {
		events, ok, err := b.page(ctx, name)
		if err != nil || !ok || len(events) != 1 {
			t.Fatalf("%s: %v", name, err)
		}
		menu, ok := events[0].(tui.MenuEvent)
		if !ok || menu.Title == "" {
			t.Fatalf("missing %s menu", name)
		}
	}
	events, _, err := b.page(ctx, "/new")
	if err != nil {
		t.Fatal(err)
	}
	form := events[0].(tui.FormEvent)
	if len(form.Fields[1].Options) != 1 || !form.Fields[2].Boolean {
		t.Fatal("new session not using selector/toggle")
	}
	events, err = b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/permission auto_edit", Page: "/permission"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b.app.options.Policy.ApprovalMode) != "auto_edit" {
		t.Fatal("menu did not apply existing handler")
	}
	if _, ok := events[1].(tui.MenuEvent); !ok {
		t.Fatal("success does not refresh page")
	}
	events, _, err = b.page(ctx, "/tools")
	if err != nil {
		t.Fatal(err)
	}
	menu := events[0].(tui.MenuEvent)
	if menu.Items[0].Label != "MCP 服务器" {
		t.Fatal("tools grouping")
	}
	for _, item := range menu.Items[0].Children {
		if strings.Contains(item.Detail, "properties") {
			t.Fatal("schema exposed in default list")
		}
	}
}
func TestTerminalSourceFormsSaveChineseSpacePaths(t *testing.T) {
	b := pageFixture(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "中文 来源'")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("---\nname: example\ndescription: example skill\n---\nbody"), 0600); err != nil {
		t.Fatal(err)
	}
	form := sourceForm("skills", "add", 0, root)
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: form.Command + " " + root, Page: form.Page}); err != nil {
		t.Fatal(err)
	}
	if b.app.settingsDocument.Skills[0].Path != root {
		t.Fatal("source path changed")
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/skills remove 1", Page: "/skills"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatal("remove deleted source file")
	}
	hook := filepath.Join(root, "中文 hook.json")
	if err := os.WriteFile(hook, []byte(`{"hooks":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/hooks add " + hook, Page: "/hooks"}); err != nil {
		t.Fatal(err)
	}
	if b.app.settingsDocument.HookFiles[0] != hook {
		t.Fatal("hook path changed")
	}
	if b.app.capabilities.ScriptHooks {
		t.Fatal("source change falsely applied live")
	}
}
func TestTerminalSessionMenuReasonsAndTaskCapability(t *testing.T) {
	b := pageFixture(t)
	ctx := context.Background()
	events, _, err := b.page(ctx, "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	menu := events[0].(tui.MenuEvent)
	if menu.Items[0].Children[4].Disabled == "" {
		t.Fatal("current archive missing reason")
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/tasks enable", Page: "/tasks"}); err != nil {
		t.Fatal(err)
	}
	if b.app.capabilities.Subagents || b.app.settingsDocument.Subagents == nil || !*b.app.settingsDocument.Subagents {
		t.Fatal("capability must save without live apply")
	}
}

func TestTerminalPathQuoting(t *testing.T) {
	for _, pair := range [][2]string{{`"C:/中文 路径"`, "C:/中文 路径"}, {"'C:/中文 路径'", "C:/中文 路径"}, {"C:/中文 O'Reilly'", "C:/中文 O'Reilly'"}} {
		if commandPath(pair[0]) != pair[1] {
			t.Fatal("path quotes changed", pair)
		}
	}
}
