package session_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/session"
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
)

func storageFixture(t *testing.T) (*session.Service, *session.Session, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(db)
	info, err := svc.CreateSession(ctx, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := svc.OpenSession(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Separate connection is only for deliberately corrupting persisted data.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return svc, s, raw
}

func startStoredRun(t *testing.T, s *session.Session, id string) config.RunRecord {
	t.Helper()
	rec, err := s.StartRun(context.Background(), id, &llm.UserMessage{Role: llm.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func mutateStorage(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestSingleAndListRecordConversions(t *testing.T) {
	svc, s, raw := storageFixture(t)
	ctx := context.Background()
	if _, err := svc.GetSession(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	empty, err := s.GetCurrentHistory(ctx)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	for _, filled := range []bool{false, true} {
		if filled {
			stamp := "2026-10-02T01:02:03.123456789Z"
			mutateStorage(t, raw, `UPDATE sessions SET created_at=?,updated_at=?,archived_at=?,model_selection_json=? WHERE id=?`, stamp, stamp, stamp, `{"ref":"p/m"}`, s.SessionID())
		}
		one, err := svc.GetSession(ctx, s.SessionID())
		if err != nil {
			t.Fatal(err)
		}
		list, err := svc.ListSessions(ctx, config.SessionListOptions{ArchiveFilter: config.ArchiveFilterAll})
		if err != nil || len(list) != 1 || !reflect.DeepEqual(one, list[0]) {
			t.Fatal(one, list, err)
		}
		if filled && (one.ArchivedAt == nil || one.CreatedAt.Nanosecond() != 123456789 || one.ModelSelection == nil || one.ModelSelection.Ref != "p/m") {
			t.Fatal(one)
		}
		if !filled && (one.ArchivedAt != nil || one.ModelSelection != nil || one.HeadID != "") {
			t.Fatal(one)
		}
	}
	if err := svc.UnarchiveSession(ctx, s.SessionID()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		rec := startStoredRun(t, s, id)
		for _, finish := range []bool{false, true} {
			if finish {
				var err error
				rec, err = s.FinishRun(ctx, id, config.RunCompletion{State: config.RunStateSuccess, StopReason: llm.StopStop, RawStopReason: "end_turn"})
				if err != nil {
					t.Fatal(err)
				}
			}
			detail, err := s.GetRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			list, err := s.ListRuns(ctx, 10, 0)
			if err != nil || len(list) == 0 || !reflect.DeepEqual(list[0], rec) || !reflect.DeepEqual(detail.Record, rec) {
				t.Fatal(detail, list, err)
			}
			if detail.Entries == nil || detail.ToolExecutions == nil || detail.UsageByOrigin == nil {
				t.Fatal("nil detail collection")
			}
			pending, err := svc.ListUnfinishedRuns(ctx, s.SessionID())
			if err != nil || pending == nil {
				t.Fatal(pending, err)
			}
			if finish && len(pending) != 0 {
				t.Fatal(pending)
			}
			if !finish && (len(pending) != 1 || !reflect.DeepEqual(pending[0], rec)) {
				t.Fatal(pending)
			}
		}
	}
}

func TestRecordConversionsRejectCorruptData(t *testing.T) {
	for _, tc := range []struct{ table, column, value string }{
		{"sessions", "created_at", "bad"}, {"sessions", "updated_at", "bad"},
		{"sessions", "archived_at", "bad"}, {"sessions", "model_selection_json", "{"},
		{"runs", "started_at", "bad"}, {"runs", "finished_at", "bad"},
	} {
		t.Run(tc.table+"/"+tc.column, func(t *testing.T) {
			svc, s, raw := storageFixture(t)
			startStoredRun(t, s, "run")
			mutateStorage(t, raw, "UPDATE "+tc.table+" SET "+tc.column+"=?", tc.value)
			ctx := context.Background()
			if tc.table == "sessions" {
				if _, err := svc.GetSession(ctx, s.SessionID()); err == nil {
					t.Fatal("single accepted corrupt data")
				}
				if _, err := svc.ListSessions(ctx, config.SessionListOptions{ArchiveFilter: config.ArchiveFilterAll}); err == nil {
					t.Fatal("list accepted corrupt data")
				}
			} else {
				if _, err := s.GetRun(ctx, "run"); err == nil {
					t.Fatal("single accepted corrupt data")
				}
				if _, err := s.ListRuns(ctx, 10, 0); err == nil {
					t.Fatal("list accepted corrupt data")
				}
				if tc.column == "started_at" {
					if _, err := svc.ListUnfinishedRuns(ctx, ""); err == nil {
						t.Fatal("unfinished accepted corrupt data")
					}
				}
			}
		})
	}
}

func TestHistoryRejectsMissingNodesAndCycles(t *testing.T) {
	for _, mode := range []string{"head", "parent", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			_, s, raw := storageFixture(t)
			rec := startStoredRun(t, s, "run")
			want := "历史链中断"
			switch mode {
			case "head":
				mutateStorage(t, raw, `UPDATE sessions SET head_id='missing'`)
			case "parent":
				mutateStorage(t, raw, `UPDATE entries SET parent_id='missing'`)
			case "cycle":
				mutateStorage(t, raw, `UPDATE entries SET parent_id=?`, rec.InputEntryID)
				want = "循环历史链"
			}
			if _, err := s.GetCurrentHistory(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal(err)
			}
		})
	}
}

func TestToolCallAssociationAndUsage(t *testing.T) {
	svc, s, _ := storageFixture(t)
	ctx := context.Background()
	run := startStoredRun(t, s, "run")
	origin := &config.MessageModelOrigin{Provider: "p", API: llm.APIKindOpenAI, ModelID: "m", BaseURL: "https://example.invalid/v1"}
	u := llm.Usage{Input: 2, Output: 3, Reasoning: 1, CacheRead: 4, CacheWrite: 5, Total: 14, Cost: llm.Cost{Input: 1, Output: 2, CacheRead: 4, CacheWrite: 8, Total: 15}}
	appendCall := func(calls ...string) config.Entry {
		t.Helper()
		msg := &llm.AssistantMessage{Role: llm.RoleAssistant, State: llm.StateSuccess, StopReason: llm.StopToolUse, Usage: u}
		for _, id := range calls {
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.TypeToolCall, ID: id, Name: "add", Arguments: map[string]any{}})
		}
		e, err := s.AppendMessage(ctx, "run", msg, origin)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	result := &llm.ToolResultMessage{Role: llm.RoleTool, ToolCallID: "call", ToolName: "add", Usage: &u}
	for _, started := range []bool{true, false} {
		e := appendCall("call")
		if started {
			if _, err := s.RecordToolStart(ctx, e.ID, "call"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RecordToolStart(ctx, e.ID, "call"); !errors.Is(err, config.ErrToolResultConflict) {
				t.Fatal(err)
			}
		}
		if _, err := s.AppendToolResult(ctx, e.ID, result); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendToolResult(ctx, e.ID, result); !errors.Is(err, config.ErrToolResultConflict) {
			t.Fatal(err)
		}
		if _, err := s.RecordToolStart(ctx, e.ID, "call"); !errors.Is(err, config.ErrToolResultConflict) {
			t.Fatal(err)
		}
	}
	detail, err := s.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	want := llm.Usage{Input: 4, Output: 6, Reasoning: 2, CacheRead: 8, CacheWrite: 10, Total: 28, Cost: llm.Cost{Input: 2, Output: 4, CacheRead: 8, CacheWrite: 16, Total: 30}}
	if detail.TotalUsage != want || len(detail.UsageByOrigin) != 1 || detail.UsageByOrigin[0].Usage != want {
		t.Fatal(detail)
	}
	if len(detail.ToolExecutions) != 1 || detail.ToolExecutions[0].FinishedAt == nil {
		t.Fatal(detail.ToolExecutions)
	}
	otherInfo, err := svc.CreateSession(ctx, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.OpenSession(ctx, otherInfo.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := startStoredRun(t, other, "other-run")
	valid := appendCall("call")
	duplicate := appendCall("call", "call")
	for _, tc := range []struct{ entry, id, reason string }{
		{run.InputEntryID, "call", "不是 assistant"}, {foreign.InputEntryID, "call", "未找到"},
		{valid.ID, "missing", "未找到 ToolCallID"}, {duplicate.ID, "call", "重复的 ToolCallID"},
	} {
		if _, err := s.RecordToolStart(ctx, tc.entry, tc.id); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatal(err)
		}
		msg := *result
		msg.ToolCallID = tc.id
		if _, err := s.AppendToolResult(ctx, tc.entry, &msg); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatal(err)
		}
	}
	before, err := s.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Entries) != 7 || len(before.ToolExecutions) != 1 {
		t.Fatal("rejected operations left records", before)
	}
	if _, err := s.FinishRun(ctx, "run", config.RunCompletion{State: config.RunStateSuccess, StopReason: llm.StopStop}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordToolStart(ctx, valid.ID, "call"); !errors.Is(err, config.ErrRunFinished) {
		t.Fatal(err)
	}
	if _, err := s.AppendToolResult(ctx, valid.ID, result); !errors.Is(err, config.ErrRunFinished) {
		t.Fatal(err)
	}
}

func TestBranchHistoryDoesNotMixOtherRuns(t *testing.T) {
	_, s, _ := storageFixture(t)
	ctx := context.Background()
	finish := func(id string) {
		t.Helper()
		startStoredRun(t, s, id)
		if _, err := s.FinishRun(ctx, id, config.RunCompletion{State: config.RunStateSuccess, StopReason: llm.StopStop}); err != nil {
			t.Fatal(err)
		}
	}
	finish("a")
	finish("b")
	if err := s.SwitchBranch(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	finish("c")
	history, err := s.GetCurrentHistory(ctx)
	if err != nil || len(history) != 2 || history[0].RunID != "a" || history[1].RunID != "c" {
		t.Fatal(history, err)
	}
	b, err := s.GetRun(ctx, "b")
	if err != nil || len(b.Entries) != 1 || b.Entries[0].RunID != "b" || b.ToolExecutions == nil {
		t.Fatal(b, err)
	}
}
