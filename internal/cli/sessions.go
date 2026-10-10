package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"CodeAgent/internal/llm"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
)

type historyItem struct {
	ID      string       `json:"id"`
	RunID   string       `json:"run_id"`
	Kind    sc.EntryKind `json:"kind"`
	Message llm.Message  `json:"message"`
}
type sessionView struct {
	Info          sc.SessionInfo `json:"info"`
	Child         bool           `json:"child"`
	History       []historyItem  `json:"history"`
	Runs          []sc.RunRecord `json:"runs"`
	HistoryNotice string         `json:"history_notice,omitempty"`
}

func (a *application) sessions(ctx context.Context) error {
	i := a.invocation
	if i.action == "sessions:list" {
		out := []sc.SessionInfo{}
		skip := i.offset
		// Session Service pagination includes children; paginate the public results to
		// apply main-session filtering before the user-facing offset/limit.
		for offset := 0; len(out) < i.limit; offset += 200 {
			page, err := a.service.ListSessions(ctx, sc.SessionListOptions{ArchiveFilter: sc.ArchiveFilter(i.archive), Limit: 200, Offset: offset})
			if err != nil {
				return err
			}
			for _, info := range page {
				child, err := a.isChild(ctx, info.ID)
				if err != nil {
					return err
				}
				if child {
					continue
				}
				if skip > 0 {
					skip--
					continue
				}
				out = append(out, info)
				if len(out) == i.limit {
					break
				}
			}
			if len(page) < 200 {
				break
			}
		}
		if i.json {
			return json.NewEncoder(a.streams.Out).Encode(out)
		}
		for _, s := range out {
			if _, err := fmt.Fprintf(a.streams.Out, "%s\t%s\t%s\n", s.ID, s.Title, s.WorkingDir); err != nil {
				return err
			}
		}
		return nil
	}
	info, err := a.service.GetSession(ctx, i.sessionID)
	if err != nil {
		return err
	}
	child, err := a.isChild(ctx, info.ID)
	if err != nil {
		return err
	}
	out := sessionView{Info: info, Child: child, History: []historyItem{}, Runs: []sc.RunRecord{}}
	if info.ArchivedAt != nil {
		// Service.OpenSession rejects archives. Public Store queries can list runs,
		// but this CLI does not copy Session's ancestry algorithm to fabricate history.
		out.HistoryNotice = "archived session: metadata and runs only; history requires a public archived-history interface"
		err = a.store.WithReadTx(ctx, func(tx *store.Tx) error {
			var e error
			out.Runs, e = tx.ListRuns(ctx, info.ID, i.limit, i.offset)
			return e
		})
	} else {
		s, e := a.service.OpenSession(ctx, info.ID)
		if e != nil {
			return e
		}
		entries, e := s.GetCurrentHistory(ctx)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			out.History = append(out.History, historyItem{entry.ID, entry.RunID, entry.Kind, entry.Message})
		}
		out.Runs, err = s.ListRuns(ctx, i.limit, i.offset)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(a.streams.Out).Encode(out)
}
