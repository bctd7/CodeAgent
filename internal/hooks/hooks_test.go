package hooks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"CodeAgent/internal/agent"
)

func ExampleRegistry_RegisterCompletion() {
	registry := New(nil)
	var successfulRuns atomic.Int64
	registry.RegisterCompletion(func(_ context.Context, completion Completion) error {
		successfulRuns.Add(1)
		return nil
	})
	// 业务中由 Runtime 在成功终态保存后调用。
	registry.Completed(context.Background(), Completion{Result: agent.RunResult{RunID: "example", State: agent.RunStateSuccess}})
	fmt.Println(successfulRuns.Load())
	// Output: 1
}

func TestConcurrentHooksAndPanic(t *testing.T) {
	var failures, count atomic.Int64
	r := New(func(Failure) { failures.Add(1) })
	r.RegisterCompletion(func(context.Context, Completion) error { panic("broken") })
	r.RegisterCompletion(func(context.Context, Completion) error { return errors.New("failed") })
	r.RegisterCompletion(func(context.Context, Completion) error { count.Add(1); return nil })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Completed(context.Background(), Completion{}) }()
	}
	wg.Wait()
	if count.Load() != 20 || failures.Load() != 40 {
		t.Fatal(count.Load(), failures.Load())
	}
	r.RegisterBeforeTool(func(context.Context, ToolRequest) (ToolDecision, error) { panic("reject") })
	if r.BeforeTool(context.Background(), ToolRequest{}).Reason == "" {
		t.Fatal("panic allowed tool")
	}
}
