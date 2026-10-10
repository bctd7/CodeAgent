package tui

import (
	"context"
	"encoding/json"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"
)

func menuKeyMsg(m *Model, key tea.KeyType) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: key})
	return cmd
}
func TestMenuSearchFilterReturnAndResize(t *testing.T) {
	m := New(context.Background(), Config{})
	page := MenuEvent{Page: "/sessions", Title: "会话", Search: true, Filters: []string{"全部", "活动", "归档"}, Items: []MenuItem{{Label: "中文项目", Filter: "活动", Children: []MenuItem{{Label: "历史", Text: "完整详情"}}}, {Label: "归档项目", Filter: "归档"}}}
	m.apply(page)
	menuKeyMsg(m, tea.KeyTab)
	if len(m.dialog.menuItems) != 1 {
		t.Fatal("archive filter")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	root := m.dialog
	menuKeyMsg(m, tea.KeyEnter)
	if m.dialog.parent != root {
		t.Fatal("child navigation")
	}
	menuKeyMsg(m, tea.KeyEnter)
	if m.dialog.kind != "panel" {
		t.Fatal("detail")
	}
	menuKeyMsg(m, tea.KeyEnter)
	if m.dialog.parent != root {
		t.Fatal("detail Enter does not return")
	}
	menuKeyMsg(m, tea.KeyEsc)
	if m.dialog != root || m.dialog.filter != "中文" {
		t.Fatal("search lost")
	}
	for _, size := range [][2]int{{8, 8}, {44, 14}, {80, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, line := range strings.Split(m.View(), "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("width overflow")
			}
		}
	}
	m.apply(page)
	if m.dialog.filter != "中文" || m.dialog.filterIndex != 1 {
		t.Fatal("refresh lost navigation")
	}
	menuKeyMsg(m, tea.KeyEsc)
	if m.dialog != nil {
		t.Fatal("root return")
	}
}
func TestMenuFormsSelectionToggleErrorsAndDuplicateSubmission(t *testing.T) {
	calls := 0
	var action Action
	m := New(context.Background(), Config{Handle: func(_ context.Context, a Action) ([]Event, error) {
		calls++
		action = a
		return nil, errors.New("fake save failure")
	}})
	form := FormEvent{Title: "新会话", Command: "/new", Fields: []FormField{{Name: "project", Label: "项目", Value: "C:/中文 路径"}, {Name: "model", Label: "模型", Value: "fake/one", Options: []Choice{{ID: "fake/one", Label: "模型一"}, {ID: "fake/two", Label: "模型二"}}}, {Name: "memory_enabled", Label: "记忆", Value: "true", Boolean: true}}}
	m.apply(form)
	d := m.dialog
	menuKeyMsg(m, tea.KeyTab)
	menuKeyMsg(m, tea.KeyEnter)
	menuKeyMsg(m, tea.KeyDown)
	menuKeyMsg(m, tea.KeyEnter)
	if m.dialog != d || d.fields[1].Value() != "fake/two" {
		t.Fatal("model selection")
	}
	menuKeyMsg(m, tea.KeyTab)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	cmd := menuKeyMsg(m, tea.KeyEnter)
	if cmd == nil {
		t.Fatal("no submit")
	}
	if menuKeyMsg(m, tea.KeyEnter) != nil {
		t.Fatal("duplicate submit")
	}
	m.Update(cmd())
	if calls != 1 || action.Kind != ExecuteCommand || m.dialog != d || d.selected != 2 {
		t.Fatal("failed save lost inputs or incorrect action")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(action.Text, "/new ")), &value); err != nil {
		t.Fatal(err)
	}
	if value["project"] != "C:/中文 路径" || value["model"] != "fake/two" || value["memory_enabled"] != false {
		t.Fatal(value)
	}
	menuKeyMsg(m, tea.KeyEsc)
	if m.dialog != nil {
		t.Fatal("cancel")
	}
}
func TestMenuUnavailableOperationAndHelpReturn(t *testing.T) {
	m := New(context.Background(), Config{})
	help := MenuEvent{Page: "/help", Title: "帮助", Items: []MenuItem{{Label: "权限", Command: "/permission"}}}
	m.apply(help)
	parent := m.dialog
	m.apply(MenuEvent{Page: "/permission", Title: "权限", Items: []MenuItem{{Label: "修改", Disabled: "任务活动中"}}})
	menuKeyMsg(m, tea.KeyEnter)
	if !strings.Contains(m.notice, "任务活动中") || len(m.pending) != 0 {
		t.Fatal("disabled action submitted")
	}
	menuKeyMsg(m, tea.KeyEsc)
	if m.dialog != parent {
		t.Fatal("help return")
	}
}
func TestMenuActualRendererFormCursorAndStreaming(t *testing.T) {
	model := New(context.Background(), Config{Initial: Snapshot{Status: Status{SessionID: "s", RunID: "r"}}})
	model.cursorTracking = true
	sink := &captureCursorSink{changed: make(chan struct{}, 128)}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(newCursorOutput(sink)), tea.WithAltScreen())
	finished := make(chan error, 1)
	go func() { _, err := program.Run(); finished <- err }()
	defer func() {
		program.Send(ExitEvent{})
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("exit timeout")
		}
	}()
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	waitCursor(t, sink, 0, func(x, y int) bool { return x == 3 && y == 19 })
	send := func(msg tea.Msg, predicate func(int, int) bool) {
		offset := len(sink.snapshot())
		program.Send(msg)
		waitCursor(t, sink, offset, predicate)
	}
	send(FormEvent{Title: "来源", Command: "/skills add", Plain: true, Fields: []FormField{{Name: "path", Label: "路径", Value: ""}}}, func(x, y int) bool { return x == 3 && y == 3 })
	send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文 A"), Paste: true}, func(x, y int) bool { return x == 9 && y == 3 })
	send(DeltaEvent{Scope: Scope{SessionID: "s", RunID: "r"}, ItemID: "answer", Text: "流式中文"}, func(x, y int) bool { return x == 9 && y == 3 })
	send(tea.WindowSizeMsg{Width: 24, Height: 12}, func(x, y int) bool { return x == 9 && y == 3 })
	send(tea.KeyMsg{Type: tea.KeyEsc}, func(x, y int) bool { return x == 3 && y >= 7 })
	if strings.Contains(sink.snapshot(), cursorProbe) {
		t.Fatal("probe leaked")
	}
}

func TestMenuFunctionKeyQueryReturnsToChat(t *testing.T) {
	m := New(context.Background(), Config{Handle: func(_ context.Context, a Action) ([]Event, error) {
		return []Event{MenuEvent{Page: "/model", Title: "模型", Items: []MenuItem{{Label: "模型一"}}}}, nil
	}})
	m.input.SetValue("未提交中文 draft")
	cmd := menuKeyMsg(m, tea.KeyF3)
	if cmd == nil {
		t.Fatal("F3 not routed")
	}
	m.Update(cmd())
	if m.dialog == nil || m.dialog.menu == nil {
		t.Fatal("model page")
	}
	menuKeyMsg(m, tea.KeyEsc)
	if m.dialog != nil || m.input.Value() != "未提交中文 draft" {
		t.Fatal("F3 left loading dialog or lost draft")
	}
}
