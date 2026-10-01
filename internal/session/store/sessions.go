package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"CodeAgent/internal/session/config"
	sessql "CodeAgent/internal/session/sql"
)

// CreateSession 在事务内创建一条新的会话元数据记录。初始 HeadID 与 ArchivedAt 强制为 NULL。
func (tx *Tx) CreateSession(ctx context.Context, info config.SessionInfo) error {
	var selection any
	if info.ModelSelection != nil {
		data, err := json.Marshal(info.ModelSelection)
		if err != nil {
			return err
		}
		selection = string(data)
	}
	nowStr := info.CreatedAt.UTC().Format(time.RFC3339Nano)
	updatedStr := info.UpdatedAt.UTC().Format(time.RFC3339Nano)

	_, err := tx.tx.ExecContext(
		ctx,
		sessql.InsertSessionSQL,
		info.ID,
		info.Title,
		info.WorkingDir,
		nowStr,
		updatedStr,
		nil, // 新建会话归档状态必须为 NULL
		nil, // 新建会话初始 HeadID 强制为 NULL，避免跨会话携带
		selection,
	)
	if err != nil {
		return fmt.Errorf("创建会话记录: %w", err)
	}
	return nil
}

// GetSession 在事务内根据会话 ID 读取单条会话元数据。
func (tx *Tx) GetSession(ctx context.Context, id string) (config.SessionInfo, error) {
	return scanSession(tx.tx.QueryRowContext(ctx, sessql.SelectSessionByIDSQL, id))
}

// ListSessions 在事务内按照筛选、搜索、排序与分页要求列出会话。
func (tx *Tx) ListSessions(ctx context.Context, opts config.SessionListOptions) ([]config.SessionInfo, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.SelectSessionsSQL,
		opts.ArchiveFilter, opts.ArchiveFilter, opts.ArchiveFilter,
		opts.TitleSearch, opts.TitleSearch, opts.Limit, opts.Offset)
	if err != nil {
		return nil, fmt.Errorf("查询会话列表: %w", err)
	}
	defer rows.Close()

	var sessions []config.SessionInfo
	for rows.Next() {
		info, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, info)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话列表失败: %w", err)
	}

	if sessions == nil {
		sessions = []config.SessionInfo{}
	}
	return sessions, nil
}

// UpdateSessionTitle 更新会话标题与更新时间。
func (tx *Tx) UpdateSessionTitle(ctx context.Context, id, title string, updatedAt time.Time) error {
	res, err := tx.tx.ExecContext(ctx, sessql.UpdateSessionTitleSQL, title, updatedAt.UTC().Format(time.RFC3339Nano), id)
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

// SetSessionTitleIfEmpty 仅在当前标题为空或 NULL 时条件更新会话标题。返回是否发生更新。
func (tx *Tx) SetSessionTitleIfEmpty(ctx context.Context, id, title string, updatedAt time.Time) (bool, error) {
	res, err := tx.tx.ExecContext(ctx, sessql.UpdateSessionTitleIfEmptySQL, title, updatedAt.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, fmt.Errorf("按条件更新空标题: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("获取受影响行数: %w", err)
	}
	return rows > 0, nil
}

// BindSessionWorkingDir 绑定会话工作目录。
func (tx *Tx) BindSessionWorkingDir(ctx context.Context, id, workingDir string, updatedAt time.Time) error {
	res, err := tx.tx.ExecContext(ctx, sessql.UpdateSessionWorkingDirSQL, workingDir, updatedAt.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("更新会话工作目录: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新会话工作目录失败: %w", sql.ErrNoRows)
	}
	return nil
}

// UpdateSessionHead 更新会话 HeadID 与更新时间。
func (tx *Tx) UpdateSessionHead(ctx context.Context, id, headID string, updatedAt time.Time) error {
	res, err := tx.tx.ExecContext(ctx, sessql.UpdateSessionHeadSQL, headID, updatedAt.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("更新会话 Head: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新会话 Head 失败: %w", sql.ErrNoRows)
	}
	return nil
}

// ArchiveSession 写入归档时间，业务条件由 Service 在同一事务中检查。
func (tx *Tx) ArchiveSession(ctx context.Context, id string, now time.Time) error {
	timestamp := now.UTC().Format(time.RFC3339Nano)
	_, err := tx.tx.ExecContext(ctx, sessql.UpdateSessionArchiveSQL, timestamp, timestamp, id)
	return err
}

// UnarchiveSession 清空归档时间。
func (tx *Tx) UnarchiveSession(ctx context.Context, id string, now time.Time) error {
	_, err := tx.tx.ExecContext(ctx, sessql.UpdateSessionUnarchiveSQL, now.UTC().Format(time.RFC3339Nano), id)
	return err
}

// Store 级别的会话读写包装方法

func (s *Store) CreateSession(ctx context.Context, info config.SessionInfo) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		return tx.CreateSession(ctx, info)
	})
}

func (s *Store) GetSession(ctx context.Context, id string) (config.SessionInfo, error) {
	var info config.SessionInfo
	err := s.WithReadTx(ctx, func(tx *Tx) error {
		var err error
		info, err = tx.GetSession(ctx, id)
		return err
	})
	return info, err
}

func (s *Store) ListSessions(ctx context.Context, opts config.SessionListOptions) ([]config.SessionInfo, error) {
	var sessions []config.SessionInfo
	err := s.WithReadTx(ctx, func(tx *Tx) error {
		var err error
		sessions, err = tx.ListSessions(ctx, opts)
		return err
	})
	return sessions, err
}

func (tx *Tx) UpdateModelSelection(ctx context.Context, id string, selection config.ModelSelection, now time.Time) error {
	data, err := json.Marshal(selection)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, sessql.UpdateModelSelectionSQL, string(data), now.UTC().Format(time.RFC3339Nano), id)
	return err
}

// scanSession 统一单条与列表查询的元数据转换。
func scanSession(row interface{ Scan(...any) error }) (config.SessionInfo, error) {
	var (
		info         config.SessionInfo
		createdAtRaw string
		updatedAtRaw string
		archivedAt   sql.NullString
		headID       sql.NullString
		selection    sql.NullString
	)

	err := row.Scan(
		&info.ID,
		&info.Title,
		&info.WorkingDir,
		&createdAtRaw,
		&updatedAtRaw,
		&archivedAt,
		&headID,
		&selection,
	)
	if err != nil {
		return config.SessionInfo{}, fmt.Errorf("获取会话: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
	if err != nil {
		return config.SessionInfo{}, fmt.Errorf("解析会话创建时间: %w", err)
	}
	info.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
	if err != nil {
		return config.SessionInfo{}, fmt.Errorf("解析会话更新时间: %w", err)
	}
	info.UpdatedAt = updatedAt

	if archivedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, archivedAt.String)
		if err != nil {
			return config.SessionInfo{}, fmt.Errorf("解析会话归档时间: %w", err)
		}
		info.ArchivedAt = &t
	}

	if headID.Valid {
		info.HeadID = headID.String
	}

	if selection.Valid {
		var v config.ModelSelection
		if err := json.Unmarshal([]byte(selection.String), &v); err != nil {
			return config.SessionInfo{}, fmt.Errorf("解码模型选择: %w", err)
		}
		info.ModelSelection = &v
	}
	return info, nil
}
