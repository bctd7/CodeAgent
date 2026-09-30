package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// GetCurrentHistory 读取当前 Head 所在分支的完整祖先链 Entries。
// 使用递归查询只读取并解码当前分支从根节点到 Head 的记录。
func (s *Session) GetCurrentHistory(ctx context.Context) ([]Entry, error) {
	var headID sql.NullString
	err := s.store.db.QueryRowContext(ctx, "SELECT head_id FROM sessions WHERE id = ?", s.sessionID).Scan(&headID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
		}
		return nil, fmt.Errorf("查询会话 Head 失败: %w", err)
	}

	if !headID.Valid || headID.String == "" {
		return []Entry{}, nil
	}

	const query = `
WITH RECURSIVE ancestor_chain(id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id) AS (
    SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id
    FROM entries
    WHERE id = ? AND session_id = ?
    UNION
    SELECT e.id, e.session_id, e.parent_id, e.sequence, e.run_id, e.created_at, e.message_json, e.origin_json, e.assistant_entry_id, e.tool_call_id
    FROM entries e
    JOIN ancestor_chain a ON e.id = a.parent_id
    WHERE e.session_id = ?
)
SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id
FROM ancestor_chain;
`

	rows, err := s.store.db.QueryContext(ctx, query, headID.String, s.sessionID, s.sessionID)
	if err != nil {
		return nil, fmt.Errorf("查询历史链记录失败: %w", err)
	}
	defer rows.Close()

	entries := make(map[string]Entry)

	for rows.Next() {
		var (
			ent          Entry
			parentID     sql.NullString
			createdAtRaw string
			msgJSON      string
			originJSON   sql.NullString
			asstEntryID  sql.NullString
			tcID         sql.NullString
		)

		if err := rows.Scan(
			&ent.ID,
			&ent.SessionID,
			&parentID,
			&ent.Sequence,
			&ent.RunID,
			&createdAtRaw,
			&msgJSON,
			&originJSON,
			&asstEntryID,
			&tcID,
		); err != nil {
			return nil, fmt.Errorf("读取历史链行数据失败: %w", err)
		}

		t, err := time.Parse(time.RFC3339Nano, createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析 Entry 时间失败: %w", err)
		}
		ent.CreatedAt = t

		if parentID.Valid {
			ent.ParentID = parentID.String
		}
		if asstEntryID.Valid {
			ent.AssistantEntryID = asstEntryID.String
		}
		if tcID.Valid {
			ent.ToolCallID = tcID.String
		}

		msg, err := decodeMessage([]byte(msgJSON))
		if err != nil {
			return nil, fmt.Errorf("解码当前分支历史消息失败: %w", err)
		}
		ent.Message = msg

		if originJSON.Valid && originJSON.String != "" {
			var origin MessageModelOrigin
			if err := json.Unmarshal([]byte(originJSON.String), &origin); err != nil {
				return nil, fmt.Errorf("解码 Entry Origin 失败: %w", err)
			}
			ent.Origin = &origin
		}

		entries[ent.ID] = ent
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历历史链记录失败: %w", err)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("历史链中断: 未找到 ID 为 %s 的 Entry", headID.String)
	}

	chain := make([]Entry, 0, len(entries))
	visited := make(map[string]bool, len(entries))
	for id := headID.String; id != ""; {
		if visited[id] {
			return nil, fmt.Errorf("检测到循环历史链: %s", id)
		}
		ent, ok := entries[id]
		if !ok {
			return nil, fmt.Errorf("历史链中断: 未找到 ID 为 %s 的 Entry", id)
		}
		visited[id] = true
		chain = append(chain, ent)
		id = ent.ParentID
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	return chain, nil
}
