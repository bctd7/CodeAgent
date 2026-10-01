package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"CodeAgent/internal/agent"
	modelconfig "CodeAgent/internal/config"
	"CodeAgent/internal/llm"
	runtimeconfig "CodeAgent/internal/runtime/config"
	sessionconfig "CodeAgent/internal/session/config"
)

func testModels() *modelconfig.Config {
	return &modelconfig.Config{Providers: map[string]modelconfig.ProviderConfig{
		"local": {BaseURL: "https://example.invalid/v1", API: string(llm.APIKindOpenAI), APIKey: "secret-test-key", Headers: map[string]string{"Authorization": "secret-test-header"}, Models: map[string]modelconfig.ModelConfig{
			"a": {ID: "a", MaxTokens: 1024}, "b": {ID: "b", API: string(llm.APIKindAnthropic), MaxTokens: 1024},
		}},
	}}
}

func user(text string) *llm.UserMessage {
	return &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: text}}, Timestamp: time.Now()}
}

func answer(ctx context.Context, blocks []llm.ContentBlock, stop llm.StopReason) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant})
	w.Finish(llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, Content: blocks, StopReason: stop, RawStopReason: string(stop), Usage: llm.Usage{Input: 2, Output: 3, Total: 5}})
	return r, nil
}

func textStream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	return answer(ctx, []llm.ContentBlock{{Type: llm.TypeText, Text: "done"}}, llm.StopStop)
}

