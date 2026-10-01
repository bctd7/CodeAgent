package agent

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"CodeAgent/internal/llm"
)

func testAnswer(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StatePending})
	w.Finish(llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopStop, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "done"}}})
	return r, nil
}

func nextCheckpoint(t *testing.T, d *Driver) Checkpoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := d.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func resolveCheckpoint(t *testing.T, d *Driver, c Checkpoint, decision Decision) {
	t.Helper()
	if err := d.Resolve(context.Background(), c.ID, decision); err != nil {
		t.Fatal(err)
	}
}

func drainDriver(a *Agent, d *Driver) {
	for {
		c, err := d.Next(context.Background())
		if err == io.EOF {
			return
		}
		if err != nil {
			a.StopExecution(err)
			continue
		}
		_ = d.Resolve(context.Background(), c.ID, Decision{Kind: Continue})
	}
}

func TestDriverGatesAndConfirmations(t *testing.T) {
	var calls atomic.Int64
	completed := make(chan EventKind, 2)
	a, err := New(Config{Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		calls.Add(1)
		return testAnswer(ctx, req)
	}, OnEvent: func(e Event) {
		if e.Kind == EventStepEnd || e.Kind == EventRunEnd {
			completed <- e.Kind
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser}); err == nil {
		t.Fatal("submit without driver")
	}
	d, _ := a.Driver()
	if _, err := a.Driver(); !errors.Is(err, ErrDriverClaimed) {
		t.Fatal(err)
	}
	t.Cleanup(func() { go drainDriver(a, d); a.Close() })
	h, err := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	start := nextCheckpoint(t, d)
	if start.Kind != RunStarting || calls.Load() != 0 {
		t.Fatal(start, calls.Load())
	}
	if err := d.Resolve(context.Background(), start.ID, Decision{Kind: Skip, Reason: "no"}); !errors.Is(err, ErrDecision) {
		t.Fatal(err)
	}
	if err := d.Resolve(context.Background(), start.ID+100, Decision{Kind: Continue}); !errors.Is(err, ErrCheckpoint) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Resolve(canceled, start.ID, Decision{Kind: Continue}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	resolveCheckpoint(t, d, start, Decision{Kind: Continue})
	if err := d.Resolve(context.Background(), start.ID, Decision{Kind: Continue}); !errors.Is(err, ErrCheckpoint) {
		t.Fatal(err)
	}
	request := nextCheckpoint(t, d)
	if request.Kind != ModelRequestReady || calls.Load() != 0 {
		t.Fatal(request, calls.Load())
	}
	resolveCheckpoint(t, d, request, Decision{Kind: Continue})
	response := nextCheckpoint(t, d)
	if response.Kind != ModelResponseReady || calls.Load() != 1 {
		t.Fatal(response)
	}
	resolveCheckpoint(t, d, response, Decision{Kind: Continue})
	finish := nextCheckpoint(t, d)
	if finish.Kind != RunFinishing {
		t.Fatal(finish)
	}
	select {
	case <-h.done:
		t.Fatal("handle finished before confirmation")
	default:
	}
	select {
	case <-completed:
		t.Fatal("completion event before confirmation")
	default:
	}
	if err := a.Steer("late"); err == nil {
		t.Fatal("steer accepted after finishing")
	}
	resolveCheckpoint(t, d, finish, Decision{Kind: Continue})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := h.Wait(ctx)
	if err != nil || result.State != RunStateSuccess {
		t.Fatal(result, err)
	}
	for _, expected := range []EventKind{EventStepEnd, EventRunEnd} {
		select {
		case actual := <-completed:
			if actual != expected {
				t.Fatal(actual, expected)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestToolStartReservationHonorsControl(t *testing.T) {
	for _, control := range []string{"steer", "cancel", "fault", "close", "reject"} {
		t.Run(control, func(t *testing.T) {
			var modelCalls, toolCalls atomic.Int64
			tool := Tool{Tool: llm.Tool{Name: "work"}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				toolCalls.Add(1)
				return nil, nil
			}}
			a, _ := New(Config{Tools: []Tool{tool}, Stream: func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
				if modelCalls.Add(1) > 1 {
					return testAnswer(ctx, req)
				}
				r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant})
				w.Finish(llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopToolUse, Content: []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "call", Name: "work", Arguments: map[string]any{}}}})
				return r, nil
			}})
			d, _ := a.Driver()
			t.Cleanup(func() { go drainDriver(a, d); a.Close() })
			h, _ := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser})
			for _, kind := range []CheckpointKind{RunStarting, ModelRequestReady, ModelResponseReady} {
				c := nextCheckpoint(t, d)
				if c.Kind != kind {
					t.Fatal(c)
				}
				resolveCheckpoint(t, d, c, Decision{Kind: Continue})
			}
			start := nextCheckpoint(t, d)
			if start.Kind != ToolStarting {
				t.Fatal(start)
			}
			decision := Decision{Kind: Continue}
			switch control {
			case "steer":
				if err := a.Steer("use new instructions"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				h.Cancel()
			case "fault":
				a.StopExecution(errors.New("external failure"))
			case "close":
				a.RequestClose()
			case "reject":
				decision = Decision{Kind: Skip, Reason: "permission denied"}
			}
			resolveCheckpoint(t, d, start, decision)
			result := nextCheckpoint(t, d)
			if result.Kind != ToolFinished || !result.ToolResult.IsError || toolCalls.Load() != 0 {
				t.Fatal(result, toolCalls.Load())
			}
			resolveCheckpoint(t, d, result, Decision{Kind: Continue})
			for {
				c := nextCheckpoint(t, d)
				resolveCheckpoint(t, d, c, Decision{Kind: Continue})
				if c.Kind == RunFinishing {
					break
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			final, err := h.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if control == "fault" && final.State != RunStateError {
				t.Fatal(final)
			}
		})
	}
}

func TestNextCancellationAndExclusiveWait(t *testing.T) {
	a, _ := New(Config{Stream: testAnswer})
	d, _ := a.Driver()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { _, err := d.Next(context.Background()); wait <- err }()
	deadline := time.Now().Add(time.Second)
	for !d.reading.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Next not waiting")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := d.Next(context.Background()); !errors.Is(err, ErrDriverBusy) {
		t.Fatal(err)
	}
	a.Close()
	if err := <-wait; err != io.EOF {
		t.Fatal(err)
	}
}

func TestCloseWaitsForDeliveredCheckpoint(t *testing.T) {
	a, _ := New(Config{Stream: testAnswer})
	d, _ := a.Driver()
	t.Cleanup(func() { go drainDriver(a, d); a.Close() })
	h, _ := a.Submit(context.Background(), &llm.UserMessage{Role: llm.RoleUser})
	c := nextCheckpoint(t, d)
	if c.Kind != RunStarting {
		t.Fatal(c)
	}
	a.RequestClose()
	select {
	case <-a.done:
		t.Fatal("closed while coordination still owned the checkpoint")
	default:
	}
	resolveCheckpoint(t, d, c, Decision{Kind: Continue})
	finish := nextCheckpoint(t, d)
	if finish.Kind != RunFinishing || finish.Result.State != RunStateAborted {
		t.Fatal(finish)
	}
	resolveCheckpoint(t, d, finish, Decision{Kind: Continue})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := h.Wait(context.Background())
	if err != nil || result.State != RunStateAborted {
		t.Fatal(result, err)
	}
}
