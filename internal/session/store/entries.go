package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"CodeAgent/internal/session/config"
	sessql "CodeAgent/internal/session/sql"
)

// Entry 保存消息；工具结果 Entry 通过 AssistantEntryID 与 ToolCallID 关联调用。
// ToolExecution 保存启动与完成记录；ResultEntryID 指向对应工具结果 Entry。

// InsertEntry 插入一条消息记录，编码和业务校验由调用方完成。
func (tx *Tx) InsertEntry(ctx context.Context, ent config.Entry, msgJSON string, originJSON any) error {
	var parentVal any
	if ent.ParentID != "" {
		parentVal = ent.ParentID
	}

	var asstVal any
	if ent.AssistantEntryID != "" {
		asstVal = ent.AssistantEntryID
	}

	var tcVal any
	if ent.ToolCallID != "" {
		tcVal = ent.ToolCallID
	}

	if ent.Kind == "" {
		ent.Kind = config.MessageEntry
	}
	if ent.Processing == "" {
		ent.Processing = config.ProcessingOriginal
	}
	var visible any
	if ent.Processing == config.ProcessingReplaced {
		data, err := json.Marshal(ent.VisibleContent)
		if err != nil {
			return err
		}
		visible = string(data)
	}
	createdAtStr := ent.CreatedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.tx.ExecContext(
		ctx,
		sessql.InsertEntrySQL,
		ent.ID,
		ent.SessionID,
		parentVal,
		ent.Sequence,
		ent.RunID,
		createdAtStr,
		msgJSON,
		originJSON,
		asstVal,
		tcVal,
		ent.Kind, nullText(ent.SourceKind), nullText(ent.SourceID), ent.Processing, visible,
	)
	if err != nil {
		return fmt.Errorf("写入 Entry: %w", err)
	}
	return nil
}

// NextEntrySequence 查询会话的下一消息序号，须与插入在同一事务内使用。
func (tx *Tx) NextEntrySequence(ctx context.Context, sessionID string) (int64, error) {
	var nextSeq int64
	err := tx.tx.QueryRowContext(ctx, sessql.SelectMaxSequenceSQL, sessionID).Scan(&nextSeq)
	if err != nil {
		return 0, fmt.Errorf("计算会话 Sequence: %w", err)
	}
	return nextSeq, nil
}

// GetEntry 读取本会话的一条完整记录。
func (tx *Tx) GetEntry(ctx context.Context, id, sessionID string) (config.Entry, error) {
	return scanEntry(tx.tx.QueryRowContext(ctx, sessql.SelectEntryByIDSQL, id, sessionID))
}

// ListEntriesByRun 按写入顺序读取一个 Run 的记录。
func (tx *Tx) ListEntriesByRun(ctx context.Context, runID, sessionID string) ([]config.Entry, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.SelectEntriesByRunIDSQL, runID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查询 Run 消息: %w", err)
	}
	defer rows.Close()
	entries := []config.Entry{}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ListEntryAncestors 查询 Head 及其祖先，按 Entry ID 返回；业务层检查链路并排序。
func (tx *Tx) ListEntryAncestors(ctx context.Context, headID, sessionID string) (map[string]config.Entry, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.SelectAncestorChainSQL, headID, sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查询历史链: %w", err)
	}
	defer rows.Close()
	entries := make(map[string]config.Entry)
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries[entry.ID] = entry
	}
	return entries, rows.Err()
}