func newTestManager(t *testing.T, stream agent.StreamFunc, tools []agent.Tool) *Manager {
	t.Helper()
	m, err := NewManager(context.Background(), runtimeconfig.Config{DatabasePath: filepath.Join(t.TempDir(), "会话 # &%.db"), Models: testModels(), Stream: stream, Tools: tools, RecordTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return m
}

func openTestSession(t *testing.T, m *Manager) *Runtime {
	t.Helper()
	info, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: sessionconfig.ModelSelection{Ref: "local/a"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitRun(t *testing.T, handle *agent.RunHandle) agent.RunResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := handle.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for controlled execution")
	}
}

func trigger(t *testing.T, m *Manager, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", m.database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentOpenAndManagement(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	info, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: sessionconfig.ModelSelection{Ref: "local/a"}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *Runtime, 20)
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{})
			results <- r
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *Runtime
	for r := range results {
		if first == nil {
			first = r
		}
		if first != r {
			t.Fatal("same session initialized more than once")
		}
	}
	m2, err := NewManager(context.Background(), m.config)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close(context.Background())
	if _, err := m2.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{}); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("open occupied: %v", err)
	}
	if _, err := m2.ListSessions(context.Background(), sessionconfig.SessionListOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m2.RenameSession(context.Background(), info.ID, "foreign"); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("foreign mutation: %v", err)
	}
	if err := m.RenameSession(context.Background(), info.ID, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseSession(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if err := m2.RenameSession(context.Background(), info.ID, "short lock"); err != nil {
		t.Fatal(err)
	}
	r2, err := m2.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.ArchiveSession(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{}); !errors.Is(err, sessionconfig.ErrSessionArchived) {
		t.Fatalf("archived open: %v", err)
	}
	if err := m.UnarchiveSession(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestQueuePersistsOnlyStartedRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return textStream(ctx, req)
	}, []agent.Tool{})
	r := openTestSession(t, m)
	h1, err := r.Submit(context.Background(), user("first"))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, started)
	h2, err := r.Submit(context.Background(), user("queued"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetRun(context.Background(), h2.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("queued run persisted: %v", err)
	}
	h2.Cancel()
	if waitRun(t, h2).State != agent.RunStateAborted {
		t.Fatal("queue cancel failed")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := h1.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := r.BindWorkingDir(context.Background(), t.TempDir()); err == nil {
		t.Fatal("bound while active")
	}
	if err := m.ArchiveSession(context.Background(), r.SessionID()); err == nil {
		t.Fatal("archived active run")
	}
	close(release)
	if waitRun(t, h1).State != agent.RunStateSuccess {
		t.Fatal("first failed")
	}
	detail, err := r.GetRun(context.Background(), h1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if detail.Record.FinishedAt == nil || detail.Record.Steps != 1 || len(detail.Entries) != 2 {
		t.Fatalf("not fully saved before handle: %+v", detail)
	}
	if calls.Load() != 1 {
		t.Fatal("cancelled queued input invoked stream")
	}
	if _, err := r.GetRun(context.Background(), h2.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cancel fabricated a run")
	}
}

func TestPersistenceFailuresStopExecution(t *testing.T) {
	for _, stage := range []string{"start", "step", "assistant", "tool_start", "tool_result", "finish"} {
		t.Run(stage, func(t *testing.T) {
			var calls, executes atomic.Int32
			tool := agent.Tool{Tool: llm.Tool{Name: "test", Schema: []byte(`{"type":"object"}`)}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				executes.Add(1)
				return []llm.ContentBlock{{Type: llm.TypeText, Text: "ok"}}, nil
			}}
			m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
				if calls.Add(1) == 1 {
					return answer(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "same", Name: "test", Arguments: map[string]any{}}}, llm.StopToolUse)
				}
				return textStream(ctx, req)
			}, []agent.Tool{tool})
			r := openTestSession(t, m)
			statements := map[string]string{
				"start":       `CREATE TRIGGER fail BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'injected start'); END`,
				"step":        `CREATE TRIGGER fail BEFORE UPDATE OF steps ON runs BEGIN SELECT RAISE(ABORT,'injected step'); END`,
				"assistant":   `CREATE TRIGGER fail BEFORE INSERT ON entries WHEN json_extract(NEW.message_json,'$.Role')='assistant' BEGIN SELECT RAISE(ABORT,'injected assistant'); END`,
				"tool_start":  `CREATE TRIGGER fail BEFORE INSERT ON tool_executions BEGIN SELECT RAISE(ABORT,'injected tool start'); END`,
				"tool_result": `CREATE TRIGGER fail BEFORE INSERT ON entries WHEN json_extract(NEW.message_json,'$.Role')='tool' BEGIN SELECT RAISE(ABORT,'injected tool result'); END`,
				"finish":      `CREATE TRIGGER fail BEFORE UPDATE OF finished_at ON runs BEGIN SELECT RAISE(ABORT,'injected finish'); END`,
			}
			trigger(t, m, statements[stage])
			h, err := r.Submit(context.Background(), user("fail"))
			if err != nil {
				t.Fatal(err)
			}
			result := waitRun(t, h)
			if result.State != agent.RunStateError || !strings.Contains(result.ErrorMessage, "injected") {
				t.Fatalf("fault was masked: %+v", result)
			}
			if r.Snapshot().State != runtimeconfig.StateFaulted {
				t.Fatal("runtime not faulted")
			}
			if _, err := r.Submit(context.Background(), user("must not run")); !errors.Is(err, runtimeconfig.ErrFaulted) {
				t.Fatal(err)
			}
			if stage == "start" || stage == "step" {
				if calls.Load() != 0 {
					t.Fatal("model invoked before record")
				}
			}
			if stage == "assistant" || stage == "tool_start" {
				if executes.Load() != 0 {
					t.Fatal("unrecorded tool executed")
				}
			}
			if stage == "tool_result" && (executes.Load() != 1 || calls.Load() != 1) {
				t.Fatal("failed result caused continued execution")
			}
			if stage == "start" {
				if _, err := r.GetRun(context.Background(), h.ID()); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("start left partial data")
				}
			}
		})
	}
}

func TestParallelResultsAndRepeatedCallIDs(t *testing.T) {
	slowStarted, fastDone, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	tools := []agent.Tool{
		{Tool: llm.Tool{Name: "slow", Schema: []byte(`{"type":"object"}`)}, ParallelSafe: true, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			close(slowStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "slow"}}, nil
		}},
		{Tool: llm.Tool{Name: "fast", Schema: []byte(`{"type":"object"}`)}, ParallelSafe: true, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			defer close(fastDone)
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "fast"}}, nil
		}},
	}
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		switch calls.Add(1) {
		case 1:
			return answer(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "slow", Name: "slow", Arguments: map[string]any{}}, {Type: llm.TypeToolCall, ID: "reused", Name: "fast", Arguments: map[string]any{}}}, llm.StopToolUse)
		case 2:
			return answer(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "reused", Name: "unknown", Arguments: map[string]any{}}}, llm.StopToolUse)
		default:
			return textStream(ctx, req)
		}
	}, tools)
	r := openTestSession(t, m)
	h, err := r.Submit(context.Background(), user("parallel"))
	if err != nil {
		t.Fatal(err)
	}
	defer close(release)
	waitSignal(t, slowStarted)
	waitSignal(t, fastDone)
	deadline := time.Now().Add(3 * time.Second)
	var interim sessionconfig.RunDetail
	for time.Now().Before(deadline) {
		interim, err = r.GetRun(context.Background(), h.ID())
		if err != nil {
			t.Fatal(err)
		}
		if len(interim.Entries) == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(interim.Entries) != 3 || interim.Entries[2].ToolCallID != "reused" || interim.Record.FinishedAt != nil {
		t.Fatalf("fast result not persisted before slow exit: %+v", interim)
	}
	// Release explicitly here; deferred release protects early failures.
	release <- struct{}{}
	result := waitRun(t, h)
	if result.State != agent.RunStateSuccess {
		t.Fatal(result.ErrorMessage)
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ToolExecutions) != 2 {
		t.Fatalf("unknown tool has fake start: %+v", detail.ToolExecutions)
	}
	var associations []string
	for _, e := range detail.Entries {
		if e.ToolCallID == "reused" {
			associations = append(associations, e.AssistantEntryID)
		}
	}
	if len(associations) != 2 || associations[0] == associations[1] {
		t.Fatal("repeated call IDs crossed assistant records")
	}
	if detail.Record.Steps != 3 || detail.TotalUsage.Total != 15 {
		t.Fatalf("usage/steps mismatch: %+v", detail.Record)
	}
}

