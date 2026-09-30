package session

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Open 打开 SQLite 数据库并配置单连接与必要 PRAGMA 参数。
// path 按实际文件路径处理，支持中文、空格及特殊字符。Open 仅验证连接，不自动建表或升级版本。
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("数据库文件路径不能为空")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析数据库文件路径: %w", err)
	}
	uriPath := filepath.ToSlash(absPath)
	if filepath.VolumeName(absPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath}
	params := url.Values{}
	params.Add("_pragma", "foreign_keys(ON)")
	params.Add("_pragma", "busy_timeout(5000)")
	uri.RawQuery = params.Encode()

	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}

	// 单连接池配置，避免读写锁等待与连接交替冲突
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}

	return &Store{db: db}, nil
}

// Init 显式初始化数据库表结构与版本信息。
func (s *Store) Init(ctx context.Context) error {
	return s.initSchema(ctx)
}

// Close 关闭底层数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateSession 创建一条会话元数据记录。
func (s *Store) CreateSession(ctx context.Context, info SessionInfo) error {
	const query = `
INSERT INTO sessions (id, title, working_dir, created_at, updated_at, archived_at, head_id)
VALUES (?, ?, ?, ?, ?, ?, ?)
`
	var archivedAt any
	if info.ArchivedAt != nil {
		archivedAt = info.ArchivedAt.UTC().Format(time.RFC3339Nano)
	}

	var headID any
	if info.HeadID != "" {
		headID = info.HeadID
	}

	_, err := s.db.ExecContext(
		ctx,
		query,
		info.ID,
		info.Title,
		info.WorkingDir,
		info.CreatedAt.UTC().Format(time.RFC3339Nano),
		info.UpdatedAt.UTC().Format(time.RFC3339Nano),
		archivedAt,
		headID,
	)
	if err != nil {
		return fmt.Errorf("创建会话: %w", err)
	}
	return nil
}

// GetSession 根据会话 ID 读取单条会话元数据。
func (s *Store) GetSession(ctx context.Context, id string) (SessionInfo, error) {
	const query = `
SELECT id, title, working_dir, created_at, updated_at, archived_at, head_id
FROM sessions
WHERE id = ?
`
	var (
		info         SessionInfo
		createdAtRaw string
		updatedAtRaw string
		archivedAt   sql.NullString
		headID       sql.NullString
	)

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&info.ID,
		&info.Title,
		&info.WorkingDir,
		&createdAtRaw,
		&updatedAtRaw,
		&archivedAt,
		&headID,
	)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("获取会话: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("解析会话创建时间: %w", err)
	}
	info.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("解析会话更新时间: %w", err)
	}
	info.UpdatedAt = updatedAt

	if archivedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, archivedAt.String)
		if err != nil {
			return SessionInfo{}, fmt.Errorf("解析会话归档时间: %w", err)
		}
		info.ArchivedAt = &t
	} else {
		info.ArchivedAt = nil
	}

	if headID.Valid {
		info.HeadID = headID.String
	} else {
		info.HeadID = ""
	}

	return info, nil
}

// ListSessions 列出数据库中的会话记录。
func (s *Store) ListSessions(ctx context.Context, limit, offset int, includeArchived bool) ([]SessionInfo, error) {
	var query string
	if includeArchived {
		query = `
SELECT id, title, working_dir, created_at, updated_at, archived_at, head_id
FROM sessions
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', created_at) DESC, id ASC
LIMIT ? OFFSET ?
`
	} else {
		query = `
SELECT id, title, working_dir, created_at, updated_at, archived_at, head_id
FROM sessions
WHERE archived_at IS NULL
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', created_at) DESC, id ASC
LIMIT ? OFFSET ?
`
	}

	rows, err := s.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询会话列表: %w", err)
	}
	defer rows.Close()

	var sessions []SessionInfo
	for rows.Next() {
		var (
			info         SessionInfo
			createdAtRaw string
			updatedAtRaw string
			archivedAt   sql.NullString
			headID       sql.NullString
		)

		if err := rows.Scan(
			&info.ID,
			&info.Title,
			&info.WorkingDir,
			&createdAtRaw,
			&updatedAtRaw,
			&archivedAt,
			&headID,
		); err != nil {
			return nil, fmt.Errorf("读取会话行数据: %w", err)
		}

		createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析会话创建时间: %w", err)
		}
		info.CreatedAt = createdAt

		updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析会话更新时间: %w", err)
		}
		info.UpdatedAt = updatedAt

		if archivedAt.Valid {
			t, err := time.Parse(time.RFC3339Nano, archivedAt.String)
			if err != nil {
				return nil, fmt.Errorf("解析会话归档时间: %w", err)
			}
			info.ArchivedAt = &t
		} else {
			info.ArchivedAt = nil
		}

		if headID.Valid {
			info.HeadID = headID.String
		} else {
			info.HeadID = ""
		}

		sessions = append(sessions, info)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话列表失败: %w", err)
	}

	if sessions == nil {
		sessions = []SessionInfo{}
	}
	return sessions, nil
}

