package runtime

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/llm"
	runtimeconfig "CodeAgent/internal/runtime/config"
	sessionconfig "CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"

	"modernc.org/sqlite"
)

func localModels() *modelconfig.Config {
	return &modelconfig.Config{Providers: map[string]modelconfig.ProviderConfig{
		"local": {BaseURL: "https://example.invalid/v1", API: string(llm.APIKindOpenAI), APIKey: "secret-not-stored", Models: map[string]modelconfig.ModelConfig{
			"a": {ID: "a", MaxTokens: 1024}, "b": {ID: "b", API: string(llm.APIKindAnthropic), MaxTokens: 1024},
		}},
	}}
}

func answer(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	return response(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopStop, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "done"}}})
}
func response(ctx context.Context, msg llm.AssistantMessage) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant})
	switch msg.State {
	case llm.StateError:
		w.Fail(msg, nil)
	case llm.StateAborted:
		w.Abort(context.Canceled)
	default:
		w.Finish(msg)
	}
	return r, nil
}
func toolCalls(ctx context.Context, names ...string) (llm.StreamReader, error) {
	var blocks []llm.ContentBlock
	for i, name := range names {
		blocks = append(blocks, llm.ContentBlock{Type: llm.TypeToolCall, ID: fmt.Sprintf("call%d", i), Name: name, Arguments: map[string]any{}})
	}
	return response(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopToolUse, Content: blocks})
}
func user(text string) *llm.UserMessage {
	return &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: text}}}
}
func fixture(t *testing.T, cfg runtimeconfig.Config) (*Manager, *Runtime) {
	t.Helper()
	cfg.Models = localModels()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "runtime.sqlite")
	if cfg.RecordTimeout == 0 {
		cfg.RecordTimeout = 3 * time.Second
	}
	if cfg.Stream == nil {
		cfg.Stream = answer
	}
	if cfg.Tools == nil {
		cfg.Tools = []agent.Tool{}
	}
	m, err := NewManager(context.Background(), cfg)
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
	info, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: sessionconfig.ModelSelection{Ref: "local/a"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return m, r
}
func waitResult(t *testing.T, h *RunHandle) agent.RunResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("controlled execution timed out")
	}
}
func testDB(t *testing.T, m *Manager) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", m.database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func execSQL(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

var barrierID atomic.Int64

// Real SQLite callback stops the statement before commit, without adding production injection hooks.
func sqlBarrier(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()
	name := fmt.Sprintf("runtime_barrier_%d", barrierID.Add(1))
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return int64(0), nil
		case <-time.After(5 * time.Second):
			return nil, errors.New("test barrier timeout")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return name, entered, unblock
}

func TestToolStartSavingControlWindow(t *testing.T) {
	for _, control := range []string{"steer", "cancel", "close"} {
		t.Run(control, func(t *testing.T) {
			name, entered, release := sqlBarrier(t)
			var streams, executes atomic.Int64
			m, r := fixture(t, runtimeconfig.Config{Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
				if streams.Add(1) == 1 {
					return toolCalls(ctx, "work")
				}
				return answer(ctx, req)
			}, Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				executes.Add(1)
				return nil, nil
			}}}})
			t.Cleanup(release)
			db := testDB(t, m)
			execSQL(t, db, fmt.Sprintf("CREATE TRIGGER block_start BEFORE INSERT ON tool_executions BEGIN SELECT %s(); END", name))
			h, err := r.Submit(context.Background(), user("task"))
			if err != nil {
				t.Fatal(err)
			}
			await(t, entered)
			switch control {
			case "steer":
				if err := r.Steer("corrected"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				h.Cancel()
			case "close":
				r.startClose()
			}
			release()
			result := waitResult(t, h)
			if executes.Load() != 0 {
				t.Fatal("tool ran despite pre-start control")
			}
			if control == "steer" && result.State != agent.RunStateSuccess {
				t.Fatal(result)
			}
			if control != "steer" && result.State != agent.RunStateAborted {
				t.Fatal(result)
			}
			detail, err := m.service.OpenSession(context.Background(), r.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			saved, err := detail.GetRun(context.Background(), h.ID())
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.ToolExecutions) != 1 || saved.ToolExecutions[0].FinishedAt == nil || saved.ToolExecutions[0].ResultEntryID == "" {
				t.Fatal("start record left open", saved.ToolExecutions)
			}
			found := false
			for _, e := range saved.Entries {
				if msg, ok := e.Message.(*llm.ToolResultMessage); ok {
					found = msg.IsError && strings.Contains(msg.Content[0].Text, "未执行")
				}
			}
			if !found {
				t.Fatal("no truthful skipped result")
			}
		})
	}
}

