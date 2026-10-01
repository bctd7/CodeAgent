package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"CodeAgent/internal/llm"
)

type memoryRecorder struct {
	start  func(context.Context) error
	finish func(context.Context, RunResult) error
}

func (m memoryRecorder) StartRun(ctx context.Context, _ string, _ *llm.UserMessage) error {
	if m.start != nil {
		return m.start(ctx)
	}
	return nil
}
func (m memoryRecorder) RecordStep(context.Context, string) error { return nil }
func (m memoryRecorder) AppendMessage(context.Context, string, llm.Message, *ModelOrigin) (string, error) {
	return "assistant-entry", nil
}
func (m memoryRecorder) RecordToolStart(context.Context, string, string) error { return nil }
func (m memoryRecorder) AppendToolResult(context.Context, string, *llm.ToolResultMessage) error {
	return nil
}
func (m memoryRecorder) FinishRun(ctx context.Context, r RunResult) error {
	if m.finish != nil {
		return m.finish(ctx, r)
	}
	return nil
}

func testAnswer(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StatePending})
	w.Finish(llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopStop, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "done"}}})
	return r, nil
}

func TestCompletionSavedBeforeHandleAndEvents(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var a *Agent
	var mu sync.Mutex
	var kinds []EventKind
	completed := make(chan struct{})
	var err error
	a, err = New(Config{Stream: testAnswer, Recorder: memoryRecorder{finish: func(ctx context.Context, result RunResult) error {
		// Snapshot would deadlock if the state mutex covered persistence.
		s := a.Snapshot()
		if s.CurrentRunID != result.RunID {
			t.Error("execution released before commit")
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}, OnEvent: func(evt Event) {
		if evt.Kind != EventStepEnd && evt.Kind != EventRunEnd {
			return
		}
		mu.Lock()
		kinds = append(kinds, evt.Kind)
		mu.Unlock()
		if evt.Kind == EventRunEnd {
			a.mu.Lock()
			active := a.currentRun
			a.mu.Unlock()
			if active != nil {
				t.Error("RunEnd published before result transfer")
			}
			close(completed)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	h, err := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("finish not reached")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if _, err := h.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("handle completed before commit")
	}
	cancel()
	if err := a.Steer("too late"); err == nil {
		t.Fatal("accepted steer while committing")
	}
	mu.Lock()
	n := len(kinds)
	mu.Unlock()
	if n != 0 {
		t.Fatal("completion event ran before commit")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := h.Wait(ctx)
	if err != nil || r.State != RunStateSuccess {
		t.Fatalf("result %+v: %v", r, err)
	}
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 2 || kinds[0] != EventStepEnd || kinds[1] != EventRunEnd {
		t.Fatalf("completion order: %v", kinds)
	}
}

func TestFinishTimeoutCompletesHandleWithStorageFailure(t *testing.T) {
	a, err := New(Config{Stream: testAnswer, RecordTimeout: 20 * time.Millisecond, Recorder: memoryRecorder{finish: func(ctx context.Context, _ RunResult) error { <-ctx.Done(); return ctx.Err() }}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	h, _ := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := h.Wait(ctx)
	if err != nil || r.State != RunStateError || !errors.Is(r.Error, context.DeadlineExceeded) {
		t.Fatalf("storage timeout hidden: %+v, %v", r, err)
	}
	if _, err := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser}); err == nil {
		t.Fatal("accepted after storage fault")
	}
}

func TestRecorderOptionalAndModelCallbackReentry(t *testing.T) {
	var a *Agent
	var once sync.Once
	var err error
	a, err = New(Config{Stream: testAnswer, OnEvent: func(evt Event) {
		if evt.Kind == EventModelSwitchRequested {
			once.Do(func() {
				if err := a.SetModel(llm.Model{ID: "second"}, llm.RequestOptions{}); err != nil {
					t.Error(err)
				}
			})
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Avoid sync.Once recursive entry by the dispatcher's existing event queue.
	if err := a.SetModel(llm.Model{ID: "first"}, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if a.Snapshot().CurrentModel.ID != "second" {
		t.Fatal("callback update did not apply")
	}
	h, err := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := h.Wait(ctx)
	if err != nil || r.State != RunStateSuccess {
		t.Fatal(r, err)
	}
}
