package tui

import (
	"encoding/json"
	"github.com/charmbracelet/bubbles/textinput"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) openChoices(kind ActionKind) tea.Cmd {
	if m.pendingInput() {
		return m.showNotice("请等待当前操作返回")
	}
	cmd := m.dispatch(Action{Kind: kind}, "")
	if _, submitted := m.pending[kind]; submitted {
		m.dialog = &dialog{kind: kind, requestID: m.pending[kind].id}
		m.layout()
	}
	return cmd
}
func (m *Model) openTools() {
	var choices []Choice
	for _, item := range m.items {
		if item.Tool != nil {
			choices = append(choices, Choice{ID: item.ID, Label: item.Tool.Name, Detail: item.Tool.State + " · " + item.Tool.Summary})
		}
	}
	m.dialog = &dialog{kind: ActionKind("tools"), choices: choices}
	m.layout()
}
func (m *Model) refreshDialog() {
	if m.dialog == nil {
		return
	}
	d := m.dialog
	if d.menu != nil {
		m.refreshMenu()
		return
	}
	if d.form != nil {
		var lines []string
		focusRow := 0
		row := 0
		for i := range d.fields {
			d.fields[i].Width = max(1, d.body.Width-2)
			label := ansi.Hardwrap(safeText(d.form.Fields[i].Label), max(1, d.body.Width), true)
			if i == d.selected {
				focusRow = row + strings.Count(label, "\n") + 1
			}
			value := d.fields[i].View()
			if field := d.form.Fields[i]; len(field.Options) > 0 {
				value = d.fields[i].Value()
				for _, option := range field.Options {
					if option.ID == value {
						value = option.Label
						break
					}
				}
				value = "[ " + value + " ]  Enter 选择"
			} else if field.Boolean {
				value = "[ " + d.fields[i].Value() + " ]  空格切换"
			}
			line := label + "\n" + value
			lines = append(lines, line)
			row += strings.Count(line, "\n") + 2
		}
		d.body.SetContent(strings.Join(lines, "\n\n"))
		if focusRow < d.body.YOffset {
			d.body.SetYOffset(focusRow)
		}
		if focusRow >= d.body.YOffset+d.body.Height {
			d.body.SetYOffset(focusRow - d.body.Height + 1)
		}
		return
	}
	if d.kind == ActionKind("panel") || d.kind == ActionKind("notice") {
		d.body.SetContent(ansi.Hardwrap(safeText(d.text), max(1, d.body.Width), true))
		return
	}
	var lines []string
	if len(d.choices) == 0 {
		lines = append(lines, "暂无可选项")
	}
	for i, choice := range d.choices {
		prefix := "  "
		if i == d.selected {
			prefix = "› "
		}
		lines = append(lines, ansi.Hardwrap(safeText(prefix+choice.Label+" · "+choice.Detail), max(1, d.body.Width), true))
	}
	d.body.SetContent(strings.Join(lines, "\n"))
	// 光标移动时保持所选项可见；宽字符行高交给 ANSI 库计算。
	row := 0
	for i := 0; i < d.selected; i++ {
		row += strings.Count(lines[i], "\n") + 1
	}
	if row < d.body.YOffset {
		d.body.SetYOffset(row)
	}
	if row >= d.body.YOffset+d.body.Height {
		d.body.SetYOffset(row - d.body.Height + 1)
	}
}
func (m *Model) dialogKey(key tea.KeyMsg) tea.Cmd {
	d := m.dialog
	if d.menu != nil {
		return m.menuKey(key)
	}
	if d.form != nil {
		return m.formKey(key)
	}
	if d.kind == ListSessions {
		if len(d.choices) > 0 {
			id := d.choices[d.selected].ID
			commands := map[string]string{"ctrl+r": "rename", "ctrl+a": "archive", "ctrl+u": "unarchive", "ctrl+h": "history", "ctrl+e": "export"}
			if operation, ok := commands[key.String()]; ok {
				return m.dispatch(Action{Kind: ExecuteCommand, Text: "/sessions " + operation + " " + id}, "")
			}
		}
		if key.Type == tea.KeyRunes || key.String() == "backspace" {
			if key.String() == "backspace" {
				r := []rune(d.filter)
				if len(r) > 0 {
					d.filter = string(r[:len(r)-1])
				}
			} else {
				d.filter += string(key.Runes)
			}
			d.choices = nil
			for _, choice := range d.allChoices {
				if strings.Contains(strings.ToLower(choice.Label+" "+choice.Detail), strings.ToLower(d.filter)) {
					d.choices = append(d.choices, choice)
				}
			}
			d.selected = 0
			m.refreshDialog()
			return nil
		}
	}
	if (d.kind == ActionKind("panel") || d.kind == ActionKind("notice")) && (key.String() == "up" || key.String() == "down") {
		d.body, _ = d.body.Update(key)
		return nil
	}
	switch key.String() {
	case "esc":
		m.dialog = d.parent
		m.layout()
	case "up":
		if len(d.choices) > 0 {
			d.selected = (d.selected + len(d.choices) - 1) % len(d.choices)
			m.refreshDialog()
		}
	case "down":
		if len(d.choices) > 0 {
			d.selected = (d.selected + 1) % len(d.choices)
			m.refreshDialog()
		}
	case "pgup", "pgdown":
		d.body, _ = d.body.Update(key)
	case "enter":
		if d.kind == ActionKind("panel") || d.kind == ActionKind("notice") {
			m.dialog = d.parent
			m.layout()
			return nil
		}
		if len(d.choices) == 0 || m.pendingInput() {
			return nil
		}
		choice := d.choices[d.selected]
		if d.kind == ActionKind("tools") {
			m.expanded[choice.ID] = !m.expanded[choice.ID]
			delete(m.cache, choice.ID)
			m.dialog = nil
			m.refresh()
			return nil
		}
		kind := SelectSession
		if d.kind == ListModels {
			kind = SelectModel
		}
		return m.dispatch(Action{Kind: kind, ChoiceID: choice.ID}, "")
	}
	return nil
}
func (m *Model) refreshApproval() {
	if len(m.approvals) == 0 {
		return
	}
	a := m.approvals[0]
	text := "来源：" + a.Source + "\n工具：" + a.Tool + "\n\n参数：\n" + a.Parameters
	if a.Diff != "" {
		text += "\n\n已有 Diff：\n" + a.Diff
	}
	m.approvalBody.SetContent(ansi.Hardwrap(safeText(text), max(1, m.approvalBody.Width), true))
}
func (m *Model) removeApproval(scope Scope, id string) {
	for i, a := range m.approvals {
		if a.Scope == scope && a.ID == id {
			m.approvals = append(m.approvals[:i], m.approvals[i+1:]...)
			if i == 0 {
				m.approvalChoice = 0
				m.approvalBody.GotoTop()
			}
			break
		}
	}
	m.refreshApproval()
}
func (m *Model) approvalKey(key tea.KeyMsg) tea.Cmd {
	request := m.approvals[0]
	if !m.approvalScopeCurrent(request.Scope) {
		m.removeApproval(request.Scope, request.ID)
		return m.showError("审批请求已过期，未提交选择")
	}
	if _, busy := m.pending[AnswerApproval]; busy {
		return nil
	}
	switch key.String() {
	case "left", "up":
		m.approvalChoice = (m.approvalChoice + 2) % 3
	case "right", "down", "tab":
		m.approvalChoice = (m.approvalChoice + 1) % 3
	case "pgup", "pgdown":
		m.approvalBody, _ = m.approvalBody.Update(key)
	case "1":
		m.approvalChoice = 0
	case "2":
		m.approvalChoice = 1
	case "3":
		m.approvalChoice = 2
	case "esc":
		return m.dispatchScoped(Action{Kind: AnswerApproval, ApprovalID: request.ID, Decision: Reject}, "", request.Scope)
	case "enter":
		choices := []ApprovalDecision{ApproveOnce, ApproveSessionTool, Reject}
		return m.dispatchScoped(Action{Kind: AnswerApproval, ApprovalID: request.ID, Decision: choices[m.approvalChoice]}, "", request.Scope)
	}
	return nil
}