func TestParallelResultsSavedBeforeSlowTool(t *testing.T) {
	slowStarted, fastStarted, releaseSlow, fastSaved := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	var streams atomic.Int64
	m, r := fixture(t, runtimeconfig.Config{Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		if streams.Add(1) == 1 {
			return toolCalls(ctx, "slow", "fast")
		}
		var order []string
		for _, msg := range req.Messages {
			if tr, ok := msg.(*llm.ToolResultMessage); ok {
				order = append(order, tr.ToolName)
			}
		}
		if strings.Join(order, ",") != "slow,fast" {
			return nil, fmt.Errorf("context order %v", order)
		}
		return answer(ctx, req)
	}, Tools: []agent.Tool{
		{Tool: llm.Tool{Name: "slow"}, ParallelSafe: true, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			close(slowStarted)
			select {
			case <-releaseSlow:
				return []llm.ContentBlock{{Type: llm.TypeText, Text: "slow"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}},
		{Tool: llm.Tool{Name: "fast"}, ParallelSafe: true, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			close(fastStarted)
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "fast"}}, nil
		}},
	}, OnEvent: func(_ string, e agent.Event) {
		if e.Kind == agent.EventToolEnd && e.ToolName == "fast" {
			close(fastSaved)
		}
	}})
	t.Cleanup(release)
	h, _ := r.Submit(context.Background(), user("parallel"))
	await(t, slowStarted)
	await(t, fastStarted)
	await(t, fastSaved)
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Entries) != 3 {
		t.Fatal("fast result not saved independently", len(detail.Entries))
	}
	if detail.Entries[2].Message.(*llm.ToolResultMessage).ToolName != "fast" {
		t.Fatal(detail.Entries)
	}
	release()
	result := waitResult(t, h)
	if result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	_ = m
}

