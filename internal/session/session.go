package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"CodeAgent/internal/llm"
	"github.com/google/uuid"
)

// Session 绑定特定会话 ID，提供该会话的 Run、消息、工具执行和分支操作。
type Session struct {
	store     *Store
	sessionID string
}

// SessionID 返回当前会话绑定的 ID。
func (s *Session) SessionID() string {
	return s.sessionID
}

// StartRun 在当前会话开启一次新的任务。
func (s *Session) StartRun(ctx context.Context, runID string, userMsg *llm.UserMessage) (RunRecord, error) {
	if runID == "" {
		return RunRecord{}, fmt.Errorf("RunID 不能为空")
	}
	if userMsg == nil {
		return RunRecord{}, fmt.Errorf("UserMessage 不能为空")
	}
	if userMsg.Role != llm.RoleUser {
		return RunRecord{}, fmt.Errorf("UserMessage 角色应为 %q，实际为 %q", llm.RoleUser, userMsg.Role)
	}

	msgData, err := encodeMessage(userMsg)
	if err != nil {
		return RunRecord{}, fmt.Errorf("编码用户消息: %w", err)
	}

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return RunRecord{}, fmt.Errorf("开启 StartRun 事务: %w", err)
	}
	defer tx.Rollback()

	var (
		archivedAt sql.NullString
		currHeadID sql.NullString
	)
	err = tx.QueryRowContext(ctx, "SELECT archived_at, head_id FROM sessions WHERE id = ?", s.sessionID).Scan(&archivedAt, &currHeadID)
	if err != nil {
		if err == sql.ErrNoRows {
			return RunRecord{}, fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
		}
		return RunRecord{}, fmt.Errorf("查询会话状态: %w", err)
	}
	if archivedAt.Valid {
		return RunRecord{}, fmt.Errorf("会话 %s 已归档，拒绝开始新任务: %w", s.sessionID, ErrSessionArchived)
	}

	var activeRunID string
	err = tx.QueryRowContext(ctx, "SELECT id FROM runs WHERE session_id = ? AND finished_at IS NULL LIMIT 1", s.sessionID).Scan(&activeRunID)
	if err == nil {
		return RunRecord{}, fmt.Errorf("会话 %s 已存在未结束的任务 %s，禁止并发启动新任务: %w", s.sessionID, activeRunID, ErrRunActive)
	} else if err != sql.ErrNoRows {
		return RunRecord{}, fmt.Errorf("检查未结束任务: %w", err)
	}

	inputEntryID := uuid.NewString()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	var nextSeq int64
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM entries WHERE session_id = ?", s.sessionID).Scan(&nextSeq)
	if err != nil {
		return RunRecord{}, fmt.Errorf("计算会话 Sequence: %w", err)
	}

	var startHeadVal any
	var parentVal any
	if currHeadID.Valid && currHeadID.String != "" {
		startHeadVal = currHeadID.String
		parentVal = currHeadID.String
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO runs (
    id, session_id, start_head_id, input_entry_id, last_entry_id,
    started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
) VALUES (?, ?, ?, ?, NULL, ?, NULL, ?, 0, '', '', '')
`, runID, s.sessionID, startHeadVal, inputEntryID, nowStr, string(RunStatePending))
	if err != nil {
		return RunRecord{}, fmt.Errorf("创建 Run 记录: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO entries (id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL)
`, inputEntryID, s.sessionID, parentVal, nextSeq, runID, nowStr, string(msgData))
	if err != nil {
		return RunRecord{}, fmt.Errorf("创建输入 Entry 记录: %w", err)
	}

	_, err = tx.ExecContext(ctx, "UPDATE runs SET last_entry_id = ? WHERE id = ?", inputEntryID, runID)
	if err != nil {
		return RunRecord{}, fmt.Errorf("设置 Run.LastEntryID: %w", err)
	}

	_, err = tx.ExecContext(ctx, "UPDATE sessions SET head_id = ?, updated_at = ? WHERE id = ?", inputEntryID, nowStr, s.sessionID)
	if err != nil {
		return RunRecord{}, fmt.Errorf("更新会话 HeadID: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return RunRecord{}, fmt.Errorf("提交 StartRun 事务: %w", err)
	}

	var startHeadStr string
	if currHeadID.Valid {
		startHeadStr = currHeadID.String
	}

	return RunRecord{
		ID:           runID,
		SessionID:    s.sessionID,
		StartHeadID:  startHeadStr,
		InputEntryID: inputEntryID,
		LastEntryID:  inputEntryID,
		StartedAt:    now,
		FinishedAt:   nil,
		State:        RunStatePending,
		Steps:        0,
	}, nil
}

