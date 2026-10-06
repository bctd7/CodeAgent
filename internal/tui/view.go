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
			label = "工具 · " + safeText(t.Name) + " · " + safeText(t.State)
			body = safeText(t.Summary)
			if m.expanded[item.ID] {
				body += "\n参数：\n" + safeText(t.Parameters) + "\n输出：\n" + safeText(t.Output)
				if t.Diff != "" {
					body += "\n已有 Diff：\n" + safeText(t.Diff)
				}
			}
		} else if item.Kind == AssistantItem && m.renderer != nil {
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
	return strings.Join(parts, "\n\n")
}
func (m *Model) statusLine() string {
	session := m.status.SessionTitle
	if session == "" {
		session = m.status.SessionID
	}
	if session == "" {
		session = "未选择会话"
	}
	model := m.status.Model
	if model == "" {
		model = "未选择模型"
	}
	state := m.status.State
	if state == "" {
		state = "就绪"
	}
	return safeText(session + " · " + model + " · " + state + " " + m.status.Detail)
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
func (m *Model) View() string {
	if len(m.approvals) > 0 {
		options := m.approvalOptions()
		if m.height < 8 {
			return m.fit("审批 · " + safeText(m.approvals[0].Tool) + "\n" + options[m.approvalChoice] + "\n←/→ 选择 Enter 确认 Esc 拒绝\n" + m.notice)
		}
		return m.fit(fmt.Sprintf("审批请求（%d 项）\n%s\n%s\n←/→ 选择 · Enter 确认 · Esc 拒绝 · PgUp/PgDn 查看\n%s", len(m.approvals), m.approvalBody.View(), strings.Join(options, "\n"), safeText(m.notice)))
	}
	if m.dialog != nil {
		title := "选择会话"
		if m.dialog.kind == ListModels {
			title = "选择模型"
		}
		if m.dialog.kind == ActionKind("tools") {
			title = "工具详情：选择工具后展开/收起"
		}
		return m.fit(title + "\n" + m.dialog.body.View() + "\n↑/↓ 选择 · Enter 确认 · Esc 返回\n" + safeText(m.notice))
	}
	hint := "Ctrl+S 发送 · Alt+S Steer · Enter 换行 · F2 会话 · F3 模型 · F4 工具 · Ctrl+Q 退出"
	if candidates := m.candidates(); len(candidates) > 0 {
		choice := candidates[m.completion%len(candidates)]
		hint = choice.Name + " · " + choice.Description + "（↑/↓ 选择，Tab 补全）"
	}
	if !m.follow {
		hint = "正在查看较早内容 · Ctrl+End 回到底部 · PgUp/PgDn 滚动"
	}
	if m.height < 8 {
		return m.fit(m.statusLine() + "\n" + m.input.View() + "\n" + safeText(m.notice))
	}
	return m.fit("CodeAgent\n" + m.transcript.View() + "\n" + safeText(hint) + "\n" + m.input.View() + "\n" + m.statusLine() + "\n" + safeText(m.notice))
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