func TestTerminalCommitGatesQueueHandleEventsAndHooks(t *testing.T) {
	name, entered, release := sqlBarrier(t)
	var calls, count atomic.Int64
	registry := hooks.New(nil)
	registry.RegisterCompletion(func(context.Context, hooks.Completion) error { count.Add(1); return nil })
	events := make(chan agent.EventKind, 4)
	m, r := fixture(t, runtimeconfig.Config{Hooks: registry, Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		calls.Add(1)
		return answer(ctx, req)
	}, OnEvent: func(_ string, e agent.Event) {
		if e.Kind == agent.EventStepEnd || e.Kind == agent.EventRunEnd {
			events <- e.Kind
		}
	}})
	t.Cleanup(release)
	db := testDB(t, m)
	execSQL(t, db, fmt.Sprintf("CREATE TRIGGER block_finish BEFORE UPDATE OF finished_at ON runs WHEN NEW.finished_at IS NOT NULL BEGIN SELECT %s(); END", name))
	h, _ := r.Submit(context.Background(), user("first"))
	await(t, entered)
	queued, _ := r.Submit(context.Background(), user("second"))
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err := h.Wait(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 1 || count.Load() != 0 {
		t.Fatal("next call or hook before commit")
	}
	select {
	case <-events:
		t.Fatal("early completion event")
	default:
	}
	var runs int
	if err := db.QueryRow("SELECT count(*) FROM runs").Scan(&runs); err != nil || runs != 1 {
		t.Fatal(runs, err)
	}
	release()
	if result := waitResult(t, h); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	if result := waitResult(t, queued); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	if count.Load() != 2 {
		t.Fatal(count.Load())
	}
	for _, expected := range []agent.EventKind{agent.EventStepEnd, agent.EventRunEnd, agent.EventStepEnd, agent.EventRunEnd} {
		select {
		case actual := <-events:
			if actual != expected {
				t.Fatal(actual, expected)
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	_ = waitResult(t, h)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("hook repeated")
	}
}

func TestStorageFailuresStopAndFinish(t *testing.T) {
	cases := map[string]string{
		"start":       "CREATE TRIGGER fail BEFORE INSERT ON runs BEGIN SELECT RAISE(FAIL,'injected-start'); END",
		"step":        "CREATE TRIGGER fail BEFORE UPDATE OF steps ON runs BEGIN SELECT RAISE(FAIL,'injected-step'); END",
		"response":    "CREATE TRIGGER fail BEFORE INSERT ON entries WHEN json_extract(NEW.message_json,'$.Role')='assistant' BEGIN SELECT RAISE(FAIL,'injected-response'); END",
		"tool_start":  "CREATE TRIGGER fail BEFORE INSERT ON tool_executions BEGIN SELECT RAISE(FAIL,'injected-tool-start'); END",
		"tool_result": "CREATE TRIGGER fail BEFORE INSERT ON entries WHEN json_extract(NEW.message_json,'$.Role')='tool' BEGIN SELECT RAISE(FAIL,'injected-tool-result'); END",
		"finish":      "CREATE TRIGGER fail BEFORE UPDATE OF finished_at ON runs WHEN NEW.finished_at IS NOT NULL BEGIN SELECT RAISE(FAIL,'injected-finish'); END",
	}
	for point, trigger := range cases {
		t.Run(point, func(t *testing.T) {
			var calls, executes, count atomic.Int64
			registry := hooks.New(nil)
			registry.RegisterCompletion(func(context.Context, hooks.Completion) error { count.Add(1); return nil })
			m, r := fixture(t, runtimeconfig.Config{Hooks: registry, Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
				if calls.Add(1) == 1 {
					return toolCalls(ctx, "work")
				}
				return answer(ctx, req)
			}, Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				executes.Add(1)
				return nil, nil
			}}}})
			execSQL(t, testDB(t, m), trigger)
			h, _ := r.Submit(context.Background(), user("failure"))
			result := waitResult(t, h)
			if result.State != agent.RunStateError || !strings.Contains(result.ErrorMessage, "injected-") {
				t.Fatal("first fault hidden", result)
			}
			if (point == "start" || point == "step") && calls.Load() != 0 {
				t.Fatal("model started before save")
			}
			if (point == "response" || point == "tool_start") && executes.Load() != 0 {
				t.Fatal("tool started before save")
			}
			if count.Load() != 0 || r.Snapshot().State != runtimeconfig.StateFaulted {
				t.Fatal("failure accepted as success")
			}
			if _, err := r.Submit(context.Background(), user("later")); !errors.Is(err, runtimeconfig.ErrFaulted) {
				t.Fatal(err)
			}
			if point != "start" && point != "finish" {
				detail, err := r.GetRun(context.Background(), h.ID())
				if err != nil || detail.Record.FinishedAt == nil || detail.Record.State != sessionconfig.RunStateError {
					t.Fatal(detail.Record, err)
				}
			}
		})
	}
}

