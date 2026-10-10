package session

import (
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"context"
	"database/sql"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"
)

func compactionShape(input config.CompactionInput) error {
	if input.HeadID == "" || strings.TrimSpace(input.Summary) == "" || input.FirstKeptIndex < 1 || input.FirstKeptIndex > len(input.SourceIDs) {
		return fmt.Errorf("无效压缩摘要或边界")
	}
	if input.Origin.ModelID == "" || input.Origin.Provider == "" {
		return fmt.Errorf("压缩必须记录模型来源")
	}
	u := input.Usage
	for _, n := range []int64{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning, u.Total} {
		if n < 0 {
			return fmt.Errorf("压缩用量不能为负")
		}
	}
	for _, n := range []float64{u.Cost.Input, u.Cost.Output, u.Cost.CacheRead, u.Cost.CacheWrite, u.Cost.Total} {
		if n < 0 {
			return fmt.Errorf("压缩费用不能为负")
		}
	}
	return nil
}

// SaveCompaction 保存已生成摘要和来源引用，不调用 compact 或模型。
func (s *Session) SaveCompaction(ctx context.Context, input config.CompactionInput) (config.Compaction, error) {
	if err := compactionShape(input); err != nil {
		return config.Compaction{}, err
	}
	var result config.Compaction
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			return err
		}
		if info.ArchivedAt != nil {
			return config.ErrSessionArchived
		}
		path, err := s.historyAt(ctx, tx, input.HeadID)
		if err != nil {
			return err
		}
		positions := map[string]int{}
		for i, e := range path {
			positions[e.ID] = i
		}
		covered := -1
		if input.PreviousID != "" {
			previous, err := tx.GetCompaction(ctx, input.PreviousID, s.sessionID)
			if err != nil {
				return err
			}
			if err := compactionShape(previous.CompactionInput); err != nil {
				return err
			}
			if _, ok := positions[previous.HeadID]; !ok {
				return fmt.Errorf("上次摘要不在当前历史路径")
			}
			var ok bool
			covered, ok = positions[previous.SourceIDs[previous.FirstKeptIndex-1]]
			if !ok {
				return config.ErrSessionConflict
			}
		}
		last := covered
		for _, id := range input.SourceIDs {
			index, ok := positions[id]
			if !ok || index <= last {
				return fmt.Errorf("压缩来源必须按路径顺序唯一排列，且不重复覆盖前缀")
			}
			if path[index].Processing == config.ProcessingPending {
				return fmt.Errorf("压缩来源尚未完成处理")
			}
			last = index
		}
		result = config.Compaction{ID: uuid.NewString(), SessionID: s.sessionID, CompactionInput: input, CreatedAt: time.Now().UTC()}
		return tx.InsertCompaction(ctx, result)
	})
	if err != nil {
		return config.Compaction{}, err
	}
	return result, nil
}

// LatestCompaction 只选择生成时 Head 为当前 Head 祖先的摘要；没有适用记录返回 sql.ErrNoRows。
func (s *Session) LatestCompaction(ctx context.Context) (config.Compaction, error) {
	var result config.Compaction
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			return err
		}
		path, err := s.historyAt(ctx, tx, info.HeadID)
		if err != nil {
			return err
		}
		positions := map[string]int{}
		for i, e := range path {
			positions[e.ID] = i
		}
		values, err := tx.ListCompactions(ctx, s.sessionID)
		if err != nil {
			return err
		}
		best := -1
		for _, value := range values {
			if _, ok := positions[value.HeadID]; !ok {
				continue
			}
			if err := compactionShape(value.CompactionInput); err != nil {
				return err
			}
			pos, ok := positions[value.SourceIDs[value.FirstKeptIndex-1]]
			if !ok {
				return config.ErrSessionConflict
			}
			if pos > best {
				best = pos
				result = value
			}
		}
		if best < 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	return result, err
}