// UpdateSessionTitle 更新会话标题与更新时间。
func (s *Store) UpdateSessionTitle(ctx context.Context, id, title string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?
`, title, updatedAt.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("更新会话标题: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新会话标题失败: %w", sql.ErrNoRows)
	}
	return nil
}

// ArchiveSession 原子化归档会话。在同一事务内完成存在性检查、未结束 Run 冲突检查及归档更新。
func (s *Store) ArchiveSession(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启归档事务: %w", err)
	}
	defer tx.Rollback()

	var archivedAt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT archived_at FROM sessions WHERE id = ?", id).Scan(&archivedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
		}
		return fmt.Errorf("查询会话状态: %w", err)
	}

	if archivedAt.Valid {
		return nil
	}

	var activeRunID string
	err = tx.QueryRowContext(ctx, "SELECT id FROM runs WHERE session_id = ? AND finished_at IS NULL LIMIT 1", id).Scan(&activeRunID)
	if err == nil {
		return fmt.Errorf("会话 %s 存在未结束的任务 %s，无法归档: %w", id, activeRunID, ErrRunActive)
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("检查未结束任务: %w", err)
	}

	nowStr := now.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET archived_at = ?, updated_at = ? WHERE id = ?", nowStr, nowStr, id)
	if err != nil {
		return fmt.Errorf("更新会话归档状态: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交归档事务: %w", err)
	}
	return nil
}

// UnarchiveSession 原子化取消归档会话。在同一事务内完成存在性检查及状态更新。
func (s *Store) UnarchiveSession(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启取消归档事务: %w", err)
	}
	defer tx.Rollback()

	var archivedAt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT archived_at FROM sessions WHERE id = ?", id).Scan(&archivedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
		}
		return fmt.Errorf("查询会话状态: %w", err)
	}

	if !archivedAt.Valid {
		return nil
	}

	nowStr := now.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET archived_at = NULL, updated_at = ? WHERE id = ?", nowStr, id)
	if err != nil {
		return fmt.Errorf("更新会话取消归档状态: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交取消归档事务: %w", err)
	}
	return nil
}

// ListUnfinishedRuns 查询未结束的 Run 记录。sessionID 为空时跨会话查询。
func (s *Store) ListUnfinishedRuns(ctx context.Context, sessionID string) ([]RunRecord, error) {
	var (
		query string
		args  []any
	)

	if sessionID != "" {
		query = `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
FROM runs
WHERE session_id = ? AND finished_at IS NULL
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', started_at) ASC, id ASC
`
		args = []any{sessionID}
	} else {
		query = `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
FROM runs
WHERE finished_at IS NULL
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', started_at) ASC, id ASC
`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询未结束任务: %w", err)
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
			return nil, fmt.Errorf("读取未结束任务行: %w", err)
		}

		if startHeadID.Valid {
			rec.StartHeadID = startHeadID.String
		}
		if lastEntryID.Valid {
			rec.LastEntryID = lastEntryID.String
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

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历未结束任务列表失败: %w", err)
	}

	if records == nil {
		records = []RunRecord{}
	}
	return records, nil
}

// MarkRunAborted 将指定未结束 Run 显式标记为中断。
func (s *Store) MarkRunAborted(ctx context.Context, runID string, errorMessage string) error {
	if errorMessage == "" {
		errorMessage = "程序中断"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启标记中断事务: %w", err)
	}
	defer tx.Rollback()

	var finishedAt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT finished_at FROM runs WHERE id = ?", runID).Scan(&finishedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("标记任务中断失败: %w", sql.ErrNoRows)
		}
		return fmt.Errorf("查询任务状态: %w", err)
	}

	if finishedAt.Valid {
		return fmt.Errorf("任务 %s 已经处于结束状态，无法标记为中断: %w", runID, ErrRunFinished)
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `
UPDATE runs
SET finished_at = ?, state = ?, stop_reason = '', raw_stop_reason = '', error_message = ?
WHERE id = ?
`, nowStr, string(RunStateAborted), errorMessage, runID)
	if err != nil {
		return fmt.Errorf("更新任务中断终态: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交任务中断事务: %w", err)
	}
	return nil
}