func TestHookRejectionAndCompletionFailure(t *testing.T) {
	var executes, count, errorsSeen atomic.Int64
	registry := hooks.New(func(hooks.Failure) { errorsSeen.Add(1) })
	registry.RegisterBeforeTool(func(context.Context, hooks.ToolRequest) (hooks.ToolDecision, error) {
		return hooks.ToolDecision{Reason: "denied by policy"}, nil
	})
	registry.RegisterCompletion(func(context.Context, hooks.Completion) error { panic("completion panic") })
	registry.RegisterCompletion(func(context.Context, hooks.Completion) error { count.Add(1); return nil })
	var streams atomic.Int64
	_, r := fixture(t, runtimeconfig.Config{Hooks: registry, Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		if streams.Add(1) == 1 {
			return toolCalls(ctx, "work", "unknown")
		}
		return answer(ctx, req)
	}, Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		executes.Add(1)
		return nil, nil
	}}}})
	h, _ := r.Submit(context.Background(), user("hooks"))
	result := waitResult(t, h)
	if result.State != agent.RunStateSuccess || executes.Load() != 0 || count.Load() != 1 || errorsSeen.Load() != 1 {
		t.Fatal(result, executes.Load(), count.Load(), errorsSeen.Load())
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ToolExecutions) != 0 || len(detail.Entries) != 5 || detail.Record.State != sessionconfig.RunStateSuccess {
		t.Fatal(detail)
	}
}

func TestQueueCancellationNeverCreatesRun(t *testing.T) {
	started, releaseModel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseModel) }) }
	_, r := fixture(t, runtimeconfig.Config{Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		close(started)
		select {
		case <-releaseModel:
			return answer(ctx, req)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	t.Cleanup(release)
	h, _ := r.Submit(context.Background(), user("active"))
	await(t, started)
	queued, _ := r.Submit(context.Background(), user("queued"))
	queued.Cancel()
	if result := waitResult(t, queued); result.State != agent.RunStateAborted {
		t.Fatal(result)
	}
	if _, err := r.GetRun(context.Background(), queued.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("queued task persisted", err)
	}
	release()
	if result := waitResult(t, h); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	runs, err := r.ListRuns(context.Background(), 50, 0)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
}

func TestFailureCancelsInflightAndStillStoresResults(t *testing.T) {
	slowStarted, cancelled, releaseSlow := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseSlow) }) }
	var calls atomic.Int64
	m, r := fixture(t, runtimeconfig.Config{Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		calls.Add(1)
		return toolCalls(ctx, "slow", "fast")
	}, Tools: []agent.Tool{
		{Tool: llm.Tool{Name: "slow"}, ParallelSafe: true, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			close(slowStarted)
			<-ctx.Done()
			close(cancelled)
			<-releaseSlow
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "partial fact"}}, ctx.Err()
		}},
		{Tool: llm.Tool{Name: "fast"}, ParallelSafe: true, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			<-slowStarted
			return nil, nil
		}},
	}})
	t.Cleanup(release)
	db := testDB(t, m)
	execSQL(t, db, "CREATE TRIGGER fail_fast BEFORE INSERT ON entries WHEN NEW.tool_call_id='call1' BEGIN SELECT RAISE(FAIL,'first-save-fault'); END")
	h, _ := r.Submit(context.Background(), user("inflight failure"))
	await(t, cancelled)
	if r.Snapshot().State != runtimeconfig.StateFaulted {
		t.Fatal("fault not visible")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err := h.Wait(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("abandoned inflight tool", err)
	}
	if _, err := r.Submit(context.Background(), user("later")); !errors.Is(err, runtimeconfig.ErrFaulted) {
		t.Fatal(err)
	}
	release()
	result := waitResult(t, h)
	if result.State != agent.RunStateError || !strings.Contains(result.ErrorMessage, "first-save-fault") || calls.Load() != 1 {
		t.Fatal(result, calls.Load())
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range detail.Entries {
		if tr, ok := entry.Message.(*llm.ToolResultMessage); ok && tr.ToolCallID == "call0" {
			found = true
		}
	}
	if !found || detail.Record.FinishedAt == nil {
		t.Fatal("inflight facts or terminal not saved", detail)
	}
}

func TestCloseWaitsForCoordinationAndRetainsOwnership(t *testing.T) {
	name, entered, release := sqlBarrier(t)
	m, r := fixture(t, runtimeconfig.Config{})
	t.Cleanup(release)
	other, err := NewManager(context.Background(), m.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close(context.Background()) })
	db := testDB(t, m)
	execSQL(t, db, fmt.Sprintf("CREATE TRIGGER wait_response BEFORE INSERT ON entries WHEN json_extract(NEW.message_json,'$.Role')='assistant' BEGIN SELECT %s(); END", name))
	h, _ := r.Submit(context.Background(), user("close while saving"))
	await(t, entered)
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = r.Close(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := other.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{}); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatal("owner released before save", err)
	}
	release()
	if result := waitResult(t, h); result.State != agent.RunStateAborted {
		t.Fatal(result)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := other.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetRun(context.Background(), h.ID())
	if err != nil || detail.Record.State != sessionconfig.RunStateAborted || len(detail.Entries) != 2 {
		t.Fatal(detail, err)
	}
}

