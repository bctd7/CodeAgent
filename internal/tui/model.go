package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
)

type dialog struct {
	title, text string
	kind        ActionKind
	requestID   uint64
	choices     []Choice
	selected    int
	body        viewport.Model
}
type operation struct {
	id     uint64
	action Action
	draft  string
}
type operationResult struct {
	operation operation
	events    []Event
	err       error
}
type incoming struct {
	event Event
	open  bool
}
type noticeExpired struct{ generation uint64 }

// 审批保留收到请求时的归属，不随当前 Run 变化而重新绑定。
type approvalRequest struct {
	Scope
	Approval
}

// Model 仅由 Tea 事件循环拥有。外部注入使用 Program.Send(Event) 或 Config.Events。
type Model struct {
	ctx              context.Context
	cfg              Config
	status           Status
	items            []Item
	input            textarea.Model
	transcript       viewport.Model
	width, height    int
	follow           bool
	expanded         map[string]bool
	approvals        []approvalRequest
	approvalChoice   int
	approvalBody     viewport.Model
	dialog           *dialog
	pending          map[ActionKind]operation
	sequence         uint64
	notice           string
	noticeGeneration uint64
	alerts           []Notice
	errorHistory     []Notice
	completion       int
	cancelledRun     string
	cancelRequested  bool
	quitRequested    bool
	renderer         *glamour.TermRenderer
	renderWidth      int
	cache            map[string]string
	cursorTracking   bool
	cursorFrame      uint64
	windowFocused    bool
}

func New(ctx context.Context, cfg Config) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	input := textarea.New()
	input.Placeholder = "输入任务或 /help；Enter 发送，Alt+Enter/Ctrl+J 换行"
	input.ShowLineNumbers, input.CharLimit, input.MaxWidth = false, 0, 0
	input.Prompt = "│ "
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	input.SetHeight(4)
	input.Focus()
	m := &Model{ctx: ctx, cfg: cfg, input: input, transcript: viewport.New(80, 15),
		width: 80, height: 24, follow: true, windowFocused: true, expanded: map[string]bool{},
		pending: map[ActionKind]operation{}, cache: map[string]string{}, approvalBody: viewport.New(76, 12)}
	m.cfg.Commands = append([]Command(nil), cfg.Commands...)
	m.setSnapshot(cfg.Initial)
	m.layout()
	return m
}

// Run 不装配实际服务；外部 context 取消会停止 UI，后端负责运行层收尾。
func Run(ctx context.Context, cfg Config, streams IO) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 派生 context 确保退出后，等待事件的 Tea 命令也结束。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := New(ctx, cfg)
	model.cursorTracking = true
	output := streams.Out
	if output == nil {
		output = os.Stdout
	}
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus(), tea.WithOutput(newCursorOutput(output))}
	if streams.In != nil {
		options = append(options, tea.WithInput(streams.In))
	}
	_, err := tea.NewProgram(model, options...).Run()
	return err
}

func (m *Model) Init() tea.Cmd { return tea.Batch(textarea.Blink, m.nextEvent()) }
func (m *Model) nextEvent() tea.Cmd {
	if m.cfg.Events == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case event, ok := <-m.cfg.Events:
			return incoming{event, ok}
		case <-m.ctx.Done():
			return incoming{}
		}
	}
}
func cloneItem(item Item) Item {
	if item.Tool != nil {
		tool := *item.Tool
		item.Tool = &tool
	}
	return item
}
func (m *Model) setSnapshot(snapshot Snapshot) {
	m.status = snapshot.Status
	m.items = make([]Item, len(snapshot.Items))
	for i, item := range snapshot.Items {
		m.items[i] = cloneItem(item)
	}
	m.expanded, m.cache = map[string]bool{}, map[string]string{}
	m.approvals, m.dialog = nil, nil
	m.follow, m.approvalChoice = true, 0
	m.cancelledRun = ""
	m.cancelRequested = false
	m.clearNotice()
	for _, notice := range snapshot.Notices {
		m.addAlert(notice)
	}
	m.refresh()
}
func (m *Model) clearNotice() {
	m.noticeGeneration++
	m.notice = ""
}
func (m *Model) showNotice(text string) tea.Cmd {
	m.clearNotice()
	m.notice = safeText(text)
	generation := m.noticeGeneration
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return noticeExpired{generation} })
}
func (m *Model) addAlert(notice Notice) {
	notice.Text = safeText(notice.Text)
	if notice.ID == "" {
		m.sequence++
		notice.ID = fmt.Sprintf("ui-error/%d", m.sequence)
	}
	for _, existing := range m.alerts {
		if existing.ID == notice.ID {
			return
		}
	}
	m.alerts = append(m.alerts, notice)
}

