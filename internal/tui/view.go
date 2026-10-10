package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// 过滤终端控制序列；内容是否包含业务秘密仍必须由调用方投影层保证。
func safeText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(text))
}
func (m *Model) transcriptText() string {
	var parts []string
	for _, item := range m.items {
		if cached, ok := m.cache[item.ID]; ok {
			parts = append(parts, cached)
			continue
		}
		label := safeText(string(item.Kind))
		switch item.Kind {
		case UserItem:
			label = "你"
		case AssistantItem:
			label = "助手"
		case NoticeItem:
			label = "提示"
		case ToolItem:
			label = "工具"
		}
		body := safeText(item.Text)
		if item.Tool != nil {
			t := item.Tool
			part := toolLine(t, max(1, m.width))
			if m.expanded[item.ID] {
				details := "参数：\n" + safeText(t.Parameters) + "\n结果：\n" + safeText(t.Output)
				if t.Diff != "" {
					details += "\n已有 Diff：\n" + safeText(t.Diff)
				}
				part += "\n" + ansi.Hardwrap(details, max(1, m.width), true)
			}
			m.cache[item.ID] = part
			parts = append(parts, part)
			continue
		}
		if item.Kind == AssistantItem && m.renderer != nil {
			if rendered, err := m.renderer.Render(body); err == nil {
				body = strings.Trim(rendered, "\n")
			}
		}
		// 硬换行兜底处理超长代码行和宽字符，Markdown 解析由 Glamour 负责。
		body = ansi.Hardwrap(body, max(1, m.width), true)
		part := lipgloss.NewStyle().Bold(true).Render(ansi.Truncate(label, max(1, m.width), "")) + "\n" + body
		m.cache[item.ID] = part
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "开始会话后，消息与工具过程显示在这里。"
	}
	var transcript strings.Builder
	for i, part := range parts {
		if i > 0 {
			if m.items[i-1].Tool != nil && m.items[i].Tool != nil {
				transcript.WriteByte('\n')
			} else {
				transcript.WriteString("\n\n")
			}
		}
		transcript.WriteString(part)
	}
	return transcript.String()
}

// Tool rows use terminal cell widths; full content remains available via F4.
func singleLine(text string) string {
	return strings.Join(strings.Fields(safeText(strings.ReplaceAll(text, "\r", " "))), " ")
}
func toolLine(tool *Tool, width int) string {
	state := singleLine(tool.State)
	nameWidth := max(1, width-ansi.StringWidth(" · "+state))
	header := ansi.Truncate(singleLine(tool.Name), nameWidth, "…") + " · " + state
	if ansi.StringWidth(header) > width {
		return ansi.Truncate(header, width, "…")
	}
	summary := singleLine(tool.Summary)
	remaining := width - ansi.StringWidth(header) - ansi.StringWidth(" · ")
	if summary != "" && remaining > 0 {
		return header + " · " + ansi.Truncate(summary, remaining, "…")
	}
	return header
}
func (m *Model) statusLine() string {
	session := m.status.SessionTitle
	if session == "" {
		session = m.status.SessionID
	}
	if session == "" {
		session = "未选择会话"
	}
	model := singleLine(m.status.ModelName)
	if model == "" {
		model = m.status.Model
	}
	if model == "" {
		model = "未选择模型"
	}
	state := m.status.State
	if state == "" {
		state = "就绪"
	}
	return safeText("模型 " + model + " · " + state + " " + m.status.Detail + " · 会话 " + session)
}
func (m *Model) approvalOptions() []string {
	labels := []string{"1 仅本次（once）", "2 本会话授权该工具，包含变化参数（session-tool）", "3 拒绝（reject）"}
	for i := range labels {
		prefix := "  "
		if i == m.approvalChoice {
			prefix = "› "
		}
		labels[i] = ansi.Hardwrap(prefix+labels[i], max(1, m.width), true)
	}
	return labels
}
func (m *Model) View() (view string) {
	if m.cursorTracking {
		defer func() { view = m.cursorFrameView(view) }()
	}
	notice := m.notice
	if len(m.alerts) > 0 {
		prefix := fmt.Sprintf("提示 %d 项 · Ctrl+L 关闭 · F1 详情：", len(m.alerts))
		notice = ansi.Truncate(safeText(prefix+strings.ReplaceAll(m.alerts[0].Text, "\n", " ")), max(1, m.width), "…")
	}
	if len(m.approvals) > 0 {
		options := m.approvalOptions()
		if m.height < 8 {
			return m.fit("审批 · " + safeText(m.approvals[0].Tool) + "\n" + options[m.approvalChoice] + "\n←/→ 选择 Enter 确认 Esc 拒绝\n" + notice)
		}
		return m.fit(fmt.Sprintf("审批请求（%d 项）\n%s\n%s\n←/→ 选择 · Enter 确认 · Esc 拒绝 · PgUp/PgDn 查看\n%s", len(m.approvals), m.approvalBody.View(), strings.Join(options, "\n"), safeText(notice)))
	}
	if m.dialog != nil {
		title := "选择会话"
		controls := "↑/↓ 选择 · Enter 确认 · Esc 取消"
		if m.dialog.kind == ActionKind("panel") || m.dialog.kind == ActionKind("notice") {
			title = m.dialog.title
			controls = "↑/↓ 或滚轮滚动 · PgUp/PgDn 翻页 · Enter/Esc 返回"
		}
		if m.dialog.kind == ListModels {
			title = "选择模型"
		}
		if m.dialog.kind == ActionKind("tools") {
			title = "工具详情：选择工具后展开/收起"
		}
		return m.fit(title + "\n" + m.dialog.body.View() + "\n" + controls + "\n" + safeText(notice))
	}
	hint := "Enter 发送 · Alt+Enter/Ctrl+J 换行 · Ctrl+C 取消 · Ctrl+Q 退出 · F1 提示历史 · F2/F3/F4 菜单"
	if candidates := m.candidates(); len(candidates) > 0 {
		choice := candidates[m.completion%len(candidates)]
		hint = choice.Name + " · " + choice.Description + "（↑/↓ 选择，Tab 补全）"
	}
	if !m.follow {
		hint = "正在查看较早内容 · Ctrl+End 回到底部 · PgUp/PgDn 滚动"
	}
	status := m.rows(singleLine(m.statusLine()), 1)
	if m.height == 1 {
		return status
	}
	input := m.rows(m.input.View(), m.input.Height())
	if m.height == 2 {
		return m.rows(m.input.View(), 1) + "\n" + status
	}
	footer := status + "\n" + m.rows(singleLine(notice), 1)
	if m.height < 6 {
		return input + "\n" + footer
	}
	// Every region owns a fixed number of rows; typing and streaming cannot
	// consume footer space. The textarea keeps excess input in its own viewport.
	parts := []string{m.rows(m.transcript.View(), m.transcript.Height), strings.Repeat("─", m.width), m.rows(singleLine(hint), 1), input, footer}
	if m.height >= 10 {
		header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75")).Render("CodeAgent")
		parts = append([]string{m.rows(header, 1)}, parts...)
	}
	return strings.Join(parts, "\n")
}

// 最终逐行裁剪，防止极窄窗口和超长状态文本突破终端边界。
func (m *Model) fit(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], max(1, m.width), "")
	}
	return strings.Join(lines, "\n")
}

// Clip each region independently and pad empty rows before composing the screen.
func (m *Model) rows(text string, height int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], max(1, m.width), "")
	}
	return strings.Join(lines, "\n")
}