func TestCancelAndCloseTimeoutRetainOwnership(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	tool := agent.Tool{Tool: llm.Tool{Name: "wait", Schema: []byte(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		close(started)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}}
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		return answer(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "call", Name: "wait", Arguments: map[string]any{}}}, llm.StopToolUse)
	}, []agent.Tool{tool})
	r := openTestSession(t, m)
	h, err := r.Submit(context.Background(), user("close"))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, started)
	h2, err := r.Submit(context.Background(), user("queued"))
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if r.Snapshot().State != runtimeconfig.StateClosing {
		t.Fatal("timeout pretended closed")
	}
	if _, err := acquireOwnership(context.Background(), m.database, r.SessionID()); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("released before tools exited: %v", err)
	}
	if waitRun(t, h2).State != agent.RunStateAborted {
		t.Fatal("queue close did not complete")
	}
	close(release)
	result := waitRun(t, h)
	if result.State != agent.RunStateAborted {
		t.Fatalf("close cancellation: %+v", result)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reopen under the same manager, retaining committed result and model.
	r2, err := m.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := r2.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if detail.Record.State != sessionconfig.RunStateAborted || len(detail.ToolExecutions) != 1 || detail.ToolExecutions[0].FinishedAt == nil {
		t.Fatalf("cancelled result not saved: %+v", detail)
	}
	if _, err := r2.GetRun(context.Background(), h2.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("closed queue persisted")
	}
}

func TestModelSelectionAndOriginRestore(t *testing.T) {
	var seenMu sync.Mutex
	var seen []llm.Request
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		seenMu.Lock()
		seen = append(seen, req)
		seenMu.Unlock()
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return answer(ctx, []llm.ContentBlock{{Type: llm.TypeThinking, Thinking: "private", ThinkingSignature: "signed"}, {Type: llm.TypeText, Text: "done"}}, llm.StopStop)
		}
		return textStream(ctx, req)
	}, []agent.Tool{})
	r := openTestSession(t, m)
	h, _ := r.Submit(context.Background(), user("first"))
	waitSignal(t, started)
	if err := r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "local/b"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if waitRun(t, h).State != agent.RunStateSuccess {
		t.Fatal("old request was cancelled")
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if detail.Entries[1].Origin.ModelID != "a" {
		t.Fatal("pending model polluted actual origin")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err = m.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().Model.Ref != "local/b" {
		t.Fatal("selection not restored")
	}
	h, _ = r.Submit(context.Background(), user("second"))
	if waitRun(t, h).State != agent.RunStateSuccess {
		t.Fatal("second failed")
	}
	seenMu.Lock()
	second := seen[1]
	seenMu.Unlock()
	if second.Model.ID != "b" || second.Model.API != llm.APIKindAnthropic {
		t.Fatal("new protocol not used")
	}
	for _, msg := range second.Messages {
		if a, ok := msg.(*llm.AssistantMessage); ok {
			for _, b := range a.Content {
				if b.Type == llm.TypeThinking {
					t.Fatal("cross model thinking leaked")
				}
			}
		}
	}
	// Same-origin recovery must retain signature, not lose its pointer-keyed source.
	if err := r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "local/a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err = m.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h, _ = r.Submit(context.Background(), user("third"))
	waitRun(t, h)
	seenMu.Lock()
	third := seen[2]
	seenMu.Unlock()
	found := false
	for _, msg := range third.Messages {
		if a, ok := msg.(*llm.AssistantMessage); ok {
			for _, b := range a.Content {
				if b.Type == llm.TypeThinking && b.ThinkingSignature == "signed" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("same-origin signature lost on restore")
	}
	if err := r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "missing"}); err == nil {
		t.Fatal("missing reference accepted")
	}
	trigger(t, m, `CREATE TRIGGER fail_model BEFORE UPDATE OF model_selection_json ON sessions BEGIN SELECT RAISE(ABORT,'model save failed'); END`)
	if err := r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "local/b"}); err == nil {
		t.Fatal("failed save accepted")
	}
	if r.Snapshot().Model.Ref != "local/a" || r.Snapshot().Agent.CurrentModel.ID != "a" {
		t.Fatal("save failure changed selection")
	}
	db, err := sql.Open("sqlite", m.database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var selections string
	if err := db.QueryRow(`SELECT group_concat(model_selection_json) FROM sessions`).Scan(&selections); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(selections, "secret") || strings.Contains(fmt.Sprint(r.Snapshot()), "secret") {
		t.Fatal("credential persisted or exposed")
	}
}

