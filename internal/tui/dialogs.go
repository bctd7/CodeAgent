package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) openChoices(kind ActionKind) tea.Cmd {
	if m.pendingInput() {
		m.notice = "请等待当前操作返回"
		return nil
	}
	cmd := m.dispatch(Action{Kind: kind}, "")
	if cmd != nil {
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
	switch key.String() {
	case "esc":
		m.dialog = nil
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
		m.notice = "审批请求已过期，未提交选择"
		return nil
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
