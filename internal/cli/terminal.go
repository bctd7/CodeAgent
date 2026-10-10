package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/tui"
)

type runtimeEnvelope struct {
	session string
	event   runtime.Event
}
type terminalRun struct {
	runtime *runtime.Runtime
	handle  *agent.RunHandle
	cancel  context.CancelFunc
	done    chan struct{}
	ctx     context.Context
}
type terminalBackend struct {
	mu            sync.Mutex
	app           *application
	ctx           context.Context
	events        chan tui.Event
	active        *terminalRun
	published     map[string]tui.Scope
	answered      map[string]bool
	items         []tui.Item
	needsSnapshot bool
	sequence      uint64
	exitCode      int
	stopped       bool
}

func (a *application) terminalChat(ctx context.Context) int {
	life, stop := context.WithCancel(ctx)
	defer stop()
	b := &terminalBackend{app: a, ctx: life, events: make(chan tui.Event, 256), published: map[string]tui.Scope{}, answered: map[string]bool{}, exitCode: ExitOK}
	initial := b.snapshot()
	finished := make(chan struct{})
	go func() { defer close(finished); b.loop() }()
	runner := a.options.TUIRun
	if runner == nil {
		runner = tui.Run
	}
	err := runner(life, tui.Config{Initial: initial, Commands: tuiCommands(), Handle: b.handle, Events: b.events}, tui.IO{In: a.streams.In, Out: a.streams.Out})
	closeErr := b.shutdown()
	stop()
	<-finished
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(a.streams.Err, a.safe(err.Error()))
		return ExitFailure
	}
	if closeErr != nil {
		fmt.Fprintln(a.streams.Err, a.safe(closeErr.Error()))
		return ExitFailure
	}
	if c := contextCode(ctx); c != ExitOK {
		return c
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exitCode
}