// ToolResultEntryExists 查询某条 assistant 消息的指定调用是否已有结果 Entry。
func (tx *Tx) ToolResultEntryExists(ctx context.Context, assistantEntryID, toolCallID string) (bool, error) {
	var id string
	err := tx.tx.QueryRowContext(ctx, sessql.SelectToolResultEntryIDSQL, assistantEntryID, toolCallID).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// InsertToolExecution 插入工具启动记录，不执行工具。
func (tx *Tx) InsertToolExecution(ctx context.Context, te config.ToolExecution) error {
	var arguments any
	if te.Arguments != nil {
		data, err := json.Marshal(*te.Arguments)
		if err != nil {
			return err
		}
		arguments = string(data)
	}
	var startedStr any
	if te.StartedAt != nil {
		startedStr = te.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := tx.tx.ExecContext(
		ctx,
		sessql.InsertToolExecutionSQL,
		te.SessionID,
		te.RunID,
		te.AssistantEntryID,
		te.ToolCallID,
		startedStr,
		arguments, te.Status,
	)
	if err != nil {
		return fmt.Errorf("插入工具启动记录: %w", err)
	}
	return nil
}

// UpdateToolExecutionResult 写入完成时间及结果 Entry ID，不判断业务上是否允许完成。
func (tx *Tx) UpdateToolExecutionResult(ctx context.Context, assistantEntryID, toolCallID, resultEntryID string, finishedAt time.Time, input config.ToolResultInput) error {
	nowStr := finishedAt.UTC().Format(time.RFC3339Nano)
	res, err := tx.tx.ExecContext(ctx, sessql.UpdateToolExecutionFinishSQL, nowStr, resultEntryID, input.Status, input.Executed, input.ErrorMessage, assistantEntryID, toolCallID)
	if err != nil {
		return fmt.Errorf("更新工具执行完成记录: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新工具执行完成记录失败: %w", sql.ErrNoRows)
	}
	return nil
}

// ToolExecutionExists 查询调用的启动记录是否存在，不证明工具已实际执行。
func (tx *Tx) ToolExecutionExists(ctx context.Context, assistantEntryID, toolCallID string) (bool, error) {
	var dummy int
	err := tx.tx.QueryRowContext(ctx, sessql.SelectToolExecutionExistenceSQL, assistantEntryID, toolCallID).Scan(&dummy)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, err
}

// GetToolExecutionResultEntry 返回启动记录是否存在及关联的结果 Entry ID；空 ID 表示尚无结果。
func (tx *Tx) GetToolExecutionResultEntry(ctx context.Context, assistantEntryID, toolCallID string) (bool, string, error) {
	var resultEntryID sql.NullString
	err := tx.tx.QueryRowContext(ctx, sessql.SelectToolExecutionResultEntryIDSQL, assistantEntryID, toolCallID).Scan(&resultEntryID)
	if err == nil {
		var resID string
		if resultEntryID.Valid {
			resID = resultEntryID.String
		}
		return true, resID, nil
	}
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	return false, "", err
}

// ListToolExecutionsByRun 在事务内按 started_at 等稳定次序查询指定 Run 下的所有工具执行记录。
func (tx *Tx) ListToolExecutionsByRun(ctx context.Context, runID, sessionID string) ([]config.ToolExecution, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.SelectToolExecutionsByRunIDSQL, runID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取工具执行记录: %w", err)
	}
	defer rows.Close()

	var toolExecs []config.ToolExecution
	for rows.Next() {
		var (
			te            config.ToolExecution
			startedAtRaw  sql.NullString
			finishedAtRaw sql.NullString
			resultEntryID sql.NullString
			arguments     sql.NullString
			executed      sql.NullBool
		)

		if err := rows.Scan(
			&te.SessionID,
			&te.RunID,
			&te.AssistantEntryID,
			&te.ToolCallID,
			&startedAtRaw,
			&finishedAtRaw,
			&resultEntryID,
			&arguments, &te.Status, &executed, &te.ErrorMessage,
		); err != nil {
			return nil, fmt.Errorf("读取工具执行行: %w", err)
		}

		if arguments.Valid {
			var args map[string]any
			decoder := json.NewDecoder(bytes.NewBufferString(arguments.String))
			decoder.UseNumber()
			if err := decoder.Decode(&args); err != nil {
				return nil, fmt.Errorf("decode tool arguments: %w", err)
			}
			te.Arguments = &args
		}
		if startedAtRaw.Valid {
			startedAt, err := time.Parse(time.RFC3339Nano, startedAtRaw.String)
			if err != nil {
				return nil, err
			}
			te.StartedAt = &startedAt
		}
		if executed.Valid {
			te.Executed = &executed.Bool
		}

		if finishedAtRaw.Valid {
			t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
			if err != nil {
				return nil, fmt.Errorf("解析工具完成时间: %w", err)
			}
			te.FinishedAt = &t
		}
		if resultEntryID.Valid {
			te.ResultEntryID = resultEntryID.String
		}

		toolExecs = append(toolExecs, te)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历工具执行记录失败: %w", err)
	}

	if toolExecs == nil {
		toolExecs = []config.ToolExecution{}
	}
	return toolExecs, nil
}

// scanEntry 统一处理单条查询和列表查询的字段恢复，不跳过损坏记录。
func scanEntry(row interface{ Scan(...any) error }) (config.Entry, error) {
	var entry config.Entry
	var parent, origin, assistant, call, sourceKind, sourceID, visible sql.NullString
	var created, message string
	err := row.Scan(&entry.ID, &entry.SessionID, &parent, &entry.Sequence, &entry.RunID,
		&created, &message, &origin, &assistant, &call, &entry.Kind, &sourceKind, &sourceID, &entry.Processing, &visible)
	if err != nil {
		return config.Entry{}, fmt.Errorf("读取 Entry: %w", err)
	}
	entry.SourceKind, entry.SourceID = sourceKind.String, sourceID.String
	if visible.Valid {
		if err := json.Unmarshal([]byte(visible.String), &entry.VisibleContent); err != nil {
			return config.Entry{}, err
		}
	}
	entry.ParentID, entry.AssistantEntryID, entry.ToolCallID = parent.String, assistant.String, call.String
	entry.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return config.Entry{}, fmt.Errorf("解析 Entry 时间: %w", err)
	}
	entry.Message, err = config.DecodeMessage([]byte(message))
	if err != nil {
		return config.Entry{}, fmt.Errorf("解码 Entry %s: %w", entry.ID, err)
	}
	if origin.Valid {
		if err := json.Unmarshal([]byte(origin.String), &entry.Origin); err != nil {
			return config.Entry{}, fmt.Errorf("解码 Entry %s 来源: %w", entry.ID, err)
		}
	}
	return entry, nil
}

func nullText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
