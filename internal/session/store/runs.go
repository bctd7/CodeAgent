package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"CodeAgent/internal/session/config"
	sessql "CodeAgent/internal/session/sql"
)

// CreateRun 在事务内创建一条 pending 状态的 Run 记录。
func (tx *Tx) CreateRun(ctx context.Context, rec config.RunRecord) error {
	var startHeadVal any
	if rec.StartHeadID != "" {
		startHeadVal = rec.StartHeadID
	}

	startedStr := rec.StartedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.tx.ExecContext(
		ctx,
		sessql.InsertRunSQL,
		rec.ID,
		rec.SessionID,
		startHeadVal,
		rec.InputEntryID,
		startedStr,
		string(rec.State),
	)
	if err != nil {
		return fmt.Errorf("创建 Run 记录: %w", err)
	}
	return nil
}

// GetRun 在事务内查询指定会话的指定 Run 记录。
func (tx *Tx) GetRun(ctx context.Context, id, sessionID string) (config.RunRecord, error) {
	return scanRun(tx.tx.QueryRowContext(ctx, sessql.SelectRunByIDSQL, id, sessionID))
}

// GetRunSessionID 查询任务所属会话，不决定任务能否恢复。
func (tx *Tx) GetRunSessionID(ctx context.Context, runID string) (string, error) {
	var sessionID string
	err := tx.tx.QueryRowContext(ctx, sessql.SelectRunSessionIDSQL, runID).Scan(&sessionID)
	return sessionID, err
}

// GetActiveRunID 在事务内查询指定会话中未结束任务的 ID。
func (tx *Tx) GetActiveRunID(ctx context.Context, sessionID string) (string, error) {
	var activeRunID string
	err := tx.tx.QueryRowContext(ctx, sessql.SelectActiveRunIDSQL, sessionID).Scan(&activeRunID)
	if err != nil {
		return "", err
	}
	return activeRunID, nil
}

// GetRunStepsAndFinishedAt 查询指定 Run 的步骤与结束时间。
func (tx *Tx) GetRunStepsAndFinishedAt(ctx context.Context, id, sessionID string) (int, *time.Time, error) {
	var (
		steps         int
		finishedAtRaw sql.NullString
	)
	err := tx.tx.QueryRowContext(ctx, sessql.SelectRunStepsAndFinishedAtSQL, id, sessionID).Scan(&steps, &finishedAtRaw)
	if err != nil {
		return 0, nil, err
	}
	if finishedAtRaw.Valid {
		t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
		if err != nil {
			return 0, nil, fmt.Errorf("解析任务结束时间: %w", err)
		}
		return steps, &t, nil
	}
	return steps, nil, nil
}

// GetRunLastEntryAndFinishedAt 查询指定 Run 的 last_entry_id 与结束时间。
func (tx *Tx) GetRunLastEntryAndFinishedAt(ctx context.Context, id, sessionID string) (string, *time.Time, error) {
	var (
		lastEntryID   sql.NullString
		finishedAtRaw sql.NullString
	)
	err := tx.tx.QueryRowContext(ctx, sessql.SelectRunLastEntryAndFinishedAtSQL, id, sessionID).Scan(&lastEntryID, &finishedAtRaw)
	if err != nil {
		return "", nil, err
	}
	var lastID string
	if lastEntryID.Valid {
		lastID = lastEntryID.String
	}
	if finishedAtRaw.Valid {
		t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
		if err != nil {
			return "", nil, fmt.Errorf("解析任务结束时间: %w", err)
		}
		return lastID, &t, nil
	}
	return lastID, nil, nil
}

// UpdateRunLastEntry 更新 Run 的最后 Entry 引用。
func (tx *Tx) UpdateRunLastEntry(ctx context.Context, runID, lastEntryID string) error {
	res, err := tx.tx.ExecContext(ctx, sessql.UpdateRunLastEntryIDSQL, lastEntryID, runID)
	if err != nil {
		return fmt.Errorf("更新 Run.LastEntryID: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新 Run.LastEntryID 失败: %w", sql.ErrNoRows)
	}
	return nil
}