// Handler and polling serialize only presentation state. Runtime callbacks enqueue
// without taking this mutex, so rendering and slow commands cannot block the Driver.
func (b *terminalBackend) handle(ctx context.Context, action tui.Action) ([]tui.Event, error) {
	if action.Kind == tui.Send {
		return b.start(ctx, action.Text)
	}
	if action.Kind == tui.Quit {
		if err := b.shutdown(); err != nil {
			return nil, err
		}
		return []tui.Event{tui.ExitEvent{}}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.app
	if b.stopped {
		return nil, runtime.ErrClosed
	}
	if action.SessionID != "" && action.SessionID != a.current.SessionID() {
		return nil, fmt.Errorf("会话已切换，操作已过期")
	}
	switch action.Kind {
	case tui.DismissNotice:
		for i, item := range b.items {
			if item.Kind == tui.NoticeItem && item.ID == action.NoticeID {
				b.items = append(b.items[:i], b.items[i+1:]...)
				break
			}
		}
		return []tui.Event{tui.NoticeDismissedEvent{ID: action.NoticeID}}, nil
	case tui.CancelRun:
		if b.active == nil {
			return nil, fmt.Errorf("没有当前活动任务")
		}
		b.active.cancel()
		if b.active.handle != nil {
			b.active.runtime.CancelRun(b.active.handle)
		}
		return []tui.Event{tui.NoticeEvent{Text: "已请求取消，等待 RunHandle 完成"}}, nil
	case tui.Steer:
		if b.active == nil {
			return nil, fmt.Errorf("没有运行中的任务")
		}
		return nil, a.current.Steer(action.Text)
	case tui.ListModels:
		events, _, err := b.page(ctx, "/model")
		return events, err
	case tui.ListSessions:
		events, _, err := b.page(ctx, "/sessions")
		return events, err
	case tui.AnswerApproval:
		choice := map[tui.ApprovalDecision]permission.Choice{tui.ApproveOnce: permission.Once, tui.ApproveSessionTool: permission.SessionTool, tui.Reject: permission.Reject}[action.Decision]
		for _, item := range a.pending() {
			if item.approval.ID == action.ApprovalID {
				if scope, ok := b.published[action.ApprovalID]; !ok || scope.SessionID != action.SessionID || scope.RunID != action.RunID {
					return nil, runtime.ErrStaleApproval
				}
				if err := item.runtime.Approve(item.approval.ID, choice); err != nil {
					return nil, err
				}
				b.answered[item.approval.ID] = true
				return []tui.Event{tui.ApprovalResolvedEvent{Scope: b.published[item.approval.ID], ID: item.approval.ID}}, nil
			}
		}
		return nil, runtime.ErrStaleApproval
	case tui.SelectModel:
		action.Text = "/model " + action.ChoiceID
	case tui.SelectSession:
		action.Text = "/session " + action.ChoiceID
	case tui.ExecuteCommand:
	default:
		return nil, fmt.Errorf("unsupported frontend action")
	}
	if name, arg, _ := strings.Cut(strings.TrimSpace(action.Text), " "); strings.TrimSpace(arg) == "" {
		if events, ok, err := b.page(ctx, name); ok {
			return events, err
		}
	}
	if b.active != nil {
		name, _, _ := strings.Cut(strings.TrimSpace(action.Text), " ")
		switch name {
		case "/open", "/new", "/session", "/model", "/mcp", "/plan", "/build":
			if name != "/model" || strings.TrimSpace(action.Text) != "/model" {
				return nil, fmt.Errorf("任务仍活动；先 /cancel 并等待结束")
			}
		}
	}
	result, err := a.command(ctx, action.Text)
	if err != nil {
		return nil, fmt.Errorf("%s", a.safe(err.Error()))
	}
	if result.exit {
		// Do not hold presentation mutex while waiting for the active handle.
		b.mu.Unlock()
		err = b.shutdown()
		b.mu.Lock()
		if err != nil {
			return nil, err
		}
		return []tui.Event{tui.ExitEvent{}}, nil
	}
	if result.cancel {
		if b.active == nil {
			return nil, fmt.Errorf("没有当前活动任务")
		}
		b.active.cancel()
		if b.active.handle != nil {
			b.active.runtime.CancelRun(b.active.handle)
		}
		return []tui.Event{tui.NoticeEvent{Text: "已请求取消"}}, nil
	}
	if result.prompt != "" {
		b.mu.Unlock()
		events, e := b.start(ctx, result.prompt)
		b.mu.Lock()
		return events, e
	}
	if result.form != nil {
		return []tui.Event{*result.form}, nil
	}
	var events []tui.Event
	if result.reset {
		var alerts []tui.Item
		for _, item := range b.items {
			if item.Kind == tui.NoticeItem {
				alerts = append(alerts, item)
			}
		}
		b.items = alerts
		b.published = map[string]tui.Scope{}
		b.answered = map[string]bool{}
		events = append(events, tui.SnapshotEvent{Snapshot: b.snapshot()})
	}
	if action.Page != "" && !result.reset && result.form == nil && refreshAfterMenuCommand(action.Text) {
		pages, ok, e := b.page(ctx, action.Page)
		if e != nil {
			return nil, e
		}
		if ok {
			return append([]tui.Event{tui.NoticeEvent{Text: "操作已完成"}}, pages...), nil
		}
	}
	if result.value != nil {
		if result.choices != "" {
			choiceEvents, err := b.choices(ctx, result.choices)
			if err != nil {
				return nil, err
			}
			events = append(events, choiceEvents...)
		} else if result.reset {
			text := "已切换会话 · " + a.current.SessionID()[:8] + " · " + a.selectedRef
			if strings.TrimSpace(action.Text) == "/new" {
				text = "已创建新会话 · " + a.selectedRef
			}
			events = append(events, tui.NoticeEvent{Text: text})
		} else if strings.HasPrefix(strings.TrimSpace(action.Text), "/model ") {
			events = append(events, tui.NoticeEvent{Text: "已切换模型 · " + a.selectedRef})
		} else {
			text, err := formatCommandValue(result.value)
			if err != nil {
				return nil, err
			}
			name, _, _ := strings.Cut(strings.TrimSpace(action.Text), " ")
			events = append(events, tui.PanelEvent{Scope: tui.Scope{SessionID: a.current.SessionID()}, Title: name, Text: a.safe(text)})
		}
	}
	events = append(events, tui.StatusEvent{Scope: tui.Scope{SessionID: a.current.SessionID()}, Status: b.status()})
	return events, nil
}
func (b *terminalBackend) choices(ctx context.Context, kind tui.ActionKind) ([]tui.Event, error) {
	a := b.app
	choices := []tui.Choice{}
	if kind == tui.ListModels {
		for _, ref := range a.modelRefs {
			info := a.modelInfo(ref)
			detail := "ID: " + info.ID + " · REF: " + ref
			if ref == a.selectedRef {
				detail = "当前模型 · " + detail
			}
			choices = append(choices, tui.Choice{ID: ref, Label: info.DisplayName, Detail: detail})
		}
	} else {
		sessions, err := a.listMainSessions(ctx, "", "all")
		if err != nil {
			return nil, err
		}
		for _, s := range sessions {
			choices = append(choices, tui.Choice{ID: s.ID, Label: s.ID + " " + s.Title, Detail: sessionDetail(s)})
		}
	}
	return []tui.Event{tui.ChoicesEvent{Scope: tui.Scope{SessionID: a.current.SessionID()}, Kind: kind, Choices: choices}}, nil
}
func (b *terminalBackend) noticeItem(text string) tui.Item {
	b.sequence++
	return tui.Item{ID: fmt.Sprintf("notice/%d", b.sequence), Kind: tui.NoticeItem, Text: text}
}
func (b *terminalBackend) retainNotice(text string) tui.Item {
	for _, item := range b.items {
		if item.Kind == tui.NoticeItem && item.Text == text {
			return item
		}
	}
	item := b.noticeItem(text)
	b.items = append(b.items, item)
	return item
}
func (b *terminalBackend) scope() tui.Scope {
	s := b.status()
	return tui.Scope{SessionID: s.SessionID, RunID: s.RunID}
}
func (b *terminalBackend) status() tui.Status {
	a := b.app
	s := a.current.Snapshot()
	running := b.active != nil || s.Agent.Status == agent.AgentStatusRunning || s.Agent.Status == agent.AgentStatusCancelling
	run := s.Agent.CurrentRunID
	if b.active != nil && b.active.handle != nil {
		run = b.active.handle.ID()
	}
	detail := a.capabilities.Instructions.ProjectRoot
	if s.Fault != nil {
		detail += " · " + a.safe(s.Fault.Error())
	}
	shortID := s.SessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return tui.Status{SessionID: s.SessionID, SessionTitle: filepath.Base(a.capabilities.Instructions.ProjectRoot) + " [" + shortID + "]", Model: a.selectedRef, ModelName: displayModelName(a.selectedRef, s.Agent.CurrentModel.ID, s.Agent.CurrentModel.DisplayName), RunID: run, State: string(s.Agent.Status), Detail: detail, Running: running}
}
func (b *terminalBackend) snapshot() tui.Snapshot {
	a := b.app
	// Session retains trusted entry kind/source; protocol user shape alone does
	// not mean a person spoke. Use the final projection, including on restore.
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var items []tui.Item
	if session, err := a.service.OpenSession(ctx, a.current.SessionID()); err == nil {
		if records, err := session.GetContext(ctx); err == nil {
			var messages []llm.Message
			var sources []sc.ContextRecord
			for _, record := range records {
				if record.Message != nil {
					messages = append(messages, record.Message)
					sources = append(sources, record)
				}
			}
			items = historyItems(a, messages, sources)
		} else {
			b.retainNotice(a.safe("读取会话展示失败：" + err.Error()))
		}
	} else {
		b.retainNotice(a.safe("读取会话展示失败：" + err.Error()))
	}
	// Alerts stay separate from chronological chat, and survive recovery until dismissed.
	var notices []tui.Notice
	for _, item := range b.items {
		if item.Kind == tui.NoticeItem {
			notices = append(notices, tui.Notice{ID: item.ID, Text: item.Text})
		}
	}
	return tui.Snapshot{Status: b.status(), Items: items, Notices: notices}
}
func (b *terminalBackend) push(event tui.Event) {
	select {
	case b.events <- event:
	default:
		b.needsSnapshot = true
	}
}
func (b *terminalBackend) start(ctx context.Context, text string) ([]tui.Event, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("empty prompt")
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return b.handle(ctx, tui.Action{Kind: tui.ExecuteCommand, Text: text})
	}
	b.mu.Lock()
	if b.stopped || b.active != nil || b.app.busy() {
		b.mu.Unlock()
		return nil, runtime.ErrBusy
	}
	taskCtx, cancel := context.WithCancel(b.ctx)
	if b.app.invocation.timeout > 0 {
		cancel()
		taskCtx, cancel = context.WithTimeout(b.ctx, b.app.invocation.timeout)
	}
	active := &terminalRun{runtime: b.app.current, cancel: cancel, ctx: taskCtx, done: make(chan struct{})}
	b.active = active
	b.mu.Unlock()
	handle, err := active.runtime.Submit(taskCtx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: text}}, Timestamp: time.Now()})
	b.mu.Lock()
	active.handle = handle
	if err != nil {
		b.active = nil
		cancel()
		close(active.done)
		b.mu.Unlock()
		return nil, err
	}
	status := b.status()
	b.mu.Unlock()
	go b.wait(active)
	return []tui.Event{tui.StatusEvent{Scope: tui.Scope{SessionID: status.SessionID}, Status: status}}, nil
}
func (b *terminalBackend) wait(active *terminalRun) {
	result, err := active.handle.Wait(active.ctx)
	if active.ctx.Err() != nil {
		active.runtime.CancelRun(active.handle)
		cleanup, stop := b.app.cleanupContext()
		result, err = active.handle.Wait(cleanup)
		stop()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	defer close(active.done)
	defer active.cancel()
	if b.active != active {
		return
	}
	b.active = nil
	code := ExitFailure
	if result.State == agent.RunStateSuccess {
		code = ExitOK
	} else if result.State == agent.RunStateAborted {
		code = ExitCancelled
	} else if result.State == agent.RunStateDeferred {
		code = ExitDeferred
	}
	if active.ctx.Err() != nil {
		code = contextCode(active.ctx)
	}
	b.exitCode = code
	message := result.ErrorMessage
	if err != nil {
		message = err.Error()
	}
	if message != "" && code != ExitCancelled {
		b.retainNotice(b.app.safe(message))
	}
	// RunHandle.Wait is the sole completion source. Snapshot replaces partial stream.
	b.push(tui.SnapshotEvent{Snapshot: b.snapshot()})
	if code == ExitCancelled {
		b.push(tui.NoticeEvent{Text: "任务已取消"})
	}
	b.published = map[string]tui.Scope{}
}
func (b *terminalBackend) shutdown() error {
	b.mu.Lock()
	active := b.active
	b.stopped = true
	if active != nil {
		active.cancel()
		if active.handle != nil {
			active.runtime.CancelRun(active.handle)
		}
	}
	b.mu.Unlock()
	if active != nil {
		ctx, stop := b.app.cleanupContext()
		defer stop()
		select {
		case <-active.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.app.closeResources()
}
func (b *terminalBackend) loop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-b.app.rawOverflow:
			b.mu.Lock()
			b.needsSnapshot = true
			b.mu.Unlock()
		case e := <-b.app.rawEvents:
			b.mu.Lock()
			if !b.stopped {
				b.project(e)
			}
			b.mu.Unlock()
		case <-ticker.C:
			b.mu.Lock()
			if !b.stopped {
				if b.needsSnapshot {
					select {
					case b.events <- tui.SnapshotEvent{Snapshot: b.snapshot()}:
						b.needsSnapshot = false
						b.published = map[string]tui.Scope{}
					default:
					}
				}
				b.push(tui.StatusEvent{Scope: tui.Scope{SessionID: b.app.current.SessionID()}, Status: b.status()})
				b.syncApprovals()
			}
			b.mu.Unlock()
		}
	}
}
func (b *terminalBackend) syncApprovals() {
	present := map[string]bool{}
	scope := b.scope()
	for _, item := range b.app.pending() {
		id := item.approval.ID
		present[id] = true
		if b.answered[id] {
			continue
		}
		if old, ok := b.published[id]; ok && old == scope {
			continue
		}
		p := item.approval
		b.push(tui.ApprovalEvent{Scope: scope, Approval: tui.Approval{ID: id, Source: fmt.Sprintf("session=%s run=%s hook_ask=%t reason=%s", item.sessionID, p.RunID, p.HookAsk, p.Reason), Tool: p.ToolName, Parameters: b.app.safe(p.Arguments + fmt.Sprintf("\nTargets: %v", p.FileTargets))}})
		b.published[id] = scope
	}
	for id, scope := range b.published {
		if !present[id] {
			b.push(tui.ApprovalResolvedEvent{Scope: scope, ID: id})
			delete(b.published, id)
			delete(b.answered, id)
		}
	}
}

