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

func answerWithBlocks(ctx context.Context, blocks []llm.ContentBlock, stop llm.StopReason) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant})
	w.Finish(llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, Content: blocks, StopReason: stop, RawStopReason: string(stop), Usage: llm.Usage{Input: 2, Output: 3, Reasoning: 1, Total: 5}})
	return r, nil
}

func textStream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeText, Text: "done"}}, llm.StopStop)
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

func waitRun(t *testing.T, handle *RunHandle) agent.RunResult {
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

// 并行结果及时保存由 execution_test 覆盖；此处保留跨响应重复 ID、步骤和用量断言。
func TestRepeatedCallIDsAndUsage(t *testing.T) {
	var calls, executes atomic.Int32
	tool := agent.Tool{Tool: llm.Tool{Name: "fast"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		executes.Add(1)
		return nil, nil
	}}
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		switch calls.Add(1) {
		case 1:
			return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "reused", Name: "fast", Arguments: map[string]any{}}}, llm.StopToolUse)
		case 2:
			return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "reused", Name: "unknown", Arguments: map[string]any{}}}, llm.StopToolUse)
		default:
			return textStream(ctx, req)
		}
	}, []agent.Tool{tool})
	r := openTestSession(t, m)
	h, err := r.Submit(context.Background(), user("reuse"))
	if err != nil {
		t.Fatal(err)
	}
	if result := waitRun(t, h); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if executes.Load() != 1 || len(detail.ToolExecutions) != 1 {
		t.Fatalf("unknown tool has fake start: %+v", detail.ToolExecutions)
	}
	var associations []string
	for _, e := range detail.Entries {
		if e.ToolCallID == "reused" {
			associations = append(associations, e.AssistantEntryID)
		}
	}
	if len(associations) != 2 || associations[0] == associations[1] {
		t.Fatal("repeated IDs crossed assistant entries")
	}
	if detail.Record.Steps != 3 || detail.TotalUsage.Input != 6 || detail.TotalUsage.Output != 9 || detail.TotalUsage.Reasoning != 3 || detail.TotalUsage.Total != 15 {
		t.Fatalf("usage/steps mismatch: %+v", detail)
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
		return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "call", Name: "wait", Arguments: map[string]any{}}}, llm.StopToolUse)
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

// 请求来源与签名恢复由 execution_test 覆盖，此处保留选择失败及凭据隔离断言。
func TestModelSelectionValidationSaveFailureAndSecrets(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	r := openTestSession(t, m)
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
		return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "wait", Name: "wait", Arguments: map[string]any{}}}, llm.StopToolUse)
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
	h3, err := r.Submit(context.Background(), user("third"))
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if waitRun(t, h1).State != agent.RunStateError {
		t.Fatal("failed finish was accepted")
	}
	for _, h := range []*RunHandle{h2, h3} {
		result := waitRun(t, h)
		if result.State != agent.RunStateAborted || result.ErrorMessage == "" {
			t.Fatal("failure queue did not terminate clearly", result)
		}
		if _, err := r.GetRun(context.Background(), h.ID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("discarded queue run created", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("queue continued after failed finish")
	}
}
