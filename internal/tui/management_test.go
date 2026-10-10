package tui

import (
	"context"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestProductFormChinesePasteResizeAndCancel(t *testing.T) {
	m := New(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 44, Height: 14})
	m.apply(FormEvent{Title: "新会话", Command: "/new", Fields: []FormField{{Name: "project", Label: "项目目录", Value: "C:/中文项目/路径"}, {Name: "model", Label: "模型 REF", Value: "fake/main"}, {Name: "memory_enabled", Label: "长期记忆", Value: "true", Boolean: true}}})
	if m.dialog == nil || !strings.Contains(m.View(), "新会话") {
		t.Fatal("form missing")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文"), Paste: true})
	if !strings.Contains(m.dialog.fields[0].Value(), "中文") {
		t.Fatal("paste ignored")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.dialog.selected != 2 {
		t.Fatal("focus")
	}
	if !strings.Contains(m.View(), "长期记忆") {
		t.Fatal("focused field invisible")
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if ansi.StringWidth(line) > 44 {
			t.Fatal("layout exceeds width")
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.dialog != nil {
		t.Fatal("cancel")
	}
}
func TestProductSessionsFilterAndActions(t *testing.T) {
	var sent Action
	m := New(context.Background(), Config{Handle: func(_ context.Context, a Action) ([]Event, error) { sent = a; return nil, nil }})
	m.apply(ChoicesEvent{Kind: ListSessions, Choices: []Choice{{ID: "one", Label: "中文会话"}, {ID: "two", Label: "other", Detail: "archived"}}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	if len(m.dialog.choices) != 1 || m.dialog.choices[0].ID != "one" {
		t.Fatal("search")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if cmd == nil {
		t.Fatal("rename not routed")
	}
	cmd()
	if sent.Text != "/sessions rename one" {
		t.Fatal("wrong session action", sent)
	}
}