func TestManagerCloseAttemptsAllInstances(t *testing.T) {
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	release := make(chan struct{})
	tool := agent.Tool{Tool: llm.Tool{Name: "wait", Schema: []byte(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release
		return nil, ctx.Err()
	}}
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		return answer(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "wait", Name: "wait", Arguments: map[string]any{}}}, llm.StopToolUse)
	}, []agent.Tool{tool})
	r1, r2 := openTestSession(t, m), openTestSession(t, m)
	h1, _ := r1.Submit(context.Background(), user("one"))
	h2, _ := r2.Submit(context.Background(), user("two"))
	waitSignal(t, started)
	waitSignal(t, started)
	firstClosed := make(chan error, 1)
	go func() { firstClosed <- m.CloseSession(context.Background(), r1.SessionID()) }()
	waitSignal(t, cancelled)
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	waitSignal(t, cancelled)
	if _, err := r2.Submit(context.Background(), user("late")); !errors.Is(err, runtimeconfig.ErrClosing) {
		t.Fatal("manager shutdown accepted input", err)
	}
	if _, err := m.store.GetSession(context.Background(), r1.SessionID()); err != nil {
		t.Fatal("database closed before tool drain", err)
	}
	close(release)
	waitRun(t, h1)
	waitRun(t, h2)
	if err := <-firstClosed; err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFinishFailureDiscardsQueueWithoutStartingIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		calls.Add(1)
		close(started)
		<-release
		return textStream(ctx, req)
	}, []agent.Tool{})
	r := openTestSession(t, m)
	trigger(t, m, `CREATE TRIGGER fail_finish BEFORE UPDATE OF finished_at ON runs BEGIN SELECT RAISE(ABORT,'cannot finish'); END`)
	h1, _ := r.Submit(context.Background(), user("first"))
	waitSignal(t, started)
	h2, _ := r.Submit(context.Background(), user("second"))
	close(release)
	if waitRun(t, h1).State != agent.RunStateError || waitRun(t, h2).State != agent.RunStateAborted {
		t.Fatal("failure queue did not terminate")
	}
	if calls.Load() != 1 {
		t.Fatal("queue continued after failed finish")
	}
	if _, err := r.GetRun(context.Background(), h2.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("discarded queue run created")
	}
}

func TestCompletionEventsObserveSavedResult(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	var r *Runtime
	var mu sync.Mutex
	var order []agent.EventKind
	finished := make(chan struct{})
	m.config.OnEvent = func(_ string, event agent.Event) {
		if event.Kind != agent.EventStepEnd && event.Kind != agent.EventRunEnd {
			return
		}
		detail, err := r.GetRun(context.Background(), event.RunID)
		if err != nil || detail.Record.FinishedAt == nil {
			t.Error("completion before database commit", err)
		}
		mu.Lock()
		order = append(order, event.Kind)
		mu.Unlock()
		if event.Kind == agent.EventRunEnd {
			close(finished)
		}
	}
	r = openTestSession(t, m)
	h, _ := r.Submit(context.Background(), user("events"))
	waitRun(t, h)
	waitSignal(t, finished)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != agent.EventStepEnd || order[1] != agent.EventRunEnd {
		t.Fatalf("completion event order: %v", order)
	}
}
