package session

import (
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"context"
	"fmt"
)

// GetCurrentHistory 返回当前分支原始记录，包含有明确 Kind 的宿主上下文。
func (s *Session) GetCurrentHistory(ctx context.Context) ([]config.Entry, error) {
	var entries []config.Entry
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			return err
		}
		entries, err = s.historyAt(ctx, tx, info.HeadID)
		return err
	})
	return entries, err
}

// historyAt 只沿 ParentID，不把会话内其他分支的 Sequence 拼进来。
func (s *Session) historyAt(ctx context.Context, tx *store.Tx, head string) ([]config.Entry, error) {
	out := []config.Entry{}
	if head == "" {
		return out, nil
	}
	entries, err := tx.ListEntryAncestors(ctx, head, s.sessionID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for id := head; id != ""; {
		if seen[id] {
			return nil, fmt.Errorf("历史链循环: %s", id)
		}
		seen[id] = true
		entry, ok := entries[id]
		if !ok {
			return nil, fmt.Errorf("历史链断开: %s", id)
		}
		out = append(out, entry)
		id = entry.ParentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