func TestModelSelectionPreparationAndOriginRestore(t *testing.T) {
	name, entered, releaseSQL := sqlBarrier(t)
	firstModel, releaseModel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseModel) }) }
	var calls atomic.Int64
	m, r := fixture(t, runtimeconfig.Config{Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		n := calls.Add(1)
		if n == 1 {
			if req.Model.ID != "a" {
				return nil, errors.New("prepared model changed")
			}
			close(firstModel)
			select {
			case <-releaseModel:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return response(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopToolUse, Content: []llm.ContentBlock{
				{Type: llm.TypeThinking, Thinking: "from-a", ThinkingSignature: "signature-a"}, {Type: llm.TypeToolCall, ID: "call", Name: "work", Arguments: map[string]any{}},
			}})
		}
		if req.Model.ID != "b" {
			return nil, errors.New("next request did not use selected model")
		}
		for _, msg := range req.Messages {
			if a, ok := msg.(*llm.AssistantMessage); ok {
				for _, b := range a.Content {
					if b.Thinking == "from-a" {
						return nil, errors.New("foreign thinking leaked")
					}
				}
			}
		}
		if n == 3 {
			found := false
			for _, msg := range req.Messages {
				if a, ok := msg.(*llm.AssistantMessage); ok {
					for _, b := range a.Content {
						if b.Thinking == "from-b" && b.ThinkingSignature == "signature-b" {
							found = true
						}
					}
				}
			}
			if !found {
				return nil, errors.New("restored thinking origin missing")
			}
		}
		return response(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopStop, Content: []llm.ContentBlock{{Type: llm.TypeThinking, Thinking: "from-b", ThinkingSignature: "signature-b"}, {Type: llm.TypeText, Text: "done"}}})
	}, Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		return nil, nil
	}}}})
	t.Cleanup(releaseSQL)
	t.Cleanup(release)
	db := testDB(t, m)
	execSQL(t, db, fmt.Sprintf("CREATE TRIGGER wait_step BEFORE UPDATE OF steps ON runs BEGIN SELECT %s(); END", name))
	h, _ := r.Submit(context.Background(), user("switch"))
	await(t, entered)
	selectionDone := make(chan error, 1)
	go func() {
		selectionDone <- r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "local/b"})
	}()
	releaseSQL()
	await(t, firstModel)
	select {
	case err := <-selectionDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model selection blocked")
	}
	release()
	if result := waitResult(t, h); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, entry := range detail.Entries {
		if entry.Origin != nil {
			sources = append(sources, entry.Origin.ModelID)
		}
	}
	if strings.Join(sources, ",") != "a,b" {
		t.Fatal(sources)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := reopened.Submit(context.Background(), user("restored"))
	if result := waitResult(t, h2); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	if reopened.Snapshot().Model.Ref != "local/b" {
		t.Fatal("selection not restored")
	}
}

