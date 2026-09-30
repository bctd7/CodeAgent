package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"CodeAgent/internal/llm"
	"github.com/google/uuid"
)

// RecordToolStart 记录一次工具调用的启动事实。
func (s *Session) RecordToolStart(ctx context.Context, assistantEntryID, toolCallID string) (ToolExecution, error) {
	if assistantEntryID == "" {
		return ToolExecution{}, fmt.Errorf("assistantEntryID 不能为空")
	}
	if toolCallID == "" {
		return ToolExecution{}, fmt.Errorf("toolCallID 不能为空")
	}

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ToolExecution{}, fmt.Errorf("开启 RecordToolStart 事务: %w", err)
	}
	defer tx.Rollback()

	var (
		runID   string
		msgJSON string
	)
	err = tx.QueryRowContext(ctx, "SELECT run_id, message_json FROM entries WHERE id = ? AND session_id = ?", assistantEntryID, s.sessionID).Scan(&runID, &msgJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return ToolExecution{}, fmt.Errorf("未找到对应的 assistant 记录: %w", sql.ErrNoRows)
		}
		return ToolExecution{}, fmt.Errorf("查询 assistant 记录: %w", err)
	}

	msg, err := decodeMessage([]byte(msgJSON))
	if err != nil {
		return ToolExecution{}, fmt.Errorf("解码 assistant 消息: %w", err)
	}

	asstMsg, ok := msg.(*llm.AssistantMessage)
	if !ok {
		return ToolExecution{}, fmt.Errorf("指定的记录 %s 不是 assistant 消息", assistantEntryID)
	}

	foundCount := 0
	for _, b := range asstMsg.Content {
		if b.Type == llm.TypeToolCall && b.ID == toolCallID {
			foundCount++
		}
	}
	if foundCount == 0 {
		return ToolExecution{}, fmt.Errorf("assistant 消息中未找到 ToolCallID %q: %w", toolCallID, ErrToolCallNotFound)
	}
	if foundCount > 1 {
		return ToolExecution{}, fmt.Errorf("assistant 消息中存在重复的 ToolCallID %q", toolCallID)
	}

	var finishedAt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT finished_at FROM runs WHERE id = ?", runID).Scan(&finishedAt)
	if err != nil {
		return ToolExecution{}, fmt.Errorf("查询任务状态: %w", err)
	}
	if finishedAt.Valid {
		return ToolExecution{}, fmt.Errorf("所属任务 %s 已经结束，无法启动工具: %w", runID, ErrRunFinished)
	}

	// 检查该调用是否已保存过结果
	var existResultID string
	err = tx.QueryRowContext(ctx, "SELECT id FROM entries WHERE assistant_entry_id = ? AND tool_call_id = ? LIMIT 1", assistantEntryID, toolCallID).Scan(&existResultID)
	if err == nil {
		return ToolExecution{}, fmt.Errorf("工具调用 (%s, %s) 已经保存过结果，无法记录启动: %w", assistantEntryID, toolCallID, ErrToolResultConflict)
	} else if err != sql.ErrNoRows {
		return ToolExecution{}, fmt.Errorf("检查工具结果存在性: %w", err)
	}

	// 检查是否已存在开始记录
	var dummy int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM tool_executions WHERE assistant_entry_id = ? AND tool_call_id = ?", assistantEntryID, toolCallID).Scan(&dummy)
	if err == nil {
		return ToolExecution{}, fmt.Errorf("工具调用 (%s, %s) 已经存在开始记录: %w", assistantEntryID, toolCallID, ErrToolResultConflict)
	} else if err != sql.ErrNoRows {
		return ToolExecution{}, fmt.Errorf("检查工具调用存在性: %w", err)
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	_, err = tx.ExecContext(ctx, `
INSERT INTO tool_executions (session_id, run_id, assistant_entry_id, tool_call_id, started_at, finished_at, result_entry_id)
VALUES (?, ?, ?, ?, ?, NULL, NULL)
`, s.sessionID, runID, assistantEntryID, toolCallID, nowStr)
	if err != nil {
		return ToolExecution{}, fmt.Errorf("插入工具启动记录: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ToolExecution{}, fmt.Errorf("提交 RecordToolStart 事务: %w", err)
	}

	return ToolExecution{
		SessionID:        s.sessionID,
		RunID:            runID,
		AssistantEntryID: assistantEntryID,
		ToolCallID:       toolCallID,
		StartedAt:        now,
		FinishedAt:       nil,
		ResultEntryID:    "",
	}, nil
}

// AppendToolResult 追加工具执行结果消息。
func (s *Session) AppendToolResult(ctx context.Context, assistantEntryID string, resultMsg *llm.ToolResultMessage) (Entry, error) {
	if assistantEntryID == "" {
		return Entry{}, fmt.Errorf("assistantEntryID 不能为空")
	}
	if resultMsg == nil {
		return Entry{}, fmt.Errorf("ToolResultMessage 不能为空")
	}
	if resultMsg.Role != llm.RoleTool {
		return Entry{}, fmt.Errorf("ToolResultMessage 角色应为 %q，实际为 %q", llm.RoleTool, resultMsg.Role)
	}
	if resultMsg.ToolCallID == "" {
		return Entry{}, fmt.Errorf("ToolResultMessage.ToolCallID 不能为空")
	}

	msgData, err := encodeMessage(resultMsg)
	if err != nil {
		return Entry{}, fmt.Errorf("编码工具结果消息: %w", err)
	}

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("开启 AppendToolResult 事务: %w", err)
	}
	defer tx.Rollback()

	var (
		runID   string
		msgJSON string
	)
	err = tx.QueryRowContext(ctx, "SELECT run_id, message_json FROM entries WHERE id = ? AND session_id = ?", assistantEntryID, s.sessionID).Scan(&runID, &msgJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return Entry{}, fmt.Errorf("未找到对应的 assistant 记录: %w", sql.ErrNoRows)
		}
		return Entry{}, fmt.Errorf("查询 assistant 记录: %w", err)
	}

	msg, err := decodeMessage([]byte(msgJSON))
	if err != nil {
		return Entry{}, fmt.Errorf("解码 assistant 消息: %w", err)
	}

	asstMsg, ok := msg.(*llm.AssistantMessage)
	if !ok {
		return Entry{}, fmt.Errorf("指定的记录 %s 不是 assistant 消息", assistantEntryID)
	}

	foundCount := 0
	for _, b := range asstMsg.Content {
		if b.Type == llm.TypeToolCall && b.ID == resultMsg.ToolCallID {
			foundCount++
		}
	}
	if foundCount == 0 {
		return Entry{}, fmt.Errorf("assistant 消息中未找到 ToolCallID %q: %w", resultMsg.ToolCallID, ErrToolCallNotFound)
	}
	if foundCount > 1 {
		return Entry{}, fmt.Errorf("assistant 消息中存在重复的 ToolCallID %q", resultMsg.ToolCallID)
	}

	var finishedAt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT finished_at FROM runs WHERE id = ?", runID).Scan(&finishedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("查询任务状态: %w", err)
	}
	if finishedAt.Valid {
		return Entry{}, fmt.Errorf("所属任务 %s 已经结束，无法追加工具结果: %w", runID, ErrRunFinished)
	}

	// 检查 entries 是否已存在该工具结果
	var existResultID string
	err = tx.QueryRowContext(ctx, "SELECT id FROM entries WHERE assistant_entry_id = ? AND tool_call_id = ? LIMIT 1", assistantEntryID, resultMsg.ToolCallID).Scan(&existResultID)
	if err == nil {
		return Entry{}, fmt.Errorf("工具调用 (%s, %s) 已经保存过结果，禁止重复保存: %w", assistantEntryID, resultMsg.ToolCallID, ErrToolResultConflict)
	} else if err != sql.ErrNoRows {
		return Entry{}, fmt.Errorf("查询工具结果存在性: %w", err)
	}

	// 检查 tool_executions 记录
	var (
		hasStartRecord bool
		existExecResID sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
SELECT result_entry_id FROM tool_executions WHERE assistant_entry_id = ? AND tool_call_id = ?
`, assistantEntryID, resultMsg.ToolCallID).Scan(&existExecResID)
	if err == nil {
		hasStartRecord = true
		if existExecResID.Valid && existExecResID.String != "" {
			return Entry{}, fmt.Errorf("工具调用 (%s, %s) 已经保存过结果，禁止重复保存: %w", assistantEntryID, resultMsg.ToolCallID, ErrToolResultConflict)
		}
	} else if err == sql.ErrNoRows {
		hasStartRecord = false
	} else {
		return Entry{}, fmt.Errorf("查询工具执行记录: %w", err)
	}

	var currHeadID sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT head_id FROM sessions WHERE id = ?", s.sessionID).Scan(&currHeadID)
	if err != nil {
		return Entry{}, fmt.Errorf("获取会话 Head: %w", err)
	}

	var nextSeq int64
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM entries WHERE session_id = ?", s.sessionID).Scan(&nextSeq)
	if err != nil {
		return Entry{}, fmt.Errorf("获取下一 Sequence: %w", err)
	}

	entryID := uuid.NewString()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	var parentVal any
	if currHeadID.Valid && currHeadID.String != "" {
		parentVal = currHeadID.String
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO entries (id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
`, entryID, s.sessionID, parentVal, nextSeq, runID, nowStr, string(msgData), assistantEntryID, resultMsg.ToolCallID)
	if err != nil {
		return Entry{}, fmt.Errorf("写入工具结果 Entry: %w", err)
	}

	if hasStartRecord {
		_, err = tx.ExecContext(ctx, `
UPDATE tool_executions
SET finished_at = ?, result_entry_id = ?
WHERE assistant_entry_id = ? AND tool_call_id = ?
`, nowStr, entryID, assistantEntryID, resultMsg.ToolCallID)
		if err != nil {
			return Entry{}, fmt.Errorf("更新工具执行完成记录: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, "UPDATE runs SET last_entry_id = ? WHERE id = ?", entryID, runID)
	if err != nil {
		return Entry{}, fmt.Errorf("更新 Run.LastEntryID: %w", err)
	}

	_, err = tx.ExecContext(ctx, "UPDATE sessions SET head_id = ?, updated_at = ? WHERE id = ?", entryID, nowStr, s.sessionID)
	if err != nil {
		return Entry{}, fmt.Errorf("更新会话 HeadID: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Entry{}, fmt.Errorf("提交 AppendToolResult 事务: %w", err)
	}

	var parentStr string
	if currHeadID.Valid {
		parentStr = currHeadID.String
	}

	return Entry{
		ID:               entryID,
		SessionID:        s.sessionID,
		ParentID:         parentStr,
		Sequence:         nextSeq,
		RunID:            runID,
		CreatedAt:        now,
		Message:          resultMsg,
		Origin:           nil,
		AssistantEntryID: assistantEntryID,
		ToolCallID:       resultMsg.ToolCallID,
	}, nil
}
