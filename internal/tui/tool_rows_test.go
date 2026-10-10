package tui

import (
	"context"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func toolRowsModel(width int, items []Item) *Model {
	m := New(context.Background(), Config{Initial: Snapshot{Status: Status{SessionID: "s", RunID: "r", Running: true}, Items: items}})
	m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	return m
}

func TestToolRowsLiveAndHistory(t *testing.T) {
	failure := Item{ID: "failed", Kind: ToolItem, Tool: &Tool{Name: "read", State: "requested"}}
	success := Item{ID: "success", Kind: ToolItem, Tool: &Tool{Name: "search", State: "succeeded", Summary: "found\n\tthree\r\nfiles", Output: "FULL_SUCCESS_OUTPUT", Parameters: "PRIVATE_PARAMETERS"}}
	m := toolRowsModel(80, []Item{failure})
	if strings.Contains(m.transcriptText(), "\n") {
		t.Fatal("requested tool occupies multiple rows")
	}
	failure.Tool = &Tool{Name: "read", State: "failed", Summary: "first error\nsecond error\tlast error", Output: "FULL_ERROR_OUTPUT", Parameters: "ALL_PARAMETERS", Diff: "ALL_DIFF"}
	m.Update(ItemEvent{Scope: Scope{SessionID: "s", RunID: "r"}, Item: failure})
	m.Update(ItemEvent{Scope: Scope{SessionID: "s", RunID: "r"}, Item: success})
	live := ansi.Strip(m.transcriptText())
	check := func(text, successState string) {
		t.Helper()
		if len(strings.Split(text, "\n")) != 2 {
			t.Fatalf("two tools should occupy two rows: %q", text)
		}
		for _, want := range []string{"read · failed · first error second error last error", "search · " + successState + " · found three files"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %q", want, text)
			}
		}
		for _, hidden := range []string{"FULL_", "ALL_PARAMETERS", "PRIVATE_PARAMETERS", "ALL_DIFF"} {
			if strings.Contains(text, hidden) {
				t.Fatalf("default row leaks full details: %q", text)
			}
		}
	}
	check(live, "succeeded")
	success.Tool.State = "completed"
	snapshot := Snapshot{Status: Status{SessionID: "s"}, Items: []Item{failure, success}}
	m.Update(SnapshotEvent{Snapshot: snapshot})
	history := ansi.Strip(m.transcriptText())
	check(history, "completed")
	if strings.Replace(live, "succeeded", "completed", 1) != history {
		t.Fatalf("live/history rows differ: %q / %q", live, history)
	}
	if !reflect.DeepEqual(m.items, snapshot.Items) {
		t.Fatal("display changed retained tool contents")
	}
	// Existing F4 details continue to reveal the full output only on demand.
	m.Update(tea.KeyMsg{Type: tea.KeyF4})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	expanded := ansi.Strip(m.transcriptText())
	for _, want := range []string{"FULL_ERROR_OUTPUT", "ALL_PARAMETERS", "ALL_DIFF"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("existing details lost %q: %q", want, expanded)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyF4})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	check(ansi.Strip(m.transcriptText()), "completed")
}

func TestToolRowsUnicodeWidthsAndResize(t *testing.T) {
	item := Item{ID: "unicode", Kind: ToolItem, Tool: &Tool{Name: "read", State: "failed", Summary: "中文🙂e\u0301👩‍💻\n更多结果" + strings.Repeat("中", 40), Output: "FULL_OUTPUT"}}
	m := toolRowsModel(80, []Item{item})
	for _, width := range []int{1, 4, 8, 13, 14, 20, 24, 30, 40, 80, 20, 80} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		row := ansi.Strip(m.transcriptText())
		if !utf8.ValidString(row) || strings.ContainsAny(row, "\n\r\t") || ansi.StringWidth(row) > width {
			t.Fatalf("width %d invalid row %q (%d cells)", width, row, ansi.StringWidth(row))
		}
		if width >= 13 && !strings.Contains(row, "read · failed") {
			t.Fatalf("width %d lost actual state: %q", width, row)
		}
		if strings.Contains(row, "\u200d") && !strings.Contains(row, "👩‍💻") {
			t.Fatalf("split emoji: %q", row)
		}
		if strings.Contains(row, "\u0301") && !strings.Contains(row, "e\u0301") {
			t.Fatalf("split combining sequence: %q", row)
		}
		if width == 80 && !strings.Contains(row, "中文🙂e\u0301👩‍💻 更多结果") {
			t.Fatalf("resize cache lost Unicode summary: %q", row)
		}
	}
	item.Tool.Name = strings.Repeat("长工具", 30)
	m.Update(ItemEvent{Item: item})
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 40})
	if row := m.transcriptText(); !strings.Contains(row, "failed") || ansi.StringWidth(row) > 20 {
		t.Fatalf("long name hides state or overflows: %q", row)
	}
}

func TestToolRowsKeepAnswersAndApprovalDetails(t *testing.T) {
	item := Item{ID: "t", Kind: ToolItem, Tool: &Tool{Name: "edit", State: "requested", Summary: "brief\nsummary", Output: "FULL_TOOL_OUTPUT"}}
	answer := Item{ID: "a", Kind: AssistantItem, Text: "回答第一段\n\n回答第二段"}
	m := toolRowsModel(30, []Item{item, answer})
	text := ansi.Strip(m.transcriptText())
	for _, want := range []string{"回答第一段", "回答第二段"} {
		if !strings.Contains(text, want) {
			t.Fatalf("answer lost %q", want)
		}
	}
	var submitted Action
	m.cfg.Handle = func(_ context.Context, a Action) ([]Event, error) { submitted = a; return nil, nil }
	approval := Approval{ID: "approve", Source: "controlled-source", Tool: "edit", Parameters: strings.Repeat("参数数据\n", 20) + "PARAMETER_END", Diff: strings.Repeat("diff data\n", 20) + "DIFF_END"}
	m.Update(ApprovalEvent{Scope: Scope{SessionID: "s", RunID: "r"}, Approval: approval})
	if !reflect.DeepEqual(m.approvals[0].Approval, approval) {
		t.Fatal("approval contents were shortened")
	}
	var viewed strings.Builder
	for pages := 0; pages < 100; pages++ {
		viewed.WriteString(ansi.Strip(m.approvalBody.View()))
		if m.approvalBody.AtBottom() {
			break
		}
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}
	for _, want := range []string{"controlled-source", "PARAMETER_END", "DIFF_END"} {
		if !strings.Contains(viewed.String(), want) {
			t.Fatalf("approval cannot show %q", want)
		}
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("approval decision not dispatched")
	}
	m.Update(cmd())
	if submitted.Kind != AnswerApproval || submitted.ApprovalID != approval.ID || submitted.Decision != ApproveOnce {
		t.Fatalf("wrong approval action: %+v", submitted)
	}
}
