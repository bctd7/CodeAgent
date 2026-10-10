package cli

import (
	"CodeAgent/internal/tui"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWebFetchCapabilitySaveAndDeferredAssembly(t *testing.T) {
	b := pageFixture(t)
	a := b.app
	ctx := context.Background()
	if a.options.WebFetch != nil || a.capabilities.WebFetch {
		t.Fatal("default must be disabled")
	}
	for _, op := range []string{"web_fetch enable", "subagents enable"} {
		if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/tasks " + op, Page: "/tasks"}); err != nil {
			t.Fatal(err)
		}
	}
	if a.capabilities.WebFetch || a.capabilities.Subagents || a.options.WebFetch == nil {
		t.Fatal("saved setting falsely applied live")
	}
	data, err := os.ReadFile(a.settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["web_fetch"] != true || saved["subagents"] != true {
		t.Fatal(saved)
	}
	events, _, err := b.page(ctx, "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	menu := events[0].(tui.MenuEvent)
	if len(menu.Items) != 2 || len(menu.Items[0].Children) != 2 || menu.Items[1].Label != "子任务列表" || len(menu.Items[1].Children) != 0 {
		t.Fatal("web fetch invented a task or groups missing")
	}
	for _, item := range menu.Items[0].Children {
		if !strings.Contains(item.Detail, "当前实际：关闭") || !strings.Contains(item.Detail, "待生效") {
			t.Fatal(item.Detail)
		}
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/session " + a.current.SessionID()}); err != nil {
		t.Fatal(err)
	}
	if a.capabilities.WebFetch {
		t.Fatal("same session applied web fetch")
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: `/new {"memory_enabled":false}`}); err != nil {
		t.Fatal(err)
	}
	if !a.capabilities.WebFetch || !a.capabilities.Subagents {
		t.Fatal("new Host failed to apply capability settings")
	}
	catalog, err := a.current.ToolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range catalog {
		names[tool.Name] = true
	}
	if !names["webfetch"] || !names["spawn_agent"] {
		t.Fatal("actual tools not assembled")
	}
	if _, err := b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: "/tasks web_fetch disable", Page: "/tasks"}); err != nil {
		t.Fatal(err)
	}
	if !a.capabilities.WebFetch || a.options.WebFetch != nil || !a.capabilities.Subagents {
		t.Fatal("disable altered current Runtime or unrelated subagents")
	}
}
func TestWebFetchSettingsLoadTrueFalseAndConflict(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		document := `{"web_fetch":false}`
		if enabled {
			document = `{"web_fetch":true}`
		}
		a := productApp(t, document)
		if a.capabilities.WebFetch != enabled || (a.options.WebFetch != nil) != enabled {
			t.Fatal("settings not loaded", enabled)
		}
	}
	b := pageFixture(t)
	a := b.app
	if err := os.WriteFile(a.settingsFile, []byte(`{"subagents":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.handle(context.Background(), tui.Action{Kind: tui.ExecuteCommand, Text: "/tasks web_fetch enable", Page: "/tasks"}); err == nil {
		t.Fatal("external settings conflict ignored")
	}
	if a.options.WebFetch != nil || a.capabilities.WebFetch {
		t.Fatal("failed save partially applied")
	}
	data, err := os.ReadFile(a.settingsFile)
	if err != nil || string(data) != `{"subagents":true}` {
		t.Fatal("external settings overwritten")
	}
}
