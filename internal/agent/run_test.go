package agent

import (
	"CodeAgent/internal/llm"
	"context"
	"testing"
	"time"
)

func TestUsageAggregationPreservesReportedFields(t *testing.T) {
	run := &activeRun{usage: make(map[string]llm.Usage)}
	u := llm.Usage{Input: 2, Output: 3, Reasoning: 1, CacheRead: 4, CacheWrite: 5, Total: 14,
		Cost: llm.Cost{Input: 1, Output: 2, CacheRead: 4, CacheWrite: 8, Total: 15}}
	accumulateUsage(run, "first", u)
	accumulateUsage(run, "first", u)
	accumulateUsage(run, "second", u)
	want := llm.Usage{Input: 4, Output: 6, Reasoning: 2, CacheRead: 8, CacheWrite: 10, Total: 28,
		Cost: llm.Cost{Input: 2, Output: 4, CacheRead: 8, CacheWrite: 16, Total: 30}}
	if run.usage["first"] != want || run.usage["second"] != u {
		t.Fatalf("per-model usage: %+v", run.usage)
	}
	want = llm.Usage{Input: 6, Output: 9, Reasoning: 3, CacheRead: 12, CacheWrite: 15, Total: 42,
		Cost: llm.Cost{Input: 3, Output: 6, CacheRead: 12, CacheWrite: 24, Total: 45}}
	if run.totalUsage != want {
		t.Fatalf("total: %+v, want %+v", run.totalUsage, want)
	}
}

func TestHandleDonePublishesFirstResultToAllWaiters(t *testing.T) {
	h := newRunHandle("run", nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan RunResult, 16)
	errors := make(chan error, 16)
	for range 16 {
		go func() {
			result, err := h.Wait(ctx)
			results <- result
			errors <- err
		}()
	}
	h.setResult(RunResult{RunID: "run", State: RunStateSuccess, StopReason: RunStopCompleted})
	h.setResult(RunResult{RunID: "replacement", State: RunStateError})
	for range 16 {
		result, err := <-results, <-errors
		if err != nil || result.RunID != "run" || result.State != RunStateSuccess || result.StopReason != RunStopCompleted {
			t.Fatalf("result %+v, error %v", result, err)
		}
	}
}
