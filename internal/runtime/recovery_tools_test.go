package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	runtimeconfig "CodeAgent/internal/runtime/config"
	sessionconfig "CodeAgent/internal/session/config"
)

func TestRecoveryOnlyOwnedSession(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	selection := sessionconfig.ModelSelection{Ref: "local/a"}
	a, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: selection})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateSession(context.Background(), runtimeconfig.CreateSessionOptions{Model: selection})
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := m.service.OpenSession(context.Background(), a.ID)
	s2, _ := m.service.OpenSession(context.Background(), b.ID)
	if _, err := s1.StartRun(context.Background(), "interrupted-a", user("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.StartRun(context.Background(), "live-b", user("b")); err != nil {
		t.Fatal(err)
	}
	entry, err := s1.AppendMessage(context.Background(), "interrupted-a", &llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopToolUse, Content: []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "unknown-result", Name: "test", Arguments: map[string]any{}}}}, &sessionconfig.MessageModelOrigin{Provider: "local", API: llm.APIKindOpenAI, ModelID: "a", BaseURL: "https://example.invalid/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RecordToolStart(context.Background(), entry.ID, "unknown-result"); err != nil {
		t.Fatal(err)
	}
	// Taking b's lock is sufficient to prove opening a cannot recover b.
	owner, err := acquireOwnership(context.Background(), m.database, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()
	var requests, executions atomic.Int32
	m.config.Stream = func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		requests.Add(1)
		return textStream(ctx, req)
	}
	m.config.Tools = []agent.Tool{{Tool: llm.Tool{Name: "test"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		executions.Add(1)
		return nil, nil
	}}}
	var events atomic.Int32
	m.config.OnEvent = func(string, agent.Event) { events.Add(1) }
	r, err := m.OpenSession(context.Background(), a.ID, runtimeconfig.OpenSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := r.GetRun(context.Background(), "interrupted-a")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Record.State != sessionconfig.RunStateAborted || detail.Record.ErrorMessage != "程序中断" || detail.ToolExecutions[0].FinishedAt != nil || len(detail.Entries) != 2 {
		t.Fatalf("recovery rewrote facts: %+v", detail)
	}
	unresolved, err := m.service.ListUnfinishedRuns(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 || unresolved[0].ID != "live-b" {
		t.Fatal("recovered session not owned")
	}
	if events.Load() != 0 {
		t.Fatal("recovery emitted replay events")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenSession(context.Background(), a.ID, runtimeconfig.OpenSessionOptions{}); err != nil {
		t.Fatal(err)
	}
	if events.Load() != 0 || requests.Load() != 0 || executions.Load() != 0 {
		t.Fatal("reopen replayed old requests or tools")
	}
}

func TestToolsStableBeforeAndAfterBinding(t *testing.T) {
	name := "runtime-proof-" + filepath.Base(filepath.Dir(t.TempDir())) + ".txt"
	var calls atomic.Int32
	var mu sync.Mutex
	var definitions [][]string
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		mu.Lock()
		var names []string
		for _, tool := range req.Tools {
			names = append(names, tool.Name)
		}
		definitions = append(definitions, names)
		mu.Unlock()
		if calls.Add(1)%2 == 1 {
			return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "write", Name: "write", Arguments: map[string]any{"path": name, "content": "exact"}}}, llm.StopToolUse)
		}
		return textStream(ctx, req)
	}, nil)
	r := openTestSession(t, m)
	h, err := r.Submit(context.Background(), user("unbound"))
	if err != nil {
		t.Fatal(err)
	}
	result := waitResult(t, h)
	if result.State != agent.RunStateSuccess {
		t.Fatal(result.ErrorMessage)
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	toolResult, ok := detail.Entries[2].Message.(*llm.ToolResultMessage)
	if !ok || !toolResult.IsError {
		t.Fatal("unbound tool used process cwd")
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("unbound write touched process cwd: %v", err)
	}
	if r.Snapshot().State != runtimeconfig.StateReady {
		t.Fatal("tool business error faulted runtime")
	}
	dir := t.TempDir()
	if err := r.BindWorkingDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := r.BindWorkingDir(context.Background(), dir); !errors.Is(err, sessionconfig.ErrWorkingDirAlreadyBound) {
		t.Fatalf("rebind allowed: %v", err)
	}
	h, err = r.Submit(context.Background(), user("bound"))
	if err != nil {
		t.Fatal(err)
	}
	result = waitResult(t, h)
	if result.State != agent.RunStateSuccess {
		t.Fatal(result.ErrorMessage)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(data) != "exact" {
		t.Fatalf("wrong execution directory: %q, %v", data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(definitions[0]) != 7 || len(definitions[2]) != 7 {
		t.Fatal("tools added only after binding")
	}
	for i := range definitions[0] {
		if definitions[0][i] != definitions[2][i] {
			t.Fatal("tool set changed at binding")
		}
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenSession(context.Background(), r.SessionID(), runtimeconfig.OpenSessionOptions{}); err == nil {
		t.Fatal("invalid bound directory silently replaced")
	}
	owner, err := acquireOwnership(context.Background(), m.database, r.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	owner.release()
}

func TestUserCancelPreservesQueueAndConsumedSteer(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		if calls.Add(1) == 1 {
			reader, _ := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StatePending})
			close(started)
			return reader, nil
		}
		return textStream(ctx, req)
	}, []agent.Tool{})
	r := openTestSession(t, m)
	h1, _ := r.Submit(context.Background(), user("cancel me"))
	await(t, started)
	if err := r.Steer("unconsumed"); err != nil {
		t.Fatal(err)
	}
	h2, _ := r.Submit(context.Background(), user("next"))
	h1.Cancel()
	if waitResult(t, h1).State != agent.RunStateAborted || waitResult(t, h2).State != agent.RunStateSuccess {
		t.Fatal("single cancel dropped next run")
	}
	detail, err := r.GetRun(context.Background(), h1.ID())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range detail.Entries {
		if u, ok := e.Message.(*llm.UserMessage); ok && u.Content[0].Text == "[用户纠正]: unconsumed" {
			t.Fatal("unconsumed steer persisted")
		}
	}
	if r.Snapshot().State != runtimeconfig.StateReady {
		t.Fatal("ordinary cancellation faulted instance")
	}
}

func TestSteerSkipsUnstartedToolsAndPersistsInput(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var executed, calls atomic.Int32
	tool := agent.Tool{Tool: llm.Tool{Name: "serial", Schema: []byte(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		if executed.Add(1) == 1 {
			close(started)
			<-release
		}
		return []llm.ContentBlock{{Type: llm.TypeText, Text: "done"}}, nil
	}}
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		if calls.Add(1) == 1 {
			return answerWithBlocks(ctx, []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "one", Name: "serial", Arguments: map[string]any{}}, {Type: llm.TypeToolCall, ID: "two", Name: "serial", Arguments: map[string]any{}}}, llm.StopToolUse)
		}
		return textStream(ctx, req)
	}, []agent.Tool{tool})
	r := openTestSession(t, m)
	h, _ := r.Submit(context.Background(), user("steer"))
	await(t, started)
	if err := r.Steer("change direction"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if waitResult(t, h).State != agent.RunStateSuccess {
		t.Fatal("steered run failed")
	}
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 1 || len(detail.ToolExecutions) != 1 {
		t.Fatal("skipped tool executed or has start record")
	}
	found := false
	skipped := false
	for _, e := range detail.Entries {
		if u, ok := e.Message.(*llm.UserMessage); ok && u.Content[0].Text == "[用户纠正]: change direction" {
			found = true
		}
		if result, ok := e.Message.(*llm.ToolResultMessage); ok && result.ToolCallID == "two" && result.IsError {
			skipped = true
		}
	}
	if !found || !skipped {
		t.Fatal("missing consumed steer or skipped tool result")
	}
}

func TestDifferentSessionsExecuteConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return textStream(ctx, req)
	}, []agent.Tool{})
	r1, r2 := openTestSession(t, m), openTestSession(t, m)
	h1, _ := r1.Submit(context.Background(), user("one"))
	h2, _ := r2.Submit(context.Background(), user("two"))
	await(t, started)
	await(t, started)
	close(release)
	if waitResult(t, h1).State != agent.RunStateSuccess || waitResult(t, h2).State != agent.RunStateSuccess {
		t.Fatal("different sessions failed")
	}
}

func TestConcurrentModelAndDirectoryUpdates(t *testing.T) {
	m := newTestManager(t, textStream, []agent.Tool{})
	r := openTestSession(t, m)
	dir := t.TempDir()
	var wg sync.WaitGroup
	failures := make(chan error, 31)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := "local/a"
			if i%2 == 1 {
				ref = "local/b"
			}
			failures <- r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: ref})
		}(i)
	}
	wg.Add(1)
	go func() { defer wg.Done(); failures <- r.BindWorkingDir(context.Background(), dir) }()
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	info, err := m.service.GetSession(context.Background(), r.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := r.Snapshot()
	if snapshot.Model != *info.ModelSelection || snapshot.Agent.CurrentModel.ID != string(info.ModelSelection.Ref[len("local/"):]) || snapshot.WorkingDir != dir {
		t.Fatal("database/agent configuration diverged")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// 归档后重新打开的等价覆盖保留在 execution_test 的 SharedHooksAcrossSessionsAndMetadata。
func TestModelEventCanCallRuntimeControls(t *testing.T) {
	callbackDone := make(chan struct{})
	m := newTestManager(t, textStream, []agent.Tool{})
	r := openTestSession(t, m)
	var once sync.Once
	m.config.OnEvent = func(_ string, evt agent.Event) {
		if evt.Kind != agent.EventModelSwitchRequested {
			return
		}
		once.Do(func() {
			defer close(callbackDone)
			if _, err := r.GetCurrentHistory(context.Background()); err != nil {
				t.Error(err)
			}
			if err := r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "local/a"}); err != nil {
				t.Error(err)
			}
		})
	}
	if err := r.SetModel(context.Background(), sessionconfig.ModelSelection{Ref: "local/b"}); err != nil {
		t.Fatal(err)
	}
	await(t, callbackDone)
	if r.Snapshot().Agent.CurrentModel.ID != "a" || r.Snapshot().Model.Ref != "local/a" {
		t.Fatal("callback reentry diverged")
	}
}
