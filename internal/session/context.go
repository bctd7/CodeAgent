package session

import (
	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"
)

// activeRun 集中校验写入位置，事务外不重复查询或自行修改 Head。
func (s *Session) activeRun(ctx context.Context, tx *store.Tx, runID string) (config.SessionInfo, config.RunRecord, error) {
	info, err := tx.GetSession(ctx, s.sessionID)
	if err != nil {
		return info, config.RunRecord{}, err
	}
	if info.ArchivedAt != nil {
		return info, config.RunRecord{}, config.ErrSessionArchived
	}
	run, err := tx.GetRun(ctx, runID, s.sessionID)
	if err != nil {
		return info, run, err
	}
	if run.FinishedAt != nil {
		return info, run, config.ErrRunFinished
	}
	if info.HeadID == "" || info.HeadID != run.LastEntryID {
		return info, run, config.ErrSessionConflict
	}
	return info, run, nil
}

func validateContext(input config.ContextInput) error {
	if strings.TrimSpace(input.SourceKind) == "" || strings.TrimSpace(input.SourceID) == "" {
		return fmt.Errorf("宿主上下文必须提供来源类别与事件 ID")
	}
	if input.Message == nil || input.Message.Role != llm.RoleUser {
		return fmt.Errorf("宿主上下文须为 user 形状的消息；通过 Entry.Kind 区分真实用户输入")
	}
	return nil
}

// AppendContext 保存宿主反馈；child_run 保留给 DeliverChildResult，不能伪造交付关联。
func (s *Session) AppendContext(ctx context.Context, runID string, input config.ContextInput) (config.Entry, error) {
	if input.SourceKind == "child_run" {
		return config.Entry{}, fmt.Errorf("子任务交付必须使用 DeliverChildResult")
	}
	if err := validateContext(input); err != nil {
		return config.Entry{}, err
	}
	var entry config.Entry
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		entry, err = s.appendContext(ctx, tx, runID, input)
		return err
	})
	return entry, err
}
func (s *Session) appendContext(ctx context.Context, tx *store.Tx, runID string, input config.ContextInput) (config.Entry, error) {
	info, _, err := s.activeRun(ctx, tx, runID)
	if err != nil {
		return config.Entry{}, err
	}
	if _, err := tx.FindContextSource(ctx, runID, input.SourceKind, input.SourceID); err == nil {
		return config.Entry{}, config.ErrSessionConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return config.Entry{}, err
	}
	data, err := config.EncodeMessage(input.Message)
	if err != nil {
		return config.Entry{}, err
	}
	seq, err := tx.NextEntrySequence(ctx, s.sessionID)
	if err != nil {
		return config.Entry{}, err
	}
	entry := config.Entry{ID: uuid.NewString(), SessionID: s.sessionID, RunID: runID, ParentID: info.HeadID, Sequence: seq, CreatedAt: time.Now().UTC(), Kind: config.ContextEntry, SourceKind: input.SourceKind, SourceID: input.SourceID, Processing: config.ProcessingOriginal, Message: input.Message}
	if err := tx.InsertEntry(ctx, entry, string(data), nil); err != nil {
		return config.Entry{}, err
	}
	if err := tx.UpdateRunLastEntry(ctx, runID, entry.ID); err != nil {
		return config.Entry{}, err
	}
	if err := tx.UpdateSessionHead(ctx, s.sessionID, entry.ID, entry.CreatedAt); err != nil {
		return config.Entry{}, err
	}
	return entry, nil
}

// CompleteToolProcessing 只确认一次处理。nil 表示保留原文，非 nil（包括空切片）表示替换正文。
func (s *Session) CompleteToolProcessing(ctx context.Context, entryID string, content *[]llm.ContentBlock) error {
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		entry, err := tx.GetEntry(ctx, entryID, s.sessionID)
		if err != nil {
			return err
		}
		if _, ok := entry.Message.(*llm.ToolResultMessage); !ok || entry.Kind != config.MessageEntry {
			return fmt.Errorf("只能处理工具结果")
		}
		if _, _, err := s.activeRun(ctx, tx, entry.RunID); err != nil {
			return err
		}
		return tx.SetToolProjection(ctx, s.sessionID, entryID, content)
	})
}

// GetContext 保持历史顺序及稳定 EntryID。Pending 项不携带模型可见正文，调用方必须处理该状态。
// 不过滤思考块、不配对工具消息、不执行 Hooks。
func (s *Session) GetContext(ctx context.Context) ([]config.ContextRecord, error) {
	entries, err := s.GetCurrentHistory(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]config.ContextRecord, 0, len(entries))
	for _, entry := range entries {
		item := config.ContextRecord{EntryID: entry.ID, Kind: entry.Kind, SourceKind: entry.SourceKind, SourceID: entry.SourceID, Processing: entry.Processing, Origin: entry.Origin}
		switch entry.Processing {
		case config.ProcessingPending:
		case config.ProcessingOriginal:
			item.Message = entry.Message
		case config.ProcessingReplaced:
			raw, ok := entry.Message.(*llm.ToolResultMessage)
			if !ok {
				return nil, fmt.Errorf("替换记录不是工具结果")
			}
			copy := *raw
			copy.Content = entry.VisibleContent
			item.Message = &copy
		default:
			return nil, fmt.Errorf("未知处理状态 %q", entry.Processing)
		}
		out = append(out, item)
	}
	return out, nil
}
