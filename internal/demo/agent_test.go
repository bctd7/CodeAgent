package demo

import (
	"context"
	"testing"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

// 在线场景复用同一构造入口；此处只注入内存流，不访问网络。
func TestDemoAgentDriverAndOnlineEntry(t *testing.T) {
	stream := func(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
		r, w := llm.NewEventStream(ctx, llm.AssistantMessage{Role: llm.RoleAssistant})
		w.Finish(llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopStop,
			Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "answer"}}, Usage: llm.Usage{Input: 1, Output: 2, Total: 3}})
		return r, nil
	}
	a, err := newDemoAgent(agent.Config{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	h, err := a.Submit(context.Background(), userMsg("task"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := h.Wait(ctx)
	if err != nil || result.State != agent.RunStateSuccess {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	ResetGlobalBudget(40)
	defer ResetGlobalBudget(40)
	openai := &config.ModelEntry{Model: llm.Model{ID: "fake-a", API: llm.APIKindOpenAI}}
	anthropic := &config.ModelEntry{Model: llm.Model{ID: "fake-b", API: llm.APIKindAnthropic}}
	check := runOnlineStreamChatBatch(openai, anthropic, stream, false)
	if check.Status != "通过" {
		t.Fatal(check.Actual)
	}
}

func TestOfflineAcceptanceWithExplicitDriver(t *testing.T) {
	result := RunOfflineAcceptance(false)
	if result.FailedScenarios != 0 || result.PassedScenarios != result.TotalScenarios {
		for _, scenario := range result.Results {
			if scenario.Status != "通过" {
				t.Errorf("%s: %s", scenario.Name, scenario.Actual)
			}
		}
		t.Fatalf("offline acceptance: %d/%d passed", result.PassedScenarios, result.TotalScenarios)
	}
}
