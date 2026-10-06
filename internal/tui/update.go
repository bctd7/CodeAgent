package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.layout()
		return m, nil
	case incoming:
		if !msg.open {
			m.cfg.Events = nil
			return m, nil
		}
		return m, tea.Batch(m.apply(msg.event), m.nextEvent())
	case Event:
		return m, m.apply(msg)
	case operationResult:
		op := msg.operation
		if current, ok := m.pending[op.action.Kind]; !ok || current.id != op.id {
			return m, nil
		}
		delete(m.pending, op.action.Kind)
		approvalScope := Scope{SessionID: op.action.SessionID, RunID: op.action.RunID}
		if op.action.Kind == AnswerApproval && !m.approvalScopeCurrent(approvalScope) {
			// 迟到结果只清理原请求，不改变新 Run 的提示或审批。
			m.removeApproval(approvalScope, op.action.ApprovalID)
			return m, nil
		}
		// 会话切换之后，旧操作不能清空新草稿或覆盖新会话。
		if op.action.SessionID != m.status.SessionID && op.action.Kind != Quit {
			return m, nil
		}
		// 加载结果关联本次弹层；取消后再打开其他弹层也不会被旧结果覆盖。
		if op.action.Kind == ListSessions || op.action.Kind == ListModels {
			if m.dialog == nil || m.dialog.requestID != op.id {
				return m, nil
			}
		}
		if msg.err != nil {
			m.notice = fmt.Sprintf("操作失败：%s", safeText(msg.err.Error()))
			if op.action.Kind == CancelRun {
				m.cancelledRun = ""
				m.cancelRequested = false
			}
			if op.action.Kind == Quit {
				m.quitRequested = false
			}
			return m, nil
		}
		m.notice = "操作已受理"
		if op.draft != "" && m.input.Value() == op.draft {
			m.input.Reset()
			m.completion = 0
		}
		if op.action.Kind == AnswerApproval {
			m.removeApproval(approvalScope, op.action.ApprovalID)
		}
		if op.action.Kind == SelectSession || op.action.Kind == SelectModel {
			m.dialog = nil
		}
		var cmds []tea.Cmd
		for _, event := range msg.events {
			if op.action.Kind == AnswerApproval && !m.approvalScopeCurrent(approvalScope) {
				break
			}
			cmds = append(cmds, m.apply(event))
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		// 粘贴的内容必须进入 textarea，不能被解释成快捷键或弹层操作。
		if msg.Paste {
			if len(m.approvals) > 0 || m.dialog != nil {
				return m, nil
			}
			break
		}
		if msg.String() == "ctrl+q" {
			return m, m.requestQuit()
		}
		if msg.String() == "ctrl+c" {
			if m.status.Running {
				if m.cancelRequested && m.cancelledRun == m.status.RunID {
					return m, nil
				}
				cmd := m.dispatch(Action{Kind: CancelRun}, "")
				if cmd != nil {
					m.cancelledRun = m.status.RunID
					m.cancelRequested = true
				}
				return m, cmd
			}
			return m, m.requestQuit()
		}
		if len(m.approvals) > 0 {
			return m, m.approvalKey(msg)
		}
		if m.dialog != nil {
			return m, m.dialogKey(msg)
		}
		switch msg.String() {
		case "ctrl+s":
			return m, m.send(false)
		case "alt+s":
			return m, m.send(true)
		case "f2":
			return m, m.openChoices(ListSessions)
		case "f3":
			return m, m.openChoices(ListModels)
		case "f4":
			m.openTools()
			return m, nil
		case "pgup", "pgdown":
			m.transcript, _ = m.transcript.Update(msg)
			m.follow = m.transcript.AtBottom()
			return m, nil
		case "ctrl+end":
			m.follow = true
			m.transcript.GotoBottom()
			return m, nil
		case "tab":
			candidates := m.candidates()
			if len(candidates) > 0 {
				m.input.SetValue(candidates[m.completion%len(candidates)].Name + " ")
				m.input.CursorEnd()
				m.completion = 0
				return m, nil
			}
		case "up", "down":
			candidates := m.candidates()
			if len(candidates) > 0 {
				if msg.String() == "up" {
					m.completion = (m.completion + len(candidates) - 1) % len(candidates)
				} else {
					m.completion = (m.completion + 1) % len(candidates)
				}
				return m, nil
			}
		}
	case tea.MouseMsg:
		if len(m.approvals) > 0 {
			m.approvalBody, _ = m.approvalBody.Update(msg)
			return m, nil
		}
		if m.dialog != nil {
			m.dialog.body, _ = m.dialog.body.Update(msg)
			return m, nil
		}
		m.transcript, _ = m.transcript.Update(msg)
		m.follow = m.transcript.AtBottom()
		return m, nil
	}
	// 光标、剪贴板结果等组件消息也必须统一进入 Update。
	var cmd tea.Cmd
	if len(m.approvals) == 0 && m.dialog == nil {
		m.input, cmd = m.input.Update(msg)
	}
	return m, cmd
}

func (m *Model) requestQuit() tea.Cmd {
	if m.quitRequested {
		return nil
	}
	cmd := m.dispatch(Action{Kind: Quit}, "")
	if cmd != nil {
		m.quitRequested = true
	}
	return cmd
}
func (m *Model) apply(event Event) tea.Cmd {
	switch event := event.(type) {
	case SnapshotEvent:
		m.setSnapshot(event.Snapshot)
	case ItemEvent:
		if !m.accepts(event.Scope) {
			return nil
		}
		m.upsert(event.Item)
		m.refresh()
	case DeltaEvent:
		if !m.accepts(event.Scope) || event.ItemID == "" {
			return nil
		}
		found := false
		for i := range m.items {
			if m.items[i].ID == event.ItemID && m.items[i].Kind == AssistantItem {
				m.items[i].Text += event.Text
				found = true
				break
			}
		}
		if !found {
			m.upsert(Item{ID: event.ItemID, Kind: AssistantItem, Text: event.Text})
		}
		delete(m.cache, event.ItemID)
		m.refresh()
	case StatusEvent:
		if !m.accepts(event.Scope) {
			return nil
		}
		m.status = event.Status
		if !m.status.Running {
			m.cancelledRun = ""
			m.cancelRequested = false
		}
	case ChoicesEvent:
		if !m.accepts(event.Scope) {
			return nil
		}
		if event.Kind != ListSessions && event.Kind != ListModels {
			return nil
		}
		m.dialog = &dialog{kind: event.Kind, choices: append([]Choice(nil), event.Choices...)}
		m.layout()
	case ApprovalEvent:
		if !m.accepts(event.Scope) || event.Approval.ID == "" {
			return nil
		}
		scope := m.bindApprovalScope(event.Scope)
		for _, a := range m.approvals {
			if a.Scope == scope && a.ID == event.Approval.ID {
				return nil
			}
		}
		m.approvals = append(m.approvals, approvalRequest{Scope: scope, Approval: event.Approval})
		m.refreshApproval()
	case ApprovalResolvedEvent:
		m.removeApproval(m.bindApprovalScope(event.Scope), event.ID)
	case NoticeEvent:
		if m.accepts(event.Scope) {
			m.notice = safeText(event.Text)
		}
	case ExitEvent:
		return tea.Quit
	}
	return nil
}