// IncrementRunSteps 更新已执行步数。
func (tx *Tx) IncrementRunSteps(ctx context.Context, runID string, newSteps int) error {
	res, err := tx.tx.ExecContext(ctx, sessql.IncrementRunStepsSQL, newSteps, runID)
	if err != nil {
		return fmt.Errorf("更新任务步数: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新任务步数失败: %w", sql.ErrNoRows)
	}
	return nil
}

// FinishRun 在事务内更新 Run 终态与停止说明。
func (tx *Tx) FinishRun(ctx context.Context, runID string, completion config.RunCompletion, finishedAt time.Time) error {
	nowStr := finishedAt.UTC().Format(time.RFC3339Nano)
	res, err := tx.tx.ExecContext(
		ctx,
		sessql.UpdateRunFinishSQL,
		nowStr,
		string(completion.State),
		string(completion.StopReason),
		completion.RawStopReason,
		completion.ErrorMessage,
		runID,
	)
	if err != nil {
		return fmt.Errorf("更新任务终态: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新任务终态失败: %w", sql.ErrNoRows)
	}
	return nil
}

// ListRuns 按输入 Entry 的 Sequence 倒序查询会话的所有 Run。
func (tx *Tx) ListRuns(ctx context.Context, sessionID string, limit, offset int) ([]config.RunRecord, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.SelectListRunsSQL, sessionID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询会话任务列表: %w", err)
	}
	defer rows.Close()

	var records []config.RunRecord
	for rows.Next() {
		rec, err := scanRun(rows)
		if err != nil {
			return nil, err
		}

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话任务列表失败: %w", err)
	}

	if records == nil {
		records = []config.RunRecord{}
	}
	return records, nil
}

// ListUnfinishedRuns 在事务内查询未结束的 Run 记录。sessionID 为空时跨会话查询。
func (tx *Tx) ListUnfinishedRuns(ctx context.Context, sessionID string) ([]config.RunRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID != "" {
		rows, err = tx.tx.QueryContext(ctx, sessql.SelectUnfinishedRunsBySessionSQL, sessionID)
	} else {
		rows, err = tx.tx.QueryContext(ctx, sessql.SelectAllUnfinishedRunsSQL)
	}
	if err != nil {
		return nil, fmt.Errorf("查询未结束任务: %w", err)
	}
	defer rows.Close()

	var records []config.RunRecord
	for rows.Next() {
		rec, err := scanRun(rows)
		if err != nil {
			return nil, err
		}

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历未结束任务列表失败: %w", err)
	}

	if records == nil {
		records = []config.RunRecord{}
	}
	return records, nil
}

// Store 级别的 Run 方法

func (s *Store) ListUnfinishedRuns(ctx context.Context, sessionID string) ([]config.RunRecord, error) {
	var records []config.RunRecord
	err := s.WithReadTx(ctx, func(tx *Tx) error {
		var err error
		records, err = tx.ListUnfinishedRuns(ctx, sessionID)
		return err
	})
	return records, err
}

// scanRun 统一单条与列表查询的列顺序、NULL 和时间转换。
func scanRun(row interface{ Scan(...any) error }) (config.RunRecord, error) {
	var (
		rec           config.RunRecord
		startHeadID   sql.NullString
		lastEntryID   sql.NullString
		startedAtRaw  string
		finishedAtRaw sql.NullString
	)

	err := row.Scan(
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
		return config.RunRecord{}, fmt.Errorf("读取 Run: %w", err)
	}

	startedAt, err := time.Parse(time.RFC3339Nano, startedAtRaw)
	if err != nil {
		return config.RunRecord{}, fmt.Errorf("解析任务开始时间: %w", err)
	}
	rec.StartedAt = startedAt

	if finishedAtRaw.Valid {
		t, err := time.Parse(time.RFC3339Nano, finishedAtRaw.String)
		if err != nil {
			return config.RunRecord{}, fmt.Errorf("解析任务结束时间: %w", err)
		}
		rec.FinishedAt = &t
	}
	if startHeadID.Valid {
		rec.StartHeadID = startHeadID.String
	}
	if lastEntryID.Valid {
		rec.LastEntryID = lastEntryID.String
	}

	return rec, nil
}
