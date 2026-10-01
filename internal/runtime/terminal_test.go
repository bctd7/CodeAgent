package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	sessionconfig "CodeAgent/internal/session/config"
)

func TestTerminalResponsesAndToolPanic(t *testing.T) {
	for _, mode := range []string{"error", "safety", "length", "panic"} {
		t.Run(mode, func(t *testing.T) {
			var executed atomic.Int32
			tool := agent.Tool{Tool: llm.Tool{Name: "test", Schema: []byte(`{"type":"object"}`)}, Execute: func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				executed.Add(1)
				panic("controlled panic")
			}}
			m := newTestManager(t, func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
				blocks := []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "one", Name: "test", Arguments: map[string]any{}}}
				switch mode {
				case "length":
					return answerWithBlocks(ctx, blocks, llm.StopLength)
				case "panic":
					return answerWithBlocks(ctx, blocks, llm.StopToolUse)
				default:
					reader, writer := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StatePending})
					message := llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateError, Content: blocks, ErrorMessage: "controlled provider failure"}
					if mode == "safety" {
						message.StopReason = llm.StopSafety
						message.RawStopReason = "content_filter"
					}
					writer.Fail(message, nil)
					return reader, nil
				}
			}, []agent.Tool{tool})
			var mu sync.Mutex
			var order []agent.EventKind
			ended := make(chan struct{})
			m.config.OnEvent = func(_ string, event agent.Event) {
				if event.Kind == agent.EventStepEnd || event.Kind == agent.EventRunEnd {
					mu.Lock()
					order = append(order, event.Kind)
					mu.Unlock()
				}
				if event.Kind == agent.EventRunEnd {
					close(ended)
				}
			}
			r := openTestSession(t, m)
			h, err := r.Submit(context.Background(), user(mode))
			if err != nil {
				t.Fatal(err)
			}
			result := waitRun(t, h)
			waitSignal(t, ended)
			if mode == "length" {
				if result.State != agent.RunStateSuccess || result.StopReason != agent.RunStopTruncated {
					t.Fatal(result)
				}
			} else if result.State != agent.RunStateError {
				t.Fatal(result)
			}
			if mode == "safety" && (result.StopReason != agent.RunStopSafety || result.RawStopReason != "content_filter") {
				t.Fatal("safety semantics changed")
			}
			if mode == "panic" {
				if executed.Load() != 1 {
					t.Fatal("panic tool not invoked exactly once")
				}
			} else if executed.Load() != 0 {
				t.Fatal("terminal response executed tools")
			}
			detail, err := r.GetRun(context.Background(), h.ID())
			if err != nil {
				t.Fatal(err)
			}
			if detail.Record.State != sessionconfig.RunState(result.State) || detail.Record.StopReason != llm.StopReason(result.StopReason) {
				t.Fatal("database terminal state differs")
			}
			if mode != "panic" && len(detail.ToolExecutions) != 0 {
				t.Fatal("terminal response fabricated tool starts")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(order) != 2 || order[0] != agent.EventStepEnd || order[1] != agent.EventRunEnd {
				t.Fatal("terminal completion events repeated or out of order", order)
			}
		})
	}
}