// Ordinary operation failures expire on screen but remain inspectable via F1.
func (m *Model) showError(text string) tea.Cmd {
	m.sequence++
	m.errorHistory = append(m.errorHistory, Notice{ID: fmt.Sprintf("ui-error/%d", m.sequence), Text: safeText(text)})
	return m.showNotice(text + " · F1 查看详情")
}
func (m *Model) openNoticeHistory() {
	var parts []string
	for _, notice := range m.alerts {
		parts = append(parts, notice.Text)
	}
	for _, notice := range m.errorHistory {
		parts = append(parts, notice.Text)
	}
	m.dialog = &dialog{kind: ActionKind("notice"), title: "错误历史 / 重要提示", text: strings.Join(parts, "\n\n")}
	m.layout()
}
func (m *Model) removeAlert(id string) {
	for i, notice := range m.alerts {
		if notice.ID == id {
			m.alerts = append(m.alerts[:i], m.alerts[i+1:]...)
			break
		}
	}
	if m.dialog != nil && m.dialog.kind == ActionKind("notice") {
		m.dialog = nil
	}
}
func (m *Model) dismissAlert() tea.Cmd {
	if len(m.alerts) == 0 {
		return nil
	}
	id := m.alerts[0].ID
	if strings.HasPrefix(id, "ui-error/") {
		m.removeAlert(id)
		return nil
	}
	return m.dispatch(Action{Kind: DismissNotice, NoticeID: id}, "")
}
func (m *Model) accepts(scope Scope) bool {
	return (scope.SessionID == "" || scope.SessionID == m.status.SessionID) &&
		(scope.RunID == "" || scope.RunID == m.status.RunID)
}
func (m *Model) bindApprovalScope(scope Scope) Scope {
	if scope.SessionID == "" {
		scope.SessionID = m.status.SessionID
	}
	if scope.RunID == "" {
		scope.RunID = m.status.RunID
	}
	return scope
}
func (m *Model) approvalScopeCurrent(scope Scope) bool {
	// 空 Run 也按收到时的实际值比较，不作为通配符。
	return scope.SessionID == m.status.SessionID && scope.RunID == m.status.RunID
}
func (m *Model) upsert(item Item) {
	if item.ID == "" {
		return
	}
	delete(m.cache, item.ID)
	for i := range m.items {
		if m.items[i].ID == item.ID {
			m.items[i] = cloneItem(item)
			return
		}
	}
	m.items = append(m.items, cloneItem(item))
}
func (m *Model) dispatch(action Action, draft string) tea.Cmd {
	return m.dispatchScoped(action, draft, Scope{SessionID: m.status.SessionID, RunID: m.status.RunID})
}
func (m *Model) dispatchScoped(action Action, draft string, scope Scope) tea.Cmd {
	if _, busy := m.pending[action.Kind]; busy {
		return nil
	}
	if m.cfg.Handle == nil {
		return m.showError("该操作尚未接入")
	}
	action.SessionID, action.RunID = scope.SessionID, scope.RunID
	m.sequence++
	op := operation{id: m.sequence, action: action, draft: draft}
	m.pending[action.Kind] = op
	m.clearNotice()
	return func() tea.Msg { events, err := m.cfg.Handle(m.ctx, action); return operationResult{op, events, err} }
}
func (m *Model) layout() {
	w := max(1, m.width)
	inputHeight, transcriptHeight := 4, max(1, m.height-9)
	if m.height < 10 {
		inputHeight = min(3, max(1, m.height-5))
		transcriptHeight = max(1, m.height-inputHeight-4)
	}
	if m.height < 6 {
		inputHeight = max(1, m.height-2)
	}
	m.input.Prompt = "› "
	if w < 3 {
		m.input.Prompt = ""
	}
	m.input.SetWidth(w)
	m.input.SetHeight(inputHeight)
	// Reflow the textarea's own viewport around its cursor after resizing.
	m.syncInputViewport()
	m.transcript.Width, m.transcript.Height = w, transcriptHeight
	if m.renderWidth != w {
		// 使用固定成熟主题，不进行终端背景查询。
		m.renderer, _ = glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(max(1, w-2)))
		m.renderWidth, m.cache = w, map[string]string{}
	}
	optionRows := 0
	for _, option := range m.approvalOptions() {
		optionRows += strings.Count(option, "\n") + 1
	}
	m.approvalBody.Width, m.approvalBody.Height = max(1, w-4), max(1, m.height-optionRows-3)
	if m.dialog != nil {
		m.dialog.body.Width, m.dialog.body.Height = max(1, w-4), max(1, m.height-6)
		m.refreshDialog()
	}
	m.refreshApproval()
	m.refresh()
}
func (m *Model) refresh() {
	offset := m.transcript.YOffset
	m.transcript.SetContent(m.transcriptText())
	if m.follow {
		m.transcript.GotoBottom()
	} else {
		m.transcript.SetYOffset(offset)
	}
}
func (m *Model) pendingInput() bool {
	for _, kind := range []ActionKind{Send, Steer, ExecuteCommand, SelectSession, SelectModel} {
		if _, ok := m.pending[kind]; ok {
			return true
		}
	}
	return false
}
func (m *Model) candidates() []Command {
	text := m.input.Value()
	if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \n\t") {
		return nil
	}
	var result []Command
	for _, cmd := range m.cfg.Commands {
		if strings.HasPrefix(cmd.Name, text) {
			result = append(result, cmd)
		}
	}
	return result
}
func (m *Model) send(steer bool) tea.Cmd {
	text := m.input.Value()
	if strings.TrimSpace(text) == "" || m.pendingInput() {
		return nil
	}
	kind := Send
	if steer {
		if !m.status.Running {
			return m.showNotice("当前没有运行中的 Run，不能 Steer")
		}
		kind = Steer
	} else if strings.HasPrefix(strings.TrimSpace(text), "/") {
		kind = ExecuteCommand
	} else if m.status.Running {
		return m.showNotice("Run 正在执行：Alt+S 发送 Steer，Ctrl+C 请求取消")
	}
	return m.dispatch(Action{Kind: kind, Text: text}, text)
}

// The component's View refreshes its content bounds. Reposition after that so
// paste/soft wrapping and resize use the new bounds rather than the prior frame.
func (m *Model) syncInputViewport() {
	_ = m.input.View()
	m.input, _ = m.input.Update(tea.WindowSizeMsg{Width: m.width, Height: m.input.Height()})
}
