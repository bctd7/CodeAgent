package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
)

type dialog struct {
	kind      ActionKind
	requestID uint64
	choices   []Choice
	selected  int
	body      viewport.Model
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

// 审批保留收到请求时的归属，不随当前 Run 变化而重新绑定。
type approvalRequest struct {
	Scope
	Approval
}

// Model 仅由 Tea 事件循环拥有。外部注入使用 Program.Send(Event) 或 Config.Events。
type Model struct {
	ctx             context.Context
	cfg             Config
	status          Status
	items           []Item
	input           textarea.Model
	transcript      viewport.Model
	width, height   int
	follow          bool
	expanded        map[string]bool
	approvals       []approvalRequest
	approvalChoice  int
	approvalBody    viewport.Model
	dialog          *dialog
	pending         map[ActionKind]operation
	sequence        uint64
	notice          string
	completion      int
	cancelledRun    string
	cancelRequested bool
	quitRequested   bool
	renderer        *glamour.TermRenderer
	renderWidth     int
	cache           map[string]string
}

func New(ctx context.Context, cfg Config) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	input := textarea.New()
	input.Placeholder = "输入消息；Enter 换行，Ctrl+S 发送，Alt+S Steer"
	input.ShowLineNumbers, input.CharLimit, input.MaxWidth = false, 0, 0
	input.Prompt = "│ "
	input.KeyMap.InsertNewline.SetKeys("enter", "ctrl+j")
	input.SetHeight(4)
	input.Focus()
	m := &Model{ctx: ctx, cfg: cfg, input: input, transcript: viewport.New(80, 15),
		width: 80, height: 24, follow: true, expanded: map[string]bool{},
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
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion()}
	if streams.In != nil {
		options = append(options, tea.WithInput(streams.In))
	}
	if streams.Out != nil {
		options = append(options, tea.WithOutput(streams.Out))
	}
	_, err := tea.NewProgram(New(ctx, cfg), options...).Run()
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
	m.notice = ""
	m.refresh()
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
		m.notice = "该操作尚未接入"
		return nil
	}
	action.SessionID, action.RunID = scope.SessionID, scope.RunID
	m.sequence++
	op := operation{id: m.sequence, action: action, draft: draft}
	m.pending[action.Kind] = op
	m.notice = "正在提交操作…"
	return func() tea.Msg { events, err := m.cfg.Handle(m.ctx, action); return operationResult{op, events, err} }
}
func (m *Model) layout() {
	w := max(1, m.width)
	inputHeight := min(4, max(1, m.height-5))
	m.input.SetWidth(w)
	m.input.SetHeight(inputHeight)
	m.transcript.Width, m.transcript.Height = w, max(1, m.height-inputHeight-4)
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
			m.notice = "当前没有运行中的 Run，不能 Steer"
			return nil
		}
		kind = Steer
	} else if strings.HasPrefix(strings.TrimSpace(text), "/") {
		kind = ExecuteCommand
	} else if m.status.Running {
		m.notice = "Run 正在执行：Alt+S 发送 Steer，Ctrl+C 请求取消"
		return nil
	}
	return m.dispatch(Action{Kind: kind, Text: text}, text)
}
