package session

import (
	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"
)

func (s *Session) GetMemoryProgress(ctx context.Context, scope string) (config.MemoryProgress, error) {
	if strings.TrimSpace(scope) == "" {
		return config.MemoryProgress{}, fmt.Errorf("记忆范围不能为空")
	}
	var value config.MemoryProgress
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetSession(ctx, s.sessionID); err != nil {
			return err
		}
		var err error
		value, err = tx.GetMemoryProgress(ctx, s.sessionID, scope)
		return err
	})
	return value, err
}

// NextMemoryBatch 返回当前路径下第一个未覆盖、正常完成的主 Run；无待处理批次返回 nil,nil。
// 不推进进度、不创建任务、不调用长期记忆；宿主须逐批处理，不能跳过失败批次。
func (s *Session) NextMemoryBatch(ctx context.Context, scope string) (*config.MemoryBatch, error) {
	if strings.TrimSpace(scope) == "" {
		return nil, fmt.Errorf("记忆范围不能为空")
	}
	var result *config.MemoryBatch
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			return err
		}
		result, err = s.nextMemoryBatch(ctx, tx, scope, info.HeadID)
		return err
	})
	return result, err
}
func (s *Session) nextMemoryBatch(ctx context.Context, tx *store.Tx, scope, head string) (*config.MemoryBatch, error) {
	child, err := tx.IsChildSession(ctx, s.sessionID)
	if err != nil {
		return nil, err
	}
	if child {
		return nil, nil
	}
	path, err := s.historyAt(ctx, tx, head)
	if err != nil {
		return nil, err
	}
	progress, err := tx.GetMemoryProgress(ctx, s.sessionID, scope)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	if progress.CoveredEntryID != "" {
		old, err := s.historyAt(ctx, tx, progress.CoveredEntryID)
		if err != nil {
			return nil, err
		}
		for _, entry := range old {
			covered[entry.ID] = true
		}
	}
	// 同时属于旧覆盖链与当前链的记录才算已覆盖，分叉后的新内容不能靠 Sequence 跳过。
	seen := map[string]bool{}
	for _, entry := range path {
		if seen[entry.RunID] {
			continue
		}
		seen[entry.RunID] = true
		run, err := tx.GetRun(ctx, entry.RunID, s.sessionID)
		if err != nil {
			return nil, err
		}
		if run.FinishedAt == nil || run.State != config.RunStateSuccess || run.StopReason != llm.StopStop {
			continue
		}
		if covered[run.LastEntryID] {
			continue
		}
		entries := []config.Entry{}
		endFound := false
		for _, source := range path {
			if source.RunID == run.ID {
				if source.ID == run.LastEntryID {
					endFound = true
				}
				if source.Kind == config.MessageEntry {
					entries = append(entries, source)
				}
			}
		}
		if !endFound {
			continue
		}
		// ID 只由固定身份推导，不对消息正文做哈希或内容匹配。
		identity, _ := json.Marshal([]string{"session-memory-v1", s.sessionID, run.ID})
		id := uuid.NewSHA1(uuid.NameSpaceOID, identity).String()
		return &config.MemoryBatch{ID: id, SessionID: s.sessionID, ScopeKey: scope, RunID: run.ID, HeadID: head, EndEntryID: run.LastEntryID, ExpectedEntryID: progress.CoveredEntryID, Entries: entries}, nil
	}
	return nil, nil
}

// AdvanceMemoryProgress 只能在宿主确认 Consolidate 成功或 Duplicate 后调用。
// Session 无法验证外部文件提交；通过固定批次重算与旧边界比较，禁止越批推进。
func (s *Session) AdvanceMemoryProgress(ctx context.Context, batch config.MemoryBatch) error {
	if batch.SessionID != s.sessionID || strings.TrimSpace(batch.ScopeKey) == "" {
		return config.ErrSessionConflict
	}
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetSession(ctx, s.sessionID); err != nil {
			return err
		}
		progress, err := tx.GetMemoryProgress(ctx, s.sessionID, batch.ScopeKey)
		if err != nil {
			return err
		}
		if progress.CoveredEntryID != batch.ExpectedEntryID {
			return config.ErrSessionConflict
		}
		next, err := s.nextMemoryBatch(ctx, tx, batch.ScopeKey, batch.HeadID)
		if err != nil {
			return err
		}
		if next == nil || next.ID != batch.ID || next.RunID != batch.RunID || next.EndEntryID != batch.EndEntryID {
			return config.ErrSessionConflict
		}
		return tx.PutMemoryProgress(ctx, config.MemoryProgress{SessionID: s.sessionID, ScopeKey: batch.ScopeKey, CoveredEntryID: batch.EndEntryID, UpdatedAt: time.Now().UTC()})
	})
}
