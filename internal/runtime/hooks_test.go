package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/llm"
	runtimeconfig "CodeAgent/internal/runtime/config"
	"CodeAgent/internal/session/store"
)

func TestBeforeToolAndSaveHaveSeparateBudgets(t *testing.T) {
	hookEntered, allowHook, hookReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	registry := hooks.New(nil)
	registry.RegisterBeforeTool(func(ctx context.Context, _ hooks.ToolRequest) (hooks.ToolDecision, error) {
		close(hookEntered)
		select {
		case <-allowHook:
		case <-ctx.Done():
			return hooks.ToolDecision{}, ctx.Err()
		}
		close(hookReturned)
		return hooks.ToolDecision{}, nil
	})
	var calls, executes atomic.Int32
	m, r := fixture(t, runtimeconfig.Config{RecordTimeout: time.Second, Hooks: registry,
		Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			if calls.Add(1) == 1 {
				return toolCalls(ctx, "work")
			}
			return answer(ctx, req)
		}, Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			executes.Add(1)
			return nil, nil
		}}}})
	h, err := r.Submit(context.Background(), user("task"))
	if err != nil {
		t.Fatal(err)
	}
	await(t, hookEntered)
	// 占住存储连接；Hook 与保存各等待 650ms，合计超过一个预算，分别低于预算。
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	txDone := make(chan error, 1)
	go func() {
		txDone <- m.store.WithTx(context.Background(), func(*store.Tx) error { close(held); <-release; return nil })
	}()
	await(t, held)
	time.Sleep(650 * time.Millisecond)
	close(allowHook)
	await(t, hookReturned)
	time.Sleep(650 * time.Millisecond)
	unblock()
	if err := <-txDone; err != nil {
		t.Fatal(err)
	}
	result := waitResult(t, h)
	if result.State != agent.RunStateSuccess || executes.Load() != 1 || r.Snapshot().State != runtimeconfig.StateReady {
		t.Fatalf("hook exhausted save budget: %+v executions=%d state=%s", result, executes.Load(), r.Snapshot().State)
	}
}

func TestBeforeToolTimeoutFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"timeout", "database", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			failures := make(chan hooks.Failure, 2)
			registry := hooks.New(func(f hooks.Failure) { failures <- f })
			registry.RegisterBeforeTool(func(ctx context.Context, _ hooks.ToolRequest) (hooks.ToolDecision, error) {
				close(entered)
				if mode != "database" {
					<-ctx.Done()
				}
				// 故意不返回 context error，验证协调者/Registry 仍检查实际超时。
				return hooks.ToolDecision{}, nil
			})
			var calls, executes atomic.Int32
			m, r := fixture(t, runtimeconfig.Config{RecordTimeout: 200 * time.Millisecond, Hooks: registry,
				Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					if calls.Add(1) == 1 {
						return toolCalls(ctx, "work")
					}
					return answer(ctx, req)
				},
				Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
					executes.Add(1)
					return nil, nil
				}}}})
			if mode == "database" {
				execSQL(t, testDB(t, m), `CREATE TRIGGER reject_start BEFORE INSERT ON tool_executions BEGIN SELECT RAISE(ABORT,'real save failure'); END`)
			}
			h, err := r.Submit(context.Background(), user("task"))
			if err != nil {
				t.Fatal(err)
			}
			await(t, entered)
			if mode == "cancel" {
				h.Cancel()
			}
			result := waitResult(t, h)
			if executes.Load() != 0 {
				t.Fatal("tool executed")
			}
			detail, err := r.GetRun(context.Background(), h.ID())
			if err != nil {
				t.Fatal(err)
			}
			if len(detail.ToolExecutions) != 0 {
				t.Fatal("hook failure created tool start")
			}
			switch mode {
			case "timeout":
				if result.State != agent.RunStateSuccess || r.Snapshot().State != runtimeconfig.StateReady {
					t.Fatalf("hook timeout became persistence fault: %+v", result)
				}
				select {
				case f := <-failures:
					if !errors.Is(f.Error, context.DeadlineExceeded) {
						t.Fatal(f)
					}
				default:
					t.Fatal("hook timeout not observable")
				}
				if len(detail.Entries) != 4 {
					t.Fatalf("missing skipped result: %+v", detail)
				}
				toolResult := detail.Entries[2].Message.(*llm.ToolResultMessage)
				if !toolResult.IsError || !strings.Contains(toolResult.Content[0].Text, "deadline") {
					t.Fatal(toolResult)
				}
			case "cancel":
				if result.State != agent.RunStateAborted || r.Snapshot().State != runtimeconfig.StateReady {
					t.Fatal(result)
				}
			case "database":
				if result.State != agent.RunStateError || r.Snapshot().State != runtimeconfig.StateFaulted || !strings.Contains(result.ErrorMessage, "real save failure") {
					t.Fatal(result)
				}
			}
		})
	}
}