// RecordStep 原子增加当前 Run 的模型调用次数并返回新计数值。
func (s *Session) RecordStep(ctx context.Context, runID string) (int, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启 RecordStep 事务: %w", err)
	}
	defer tx.Rollback()

	var (
		steps      int
		finishedAt sql.NullString
	)
	err = tx.QueryRowContext(ctx, "SELECT steps, finished_at FROM runs WHERE id = ? AND session_id = ?", runID, s.sessionID).Scan(&steps, &finishedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("查询任务失败: %w", sql.ErrNoRows)
		}
		return 0, fmt.Errorf("查询任务状态: %w", err)
	}
	if finishedAt.Valid {
		return 0, fmt.Errorf("任务 %s 已经结束，无法增加步骤: %w", runID, ErrRunFinished)
	}

	steps++
	_, err = tx.ExecContext(ctx, "UPDATE runs SET steps = ? WHERE id = ?", steps, runID)
	if err != nil {
		return 0, fmt.Errorf("更新任务步骤: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交 RecordStep 事务: %w", err)
	}
	return steps, nil
}

// AppendMessage 向当前未结束的任务追加一条普通消息（用户或 Assistant 消息）。
func (s *Session) AppendMessage(ctx context.Context, runID string, msg llm.Message, origin *MessageModelOrigin) (Entry, error) {
	if msg == nil {
		return Entry{}, fmt.Errorf("消息不能为空")
	}

	switch m := msg.(type) {
	case *llm.ToolResultMessage:
		return Entry{}, fmt.Errorf("普通消息追加入口不接受工具结果，必须使用 AppendToolResult")
	case *llm.UserMessage:
		if m == nil {
			return Entry{}, fmt.Errorf("UserMessage 不能为空")
		}
		if m.Role != llm.RoleUser {
			return Entry{}, fmt.Errorf("UserMessage 角色应为 %q，实际为 %q", llm.RoleUser, m.Role)
		}
		if origin != nil {
			return Entry{}, fmt.Errorf("用户消息不应提供 Origin 来源")
		}
	case *llm.AssistantMessage:
		if m == nil {
			return Entry{}, fmt.Errorf("AssistantMessage 不能为空")
		}
		if m.Role != llm.RoleAssistant {
			return Entry{}, fmt.Errorf("AssistantMessage 角色应为 %q，实际为 %q", llm.RoleAssistant, m.Role)
		}
		if origin == nil {
			return Entry{}, fmt.Errorf("assistant 消息必须提供实际请求 Origin")
		}
	default:
		return Entry{}, fmt.Errorf("不支持的消息类型: %T", msg)
	}

	msgData, err := encodeMessage(msg)
	if err != nil {
		return Entry{}, fmt.Errorf("编码消息失败: %w", err)
	}

	var originData any
	if origin != nil {
		ob, err := json.Marshal(origin)
		if err != nil {
			return Entry{}, fmt.Errorf("序列化 Origin 失败: %w", err)
		}
		originData = string(ob)
	}

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("开启 AppendMessage 事务: %w", err)
	}
	defer tx.Rollback()

	var finishedAt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT finished_at FROM runs WHERE id = ? AND session_id = ?", runID, s.sessionID).Scan(&finishedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Entry{}, fmt.Errorf("任务不存在: %w", sql.ErrNoRows)
		}
		return Entry{}, fmt.Errorf("查询任务状态: %w", err)
	}
	if finishedAt.Valid {
		return Entry{}, fmt.Errorf("无法向已结束的任务 %s 追加消息: %w", runID, ErrRunFinished)
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
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
`, entryID, s.sessionID, parentVal, nextSeq, runID, nowStr, string(msgData), originData)
	if err != nil {
		return Entry{}, fmt.Errorf("写入 Entry: %w", err)
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
		return Entry{}, fmt.Errorf("提交 AppendMessage 事务: %w", err)
	}

	var parentStr string
	if currHeadID.Valid {
		parentStr = currHeadID.String
	}

	return Entry{
		ID:        entryID,
		SessionID: s.sessionID,
		ParentID:  parentStr,
		Sequence:  nextSeq,
		RunID:     runID,
		CreatedAt: now,
		Message:   msg,
		Origin:    origin,
	}, nil
}

// FinishRun 结束指定的任务。状态必须为终态；已结束的 Run 重复结束返回冲突错误。
func (s *Session) FinishRun(ctx context.Context, runID string, completion RunCompletion) (RunRecord, error) {
	switch completion.State {
	case RunStateSuccess:
		if completion.StopReason != llm.StopStop && completion.StopReason != llm.StopLength && completion.StopReason != llm.StopToolUse {
			return RunRecord{}, fmt.Errorf("成功任务的结束原因无效: %q", completion.StopReason)
		}
	case RunStateError:
		if completion.StopReason != "" && completion.StopReason != llm.StopSafety {
			return RunRecord{}, fmt.Errorf("失败任务的结束原因无效: %q", completion.StopReason)
		}
	case RunStateAborted:
		if completion.StopReason != "" {
			return RunRecord{}, fmt.Errorf("取消任务不能带结束原因: %q", completion.StopReason)
		}
	default:
		return RunRecord{}, fmt.Errorf("结束任务的状态必须为终态，非法值: %q", completion.State)
	}

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return RunRecord{}, fmt.Errorf("开启 FinishRun 事务: %w", err)
	}
	defer tx.Rollback()

	var (
		rec           RunRecord
		startHeadID   sql.NullString
		lastEntryID   sql.NullString
		startedAtRaw  string
		finishedAtRaw sql.NullString
	)

	err = tx.QueryRowContext(ctx, `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, steps
FROM runs
WHERE id = ? AND session_id = ?
`, runID, s.sessionID).Scan(
		&rec.ID,
		&rec.SessionID,
		&startHeadID,
		&rec.InputEntryID,
		&lastEntryID,
		&startedAtRaw,
		&finishedAtRaw,
		&rec.Steps,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return RunRecord{}, fmt.Errorf("任务不存在: %w", sql.ErrNoRows)
		}
		return RunRecord{}, fmt.Errorf("查询任务信息: %w", err)
	}

	if finishedAtRaw.Valid {
		return RunRecord{}, fmt.Errorf("任务 %s 已经处于结束状态，不能重复结束: %w", runID, ErrRunFinished)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, startedAtRaw)
	if err != nil {
		return RunRecord{}, fmt.Errorf("解析任务开始时间: %w", err)
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	_, err = tx.ExecContext(ctx, `
UPDATE runs
SET finished_at = ?, state = ?, stop_reason = ?, raw_stop_reason = ?, error_message = ?
WHERE id = ?
`, nowStr, string(completion.State), string(completion.StopReason), completion.RawStopReason, completion.ErrorMessage, runID)
	if err != nil {
		return RunRecord{}, fmt.Errorf("更新任务终态: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return RunRecord{}, fmt.Errorf("提交 FinishRun 事务: %w", err)
	}

	rec.StartedAt = startedAt
	rec.FinishedAt = &now
	rec.State = completion.State
	rec.StopReason = completion.StopReason
	rec.RawStopReason = completion.RawStopReason
	rec.ErrorMessage = completion.ErrorMessage
	if startHeadID.Valid {
		rec.StartHeadID = startHeadID.String
	}
	if lastEntryID.Valid {
		rec.LastEntryID = lastEntryID.String
	}

	return rec, nil
}

// GetRun 读取指定任务的完整详情，在同一只读事务中取得一致的数据。
func (s *Session) GetRun(ctx context.Context, runID string) (RunDetail, error) {
	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RunDetail{}, fmt.Errorf("开启读取事务: %w", err)
	}
	defer tx.Rollback()

	var (
		rec           RunRecord
		startHeadID   sql.NullString
		lastEntryID   sql.NullString
		startedAtRaw  string
		finishedAtRaw sql.NullString
	)

	err = tx.QueryRowContext(ctx, `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
FROM runs
WHERE id = ? AND session_id = ?
`, runID, s.sessionID).Scan(
		&rec.ID,
		&rec.SessionID,
		&startHeadID,
		&rec.InputEntryID,
		&lastEntryID,
		&startedAtRaw,
		&finishedAtRaw,
		&rec.State,
		&rec.Steps,
		&rec.StopReason,
		&rec.RawStopReason,
		&rec.ErrorMessage,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return RunDetail{}, fmt.Errorf("任务不存在: %w", sql.ErrNoRows)
		}
		return RunDetail{}, fmt.Errorf("读取 Run 失败: %w", err)
	}

	startedAt, err := time.Parse(time.RFC3339Nano, startedAtRaw)
	if err != nil {
		return RunDetail{}, fmt.Errorf("解析任务开始时间: %w", err)
	}
	rec.StartedAt = startedAt

	if finishedAtRaw.Valid {
		t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
		if err != nil {
			return RunDetail{}, fmt.Errorf("解析任务结束时间: %w", err)
		}
		rec.FinishedAt = &t
	}
	if startHeadID.Valid {
		rec.StartHeadID = startHeadID.String
	}
	if lastEntryID.Valid {
		rec.LastEntryID = lastEntryID.String
	}

	entryRows, err := tx.QueryContext(ctx, `
SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id
FROM entries
WHERE run_id = ? AND session_id = ?
ORDER BY sequence ASC
`, runID, s.sessionID)
	if err != nil {
		return RunDetail{}, fmt.Errorf("读取任务 Entries: %w", err)
	}

	var entries []Entry
	usageByOriginMap := make(map[MessageModelOrigin]*ModelUsage)
	var totalUsage llm.Usage

	for entryRows.Next() {
		var (
			ent          Entry
			parentID     sql.NullString
			createdAtRaw string
			msgJSON      string
			originJSON   sql.NullString
			asstEntryID  sql.NullString
			tcID         sql.NullString
		)

		if err := entryRows.Scan(
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
			entryRows.Close()
			return RunDetail{}, fmt.Errorf("读取 Entry 行失败: %w", err)
		}

		t, err := time.Parse(time.RFC3339Nano, createdAtRaw)
		if err != nil {
			entryRows.Close()
			return RunDetail{}, fmt.Errorf("解析 Entry 创建时间: %w", err)
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
			entryRows.Close()
			return RunDetail{}, fmt.Errorf("解码 Entry 消息: %w", err)
		}
		ent.Message = msg

		if originJSON.Valid && originJSON.String != "" {
			var origin MessageModelOrigin
			if err := json.Unmarshal([]byte(originJSON.String), &origin); err != nil {
				entryRows.Close()
				return RunDetail{}, fmt.Errorf("解码 Entry Origin: %w", err)
			}
			ent.Origin = &origin

			if asstMsg, ok := msg.(*llm.AssistantMessage); ok {
				u := asstMsg.Usage
				item, exists := usageByOriginMap[origin]
				if !exists {
					item = &ModelUsage{
						Origin: origin,
						Usage:  llm.Usage{},
					}
					usageByOriginMap[origin] = item
				}
				item.Usage.Input += u.Input
				item.Usage.Output += u.Output
				item.Usage.CacheRead += u.CacheRead
				item.Usage.CacheWrite += u.CacheWrite
				item.Usage.Reasoning += u.Reasoning
				item.Usage.Total += u.Total
				item.Usage.Cost.Input += u.Cost.Input
				item.Usage.Cost.Output += u.Cost.Output
				item.Usage.Cost.CacheRead += u.Cost.CacheRead
				item.Usage.Cost.CacheWrite += u.Cost.CacheWrite
				item.Usage.Cost.Total += u.Cost.Total

				totalUsage.Input += u.Input
				totalUsage.Output += u.Output
				totalUsage.CacheRead += u.CacheRead
				totalUsage.CacheWrite += u.CacheWrite
				totalUsage.Reasoning += u.Reasoning
				totalUsage.Total += u.Total
				totalUsage.Cost.Input += u.Cost.Input
				totalUsage.Cost.Output += u.Cost.Output
				totalUsage.Cost.CacheRead += u.Cost.CacheRead
				totalUsage.Cost.CacheWrite += u.Cost.CacheWrite
				totalUsage.Cost.Total += u.Cost.Total
			}
		}

		entries = append(entries, ent)
	}
	if err := entryRows.Err(); err != nil {
		entryRows.Close()
		return RunDetail{}, fmt.Errorf("遍历 Entry 失败: %w", err)
	}
	entryRows.Close()

	toolRows, err := tx.QueryContext(ctx, `
SELECT session_id, run_id, assistant_entry_id, tool_call_id, started_at, finished_at, result_entry_id
FROM tool_executions
WHERE run_id = ? AND session_id = ?
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', started_at) ASC, assistant_entry_id ASC, tool_call_id ASC
`, runID, s.sessionID)
	if err != nil {
		return RunDetail{}, fmt.Errorf("读取工具执行记录: %w", err)
	}

	var toolExecs []ToolExecution
	for toolRows.Next() {
		var (
			te            ToolExecution
			startedAtRaw  string
			finishedAtRaw sql.NullString
			resultEntryID sql.NullString
		)

		if err := toolRows.Scan(
			&te.SessionID,
			&te.RunID,
			&te.AssistantEntryID,
			&te.ToolCallID,
			&startedAtRaw,
			&finishedAtRaw,
			&resultEntryID,
		); err != nil {
			toolRows.Close()
			return RunDetail{}, fmt.Errorf("读取工具执行行: %w", err)
		}

		startedAt, err := time.Parse(time.RFC3339Nano, startedAtRaw)
		if err != nil {
			toolRows.Close()
			return RunDetail{}, fmt.Errorf("解析工具启动时间: %w", err)
		}
		te.StartedAt = startedAt

		if finishedAtRaw.Valid {
			t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
			if err != nil {
				toolRows.Close()
				return RunDetail{}, fmt.Errorf("解析工具完成时间: %w", err)
			}
			te.FinishedAt = &t
		}
		if resultEntryID.Valid {
			te.ResultEntryID = resultEntryID.String
		}

		toolExecs = append(toolExecs, te)
	}
	if err := toolRows.Err(); err != nil {
		toolRows.Close()
		return RunDetail{}, fmt.Errorf("遍历工具执行记录失败: %w", err)
	}
	toolRows.Close()

	var usageByOrigin []ModelUsage
	for _, item := range usageByOriginMap {
		usageByOrigin = append(usageByOrigin, *item)
	}
	sort.Slice(usageByOrigin, func(i, j int) bool {
		a, b := usageByOrigin[i].Origin, usageByOrigin[j].Origin
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.API != b.API {
			return a.API < b.API
		}
		if a.ModelID != b.ModelID {
			return a.ModelID < b.ModelID
		}
		return a.BaseURL < b.BaseURL
	})

	if entries == nil {
		entries = []Entry{}
	}
	if toolExecs == nil {
		toolExecs = []ToolExecution{}
	}
	if usageByOrigin == nil {
		usageByOrigin = []ModelUsage{}
	}

	return RunDetail{
		Record:         rec,
		Entries:        entries,
		ToolExecutions: toolExecs,
		UsageByOrigin:  usageByOrigin,
		TotalUsage:     totalUsage,
	}, nil
}

// ListRuns 按输入 Entry 的 Sequence 倒序返回本会话的所有 RunRecord。
func (s *Session) ListRuns(ctx context.Context, limit, offset int) ([]RunRecord, error) {
	if limit < 0 {
		return nil, fmt.Errorf("limit 不能为负数: %d", limit)
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset 不能为负数: %d", offset)
	}

	if limit == 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}

	rows, err := s.store.db.QueryContext(ctx, `
SELECT r.id, r.session_id, r.start_head_id, r.input_entry_id, r.last_entry_id,
       r.started_at, r.finished_at, r.state, r.steps, r.stop_reason, r.raw_stop_reason, r.error_message
FROM runs r
JOIN entries e ON r.input_entry_id = e.id
WHERE r.session_id = ?
ORDER BY e.sequence DESC, r.id ASC
LIMIT ? OFFSET ?
`, s.sessionID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询会话任务列表: %w", err)
	}
	defer rows.Close()

	var records []RunRecord
	for rows.Next() {
		var (
			rec           RunRecord
			startHeadID   sql.NullString
			lastEntryID   sql.NullString
			startedAtRaw  string
			finishedAtRaw sql.NullString
		)

		if err := rows.Scan(
			&rec.ID,
			&rec.SessionID,
			&startHeadID,
			&rec.InputEntryID,
			&lastEntryID,
			&startedAtRaw,
			&finishedAtRaw,
			&rec.State,
			&rec.Steps,
			&rec.StopReason,
			&rec.RawStopReason,
			&rec.ErrorMessage,
		); err != nil {
			return nil, fmt.Errorf("读取 Run 行数据: %w", err)
		}

		startedAt, err := time.Parse(time.RFC3339Nano, startedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析任务开始时间: %w", err)
		}
		rec.StartedAt = startedAt

		if finishedAtRaw.Valid {
			t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
			if err != nil {
				return nil, fmt.Errorf("解析任务结束时间: %w", err)
			}
			rec.FinishedAt = &t
		}
		if startHeadID.Valid {
			rec.StartHeadID = startHeadID.String
		}
		if lastEntryID.Valid {
			rec.LastEntryID = lastEntryID.String
		}

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话任务列表失败: %w", err)
	}

	if records == nil {
		records = []RunRecord{}
	}
	return records, nil
}

// SwitchBranch 切换会话的历史分支至指定已完成 Run 的末尾。
func (s *Session) SwitchBranch(ctx context.Context, runID string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启切换分支事务: %w", err)
	}
	defer tx.Rollback()

	var activeRunID string
	err = tx.QueryRowContext(ctx, "SELECT id FROM runs WHERE session_id = ? AND finished_at IS NULL LIMIT 1", s.sessionID).Scan(&activeRunID)
	if err == nil {
		return fmt.Errorf("当前会话存在未结束任务 %s，无法切换分支: %w", activeRunID, ErrRunActive)
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("检查未结束任务: %w", err)
	}

	var (
		lastEntryID sql.NullString
		finishedAt  sql.NullString
	)
	err = tx.QueryRowContext(ctx, "SELECT last_entry_id, finished_at FROM runs WHERE id = ? AND session_id = ?", runID, s.sessionID).Scan(&lastEntryID, &finishedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("目标任务不存在: %w", sql.ErrNoRows)
		}
		return fmt.Errorf("查询目标任务: %w", err)
	}

	if !finishedAt.Valid {
		return fmt.Errorf("目标任务 %s 尚未结束，无法切换分支: %w", runID, ErrRunActive)
	}
	if !lastEntryID.Valid || lastEntryID.String == "" {
		return fmt.Errorf("目标任务 %s 没有有效的记录", runID)
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET head_id = ?, updated_at = ? WHERE id = ?", lastEntryID.String, nowStr, s.sessionID)
	if err != nil {
		return fmt.Errorf("更新会话 HeadID: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交切换分支事务: %w", err)
	}
	return nil
}