func textBlocks(blocks []llm.ContentBlock) string {
	var texts []string
	for _, v := range blocks {
		if v.Type == llm.TypeText {
			texts = append(texts, v.Text)
		}
	}
	return strings.Join(texts, "\n")
}
func historyItems(a *application, messages []llm.Message, origins ...[]sc.ContextRecord) []tui.Item {
	out := []tui.Item{}
	session := a.current.SessionID()
	for i, msg := range messages {
		id := fmt.Sprintf("%s/history/%d", session, i)
		switch m := msg.(type) {
		case *llm.UserMessage:
			kind, text := tui.UserItem, textBlocks(m.Content)
			if len(origins) > 0 && i < len(origins[0]) && origins[0][i].Kind == sc.ContextEntry {
				kind = tui.NoticeItem
				if origins[0][i].SourceKind == "child_run" {
					text = formatChildContext(text)
				}
			}
			out = append(out, tui.Item{ID: id, Kind: kind, Text: a.safe(text)})
		case *llm.AssistantMessage:
			if text := textBlocks(m.Content); text != "" {
				out = append(out, tui.Item{ID: id, Kind: tui.AssistantItem, Text: a.safe(text)})
			}
			for _, call := range m.Content {
				if call.Type == llm.TypeToolCall {
					raw, _ := json.Marshal(call.Arguments)
					out = append(out, tui.Item{ID: session + "/tool/" + call.ID, Kind: tui.ToolItem, Tool: &tui.Tool{Name: call.Name, State: "requested", Parameters: a.safe(string(raw))}})
				}
			}
		case *llm.ToolResultMessage:
			text := a.safe(textBlocks(m.Content))
			state := "completed"
			if m.IsError {
				state = "failed"
			}
			found := false
			for j := range out {
				if out[j].ID == session+"/tool/"+m.ToolCallID && out[j].Tool != nil {
					out[j].Tool.Output = text
					out[j].Tool.State = state
					out[j].Tool.Summary = shortText(text)
					found = true
				}
			}
			if !found {
				out = append(out, tui.Item{ID: id, Kind: tui.ToolItem, Tool: &tui.Tool{Name: m.ToolName, State: state, Summary: shortText(text), Output: text}})
			}
		}
	}
	return out
}
func shortText(s string) string {
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}
func (b *terminalBackend) project(envelope runtimeEnvelope) {
	a := b.app
	valid := false
	var origin *runtime.Runtime
	for _, r := range a.host.Opened() {
		if r.SessionID() == envelope.session {
			valid = true
			origin = r
			break
		}
	}
	if !valid {
		return
	}
	e := envelope.event
	if e.Dropped > 0 {
		b.needsSnapshot = true
	}
	// Status precedes run-scoped items, including notifications received before the
	// asynchronous Send handler has returned to Bubble Tea.
	b.push(tui.StatusEvent{Scope: tui.Scope{SessionID: a.current.SessionID()}, Status: b.status()})
	notice := func(text string) {
		item := b.retainNotice(a.safe(text))
		b.push(tui.NoticeEvent{Scope: b.scope(), ID: item.ID, Text: item.Text, Persistent: true})
	}
	if e.Diagnostic != nil {
		notice(e.Diagnostic.Error())
	}
	if e.Notices != nil {
		for _, text := range e.Notices.SystemMessages {
			notice(text)
		}
		for _, report := range e.Notices.Reports {
			if report.StatusMessage != "" {
				notice(report.StatusMessage)
			}
			if report.Error != nil {
				notice(report.Error.Error())
			}
		}
	}
	ev := e.Agent
	if ev == nil {
		return
	}
	if ev.RunID != "" && ev.RunID != origin.Snapshot().Agent.CurrentRunID {
		if b.active == nil || b.active.handle == nil || b.active.handle.ID() != ev.RunID {
			return
		}
	}
	if b.active == nil {
		return
	} // late events cannot overwrite a completed run's history
	id := fmt.Sprintf("%s/%s/%d", envelope.session, ev.RunID, ev.StepIndex)
	if ev.LLMEvent != nil {
		message := ev.LLMEvent.Snapshot
		if ev.LLMEvent.FinalMessage != nil {
			message = ev.LLMEvent.FinalMessage
		}
		if message != nil {
			text := textBlocks(message.Content)
			if text != "" {
				if envelope.session != a.current.SessionID() {
					text = "子任务 " + envelope.session + "\n" + text
				}
				b.push(tui.ItemEvent{Scope: b.scope(), Item: tui.Item{ID: id, Kind: tui.AssistantItem, Text: a.safe(text)}})
			}
		}
	}
	if ev.Message != nil {
		if m, ok := ev.Message.(*llm.UserMessage); ok {
			kind, text := tui.UserItem, textBlocks(m.Content)
			if envelope.session != a.current.SessionID() {
				kind = tui.NoticeItem
				text = "子任务输入\n" + text
			}
			b.push(tui.ItemEvent{Scope: b.scope(), Item: tui.Item{ID: id + "/input", Kind: kind, Text: a.safe(text)}})
		}
	}
	if ev.ToolName != "" {
		tool := &tui.Tool{Name: ev.ToolName, State: string(ev.Kind), Summary: a.safe(ev.Detail)}
		if ev.ToolOutcome != nil {
			raw, _ := json.Marshal(ev.ToolOutcome.Call.Arguments)
			tool.Parameters = a.safe(string(raw))
			result := ev.ToolOutcome.Result
			if result == nil {
				result = ev.ToolOutcome.RawResult
			}
			if result != nil {
				tool.Output = a.safe(textBlocks(result.Content))
				tool.Summary = shortText(tool.Output)
			}
			tool.State = string(ev.ToolOutcome.Status)
		}
		if ev.ContentBlock != nil {
			tool.Output = a.safe(ev.ContentBlock.Text)
			tool.Summary = shortText(tool.Output)
		}
		b.push(tui.ItemEvent{Scope: b.scope(), Item: tui.Item{ID: envelope.session + "/tool/" + ev.ToolCallID, Kind: tui.ToolItem, Tool: tool}})
	}
}