func TestCompletionOnlyForCommittedSuccess(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "safety", "cancel", "length", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			var count atomic.Int64
			registry := hooks.New(nil)
			registry.RegisterCompletion(func(context.Context, hooks.Completion) error { count.Add(1); return nil })
			cfg := runtimeconfig.Config{Hooks: registry, Stream: func(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
				msg := llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopStop}
				switch outcome {
				case "failure":
					msg.State = llm.StateError
					msg.ErrorMessage = "provider error"
				case "safety":
					msg.State = llm.StateError
					msg.StopReason = llm.StopSafety
					msg.RawStopReason = "content_filter"
					msg.ErrorMessage = "filtered"
				case "cancel":
					msg.State = llm.StateAborted
				case "length":
					msg.StopReason = llm.StopLength
				case "panic":
					return toolCalls(ctx, "panic")
				}
				return response(ctx, msg)
			}}
			if outcome == "panic" {
				cfg.Tools = []agent.Tool{{Tool: llm.Tool{Name: "panic"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
					panic("tool panic")
				}}}
			}
			_, r := fixture(t, cfg)
			h, _ := r.Submit(context.Background(), user(outcome))
			result := waitResult(t, h)
			expected := int64(0)
			if outcome == "success" {
				expected = 1
			}
			if count.Load() != expected {
				t.Fatal(outcome, count.Load())
			}
			detail, err := r.GetRun(context.Background(), h.ID())
			if err != nil || string(detail.Record.State) != string(result.State) || detail.Record.StopReason != llm.StopReason(result.StopReason) {
				t.Fatal(detail.Record, result, err)
			}
		})
	}
}

func TestRuntimeSaveTimeoutAndFirstFault(t *testing.T) {
	m, r := fixture(t, runtimeconfig.Config{RecordTimeout: 20 * time.Millisecond})
	occupied, releasePool, transactionDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(releasePool) }) }
	t.Cleanup(release)
	go func() {
		transactionDone <- m.store.WithTx(context.Background(), func(*store.Tx) error { close(occupied); <-releasePool; return nil })
	}()
	await(t, occupied)
	h, err := r.Submit(context.Background(), user("save timeout"))
	if err != nil {
		t.Fatal(err)
	}
	result := waitResult(t, h)
	if result.State != agent.RunStateError || !errors.Is(result.Error, context.DeadlineExceeded) {
		t.Fatal(result)
	}
	if r.Snapshot().State != runtimeconfig.StateFaulted {
		t.Fatal("timeout was not a storage fault")
	}
	release()
	if err := <-transactionDone; err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetRun(context.Background(), h.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("failed start created history", err)
	}
}

func TestSharedHooksAcrossSessionsAndMetadata(t *testing.T) {
	var count atomic.Int64
	registry := hooks.New(nil)
	registry.RegisterCompletion(func(context.Context, hooks.Completion) error { count.Add(1); return nil })
	m, first := fixture(t, runtimeconfig.Config{Hooks: registry})
	info, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: sessionconfig.ModelSelection{Ref: "local/a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.OpenSession(context.Background(), info.ID, runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := first.Submit(context.Background(), user("first title"))
	h2, _ := second.Submit(context.Background(), user("second title"))
	if waitResult(t, h1).State != agent.RunStateSuccess || waitResult(t, h2).State != agent.RunStateSuccess || count.Load() != 2 {
		t.Fatal("shared success counter", count.Load())
	}
	got, err := m.service.GetSession(context.Background(), first.SessionID())
	if err != nil || got.Title != "first title" {
		t.Fatal(got, err)
	}
	if err := m.ArchiveSession(context.Background(), first.SessionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenSession(context.Background(), first.SessionID(), runtimeconfig.OpenSessionOptions{}); !errors.Is(err, sessionconfig.ErrSessionArchived) {
		t.Fatal(err)
	}
	if err := m.UnarchiveSession(context.Background(), first.SessionID()); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.OpenSession(context.Background(), first.SessionID(), runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h3, _ := reopened.Submit(context.Background(), user("new title must not replace old"))
	_ = waitResult(t, h3)
	got, err = m.service.GetSession(context.Background(), first.SessionID())
	if err != nil || got.Title != "first title" || count.Load() != 3 {
		t.Fatal(got, err, count.Load())
	}
}
