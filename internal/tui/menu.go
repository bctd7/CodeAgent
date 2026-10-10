package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"strings"
)

func (m *Model) refreshMenu() {
	d := m.dialog
	d.menuItems = nil
	for _, item := range d.menu.Items {
		if len(d.menu.Filters) > 0 && d.filterIndex > 0 && item.Filter != d.menu.Filters[d.filterIndex] {
			continue
		}
		if strings.Contains(strings.ToLower(item.Label+" "+item.Detail), strings.ToLower(d.filter)) {
			d.menuItems = append(d.menuItems, item)
		}
	}
	d.selected = min(d.selected, max(0, len(d.menuItems)-1))
	lines := []string{}
	if d.menu.Text != "" {
		lines = append(lines, ansi.Hardwrap(safeText(d.menu.Text), max(1, d.body.Width), true), "")
	}
	header := len(lines)
	for i, item := range d.menuItems {
		prefix := "  "
		if i == d.selected {
			prefix = "› "
		}
		line := prefix + item.Label
		if item.Detail != "" {
			line += " · " + item.Detail
		}
		if item.Disabled != "" {
			line += " · 不可用：" + item.Disabled
		}
		lines = append(lines, ansi.Hardwrap(safeText(line), max(1, d.body.Width), true))
	}
	if len(d.menuItems) == 0 {
		lines = append(lines, "暂无项目")
	}
	d.body.SetContent(strings.Join(lines, "\n"))
	row := 0
	for i := 0; i < header+d.selected && i < len(lines); i++ {
		row += strings.Count(lines[i], "\n") + 1
	}
	if row < d.body.YOffset {
		d.body.SetYOffset(row)
	}
	if row >= d.body.YOffset+d.body.Height {
		d.body.SetYOffset(row - d.body.Height + 1)
	}
}
func (m *Model) menuKey(key tea.KeyMsg) tea.Cmd {
	d := m.dialog
	if m.pendingInput() {
		return nil
	}
	switch key.String() {
	case "esc":
		m.dialog = d.parent
		m.layout()
		return nil
	case "tab":
		if len(d.menu.Filters) > 0 {
			d.filterIndex = (d.filterIndex + 1) % len(d.menu.Filters)
			d.selected = 0
			m.refreshMenu()
			return nil
		}
		fallthrough
	case "down":
		if len(d.menuItems) > 0 {
			d.selected = (d.selected + 1) % len(d.menuItems)
		}
	case "up", "shift+tab":
		if len(d.menuItems) > 0 {
			d.selected = (d.selected + len(d.menuItems) - 1) % len(d.menuItems)
		}
	case "pgup", "pgdown":
		d.body, _ = d.body.Update(key)
		return nil
	case "enter":
		if len(d.menuItems) == 0 {
			return nil
		}
		item := d.menuItems[d.selected]
		if item.Disabled != "" {
			return m.showError(item.Disabled)
		}
		if d.parent != nil && d.parent.form != nil && item.Value != "" {
			d.parent.fields[d.targetField].SetValue(item.Value)
			m.dialog = d.parent
			m.layout()
			return nil
		}
		if item.Form != nil {
			m.openForm(*item.Form)
			m.dialog.parent = d
			return nil
		}
		if item.Children != nil {
			m.dialog = &dialog{kind: ActionKind("menu"), parent: d, menu: &MenuEvent{Title: item.Label, Text: item.Text, Items: item.Children, Page: d.menu.Page, Search: true}}
			m.layout()
			return nil
		}
		if item.Command != "" {
			return m.dispatch(Action{Kind: ExecuteCommand, Text: item.Command, Page: d.menu.Page}, "")
		}
		m.dialog = &dialog{kind: ActionKind("panel"), title: item.Label, text: item.Text, parent: d}
		m.layout()
		return nil
	default:
		if d.menu.Search && (key.Type == tea.KeyRunes || key.String() == "backspace") {
			if key.String() == "backspace" {
				r := []rune(d.filter)
				if len(r) > 0 {
					d.filter = string(r[:len(r)-1])
				}
			} else {
				d.filter += string(key.Runes)
			}
			d.selected = 0
		}
	}
	m.refreshMenu()
	return nil
}

func (m *Model) openMenu(event MenuEvent) {
	previous := m.dialog
	if previous != nil && (previous.kind == ListModels || previous.kind == ListSessions) && previous.requestID != 0 {
		previous = previous.parent
	}
	next := &dialog{kind: ActionKind("menu"), menu: &event, title: event.Title, parent: previous}
	for d := previous; d != nil; d = d.parent {
		if d.menu != nil && d.menu.Page == event.Page && event.Page != "" {
			// Choose the outermost matching page rather than a stale child menu.
			next.parent = d.parent
			next.selected = d.selected
			next.filter = d.filter
			next.filterIndex = d.filterIndex
		}
	}
	m.dialog = next
}