func TestSlowBeforeToolDoesNotBlockOtherResult(t *testing.T) {
	aStarted, returnA, bHookStarted, allowB := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseA, releaseB sync.Once
	unblockA := func() { releaseA.Do(func() { close(returnA) }) }
	unblockB := func() { releaseB.Do(func() { close(allowB) }) }
	registry := hooks.New(nil)
	registry.RegisterBeforeTool(func(ctx context.Context, req hooks.ToolRequest) (hooks.ToolDecision, error) {
		if req.Call.Name == "b" {
			select {
			case <-aStarted:
			case <-ctx.Done():
				return hooks.ToolDecision{}, ctx.Err()
			}
			close(bHookStarted)
			select {
			case <-allowB:
			case <-ctx.Done():
				return hooks.ToolDecision{}, ctx.Err()
			}
		}
		return hooks.ToolDecision{}, nil
	})
	aSaved := make(chan struct{})
	var calls atomic.Int32
	tools := []agent.Tool{
		{Tool: llm.Tool{Name: "a"}, ParallelSafe: true, Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			close(aStarted)
			select {
			case <-returnA:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "a done"}}, nil
		}},
		{Tool: llm.Tool{Name: "b"}, ParallelSafe: true, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return nil, nil
		}},
	}
	_, r := fixture(t, runtimeconfig.Config{Hooks: registry, Tools: tools, Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		if calls.Add(1) == 1 {
			return toolCalls(ctx, "a", "b")
		}
		return answer(ctx, req)
	}, OnEvent: func(_ string, e agent.Event) {
		if e.Kind == agent.EventToolEnd && e.ToolCallID == "call0" {
			close(aSaved)
		}
	}})
	t.Cleanup(unblockA)
	t.Cleanup(unblockB)
	h, err := r.Submit(context.Background(), user("parallel"))
	if err != nil {
		t.Fatal(err)
	}
	await(t, bHookStarted)
	unblockA()
	await(t, aSaved)
	detail, err := r.GetRun(context.Background(), h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Entries) != 3 || detail.Entries[2].ToolCallID != "call0" || detail.Record.FinishedAt != nil {
		t.Fatalf("A result blocked: %+v", detail)
	}
	select {
	case <-allowB:
		t.Fatal("B hook already released")
	default:
	}
	unblockB()
	if result := waitResult(t, h); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
}

func TestCompletionHookGetsFreshBudget(t *testing.T) {
	name, entered, release := sqlBarrier(t)
	remaining := make(chan time.Duration, 1)
	registry := hooks.New(nil)
	registry.RegisterCompletion(func(ctx context.Context, _ hooks.Completion) error {
		deadline, _ := ctx.Deadline()
		remaining <- time.Until(deadline)
		return ctx.Err()
	})
	m, r := fixture(t, runtimeconfig.Config{Hooks: registry, RecordTimeout: time.Second})
	execSQL(t, testDB(t, m), "CREATE TRIGGER delay_finish BEFORE UPDATE OF finished_at ON runs BEGIN SELECT "+name+"(); END")
	h, err := r.Submit(context.Background(), user("finish"))
	if err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	time.Sleep(600 * time.Millisecond)
	release()
	if result := waitResult(t, h); result.State != agent.RunStateSuccess {
		t.Fatal(result)
	}
	if budget := <-remaining; budget < 800*time.Millisecond {
		t.Fatalf("completion inherited save budget: %v", budget)
	}
}

func TestCloseWaitsForBeforeToolCleanup(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	registry := hooks.New(nil)
	registry.RegisterBeforeTool(func(ctx context.Context, _ hooks.ToolRequest) (hooks.ToolDecision, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		// 模拟响应取消后仍需收尾的 Hook；不能遗弃此工作就释放所有权。
		<-release
		return hooks.ToolDecision{}, nil
	})
	var executes atomic.Int32
	m, r := fixture(t, runtimeconfig.Config{Hooks: registry,
		Stream: func(ctx context.Context, _ llm.Request) (llm.StreamReader, error) { return toolCalls(ctx, "work") },
		Tools: []agent.Tool{{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			executes.Add(1)
			return nil, nil
		}}},
	})
	t.Cleanup(unblock)
	h, err := r.Submit(context.Background(), user("close"))
	if err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	r.startClose()
	await(t, cancelled)
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := acquireOwnership(context.Background(), m.database, r.SessionID()); !errors.Is(err, runtimeconfig.ErrSessionOwned) {
		t.Fatalf("ownership released during hook: %v", err)
	}
	if _, err := m.store.GetSession(context.Background(), r.SessionID()); err != nil {
		t.Fatal(err)
	}
	unblock()
	if result := waitResult(t, h); result.State != agent.RunStateAborted {
		t.Fatal(result)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executes.Load() != 0 {
		t.Fatal("cancelled hook started tool")
	}
	select {
	case <-r.driverDone:
	default:
		t.Fatal("driver still running after close")
	}
}