func (m *Model) openForm(event FormEvent) {
	d := &dialog{kind: ActionKind("form"), title: event.Title, form: &event}
	for i, field := range event.Fields {
		input := textinput.New()
		input.CharLimit = 4096
		input.SetValue(field.Value)
		if i == 0 {
			input.Focus()
		}
		d.fields = append(d.fields, input)
	}
	m.dialog = d
	m.layout()
}
func (m *Model) formKey(key tea.KeyMsg) tea.Cmd {
	d := m.dialog
	if m.pendingInput() {
		return nil
	}
	if key.String() == "esc" {
		m.dialog = d.parent
		m.layout()
		return nil
	}
	if key.String() == "tab" || key.String() == "shift+tab" {
		d.fields[d.selected].Blur()
		step := 1
		if key.String() == "shift+tab" {
			step = -1
		}
		d.selected = (d.selected + step + len(d.fields)) % len(d.fields)
		cmd := d.fields[d.selected].Focus()
		m.refreshDialog()
		return cmd
	}
	if field := d.form.Fields[d.selected]; len(field.Options) > 0 && (key.String() == "enter" || key.String() == " ") {
		items := []MenuItem{}
		selected := 0
		for i, option := range field.Options {
			items = append(items, MenuItem{Label: option.Label, Detail: option.Detail, Value: option.ID})
			if option.ID == d.fields[d.selected].Value() {
				selected = i
			}
		}
		m.dialog = &dialog{kind: ActionKind("menu"), parent: d, menu: &MenuEvent{Title: field.Label, Items: items, Search: true}, targetField: d.selected, selected: selected}
		m.layout()
		return nil
	}
	if d.form.Fields[d.selected].Boolean && (key.String() == " " || key.String() == "left" || key.String() == "right") {
		d.fields[d.selected].SetValue(strconv.FormatBool(d.fields[d.selected].Value() != "true"))
		m.refreshDialog()
		return nil
	}
	if key.String() == "enter" {
		if m.pendingInput() {
			return nil
		}
		text := d.form.Command + " "
		if d.form.Plain {
			text += d.fields[0].Value()
		} else {
			values := map[string]any{}
			for i, field := range d.form.Fields {
				if field.Boolean {
					value, e := strconv.ParseBool(d.fields[i].Value())
					if e != nil {
						return m.showError("请输入 true 或 false")
					}
					values[field.Name] = value
				} else {
					values[field.Name] = d.fields[i].Value()
				}
			}
			encoded, e := json.Marshal(values)
			if e != nil {
				return m.showError(e.Error())
			}
			text += string(encoded)
		}
		return m.dispatch(Action{Kind: ExecuteCommand, Text: text, Page: d.form.Page}, "")
	}
	if d.form.Fields[d.selected].Boolean || len(d.form.Fields[d.selected].Options) > 0 {
		return nil
	}
	var cmd tea.Cmd
	d.fields[d.selected], cmd = d.fields[d.selected].Update(key)
	m.refreshDialog()
	return cmd
}
