package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
)

// GetCurrentHistory 读取当前 Head 所在分支的完整祖先链 Entries。
// 使用递归查询只读取并解码当前分支从根节点到 Head 的记录，严格校验断链与循环。
func (s *Session) GetCurrentHistory(ctx context.Context) ([]config.Entry, error) {
	var chain []config.Entry
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话 Head 失败: %w", err)
		}

		if sess.HeadID == "" {
			chain = []config.Entry{}
			return nil
		}

		entries, err := tx.ListEntryAncestors(ctx, sess.HeadID, s.sessionID)
		if err != nil {
			return err
		}

		if len(entries) == 0 {
			return fmt.Errorf("历史链中断: 未找到 ID 为 %s 的 config.Entry", sess.HeadID)
		}

		nodes := make([]config.Entry, 0, len(entries))
		visited := make(map[string]bool, len(entries))
		for id := sess.HeadID; id != ""; {
			if visited[id] {
				return fmt.Errorf("检测到循环历史链: %s", id)
			}
			ent, ok := entries[id]
			if !ok {
				return fmt.Errorf("历史链中断: 未找到 ID 为 %s 的 config.Entry", id)
			}
			visited[id] = true
			nodes = append(nodes, ent)
			id = ent.ParentID
		}

		// 反转为从根节点到当前 Head 的正序
		for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
			nodes[i], nodes[j] = nodes[j], nodes[i]
		}

		chain = nodes
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chain, nil
}
