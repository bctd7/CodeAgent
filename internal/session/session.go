package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"

	"github.com/google/uuid"
)

// Service 负责会话集合和会话元数据管理：创建、获取、列表、标题、归档、取消归档、首次绑定目录和打开会话。
type Service struct {
	store *store.Store
}

// NewService 创建会话管理服务。
func NewService(db *store.Store) *Service {
	return &Service{store: db}
}

// ValidateWorkingDir 检查路径非空、存在且为目录，并返回规范化绝对路径。不自动创建目录。
func ValidateWorkingDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("工作目录不能为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("解析目录绝对路径: %w", err)
	}
	clean := filepath.Clean(abs)
	fi, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("检查工作目录存在性: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("路径 %q 不是目录", clean)
	}
	return clean, nil
}

// ValidateSessionWorkingDir 供运行时重新初始化时进行目录有效性检查：
// 若会话从未绑定工作目录（WorkingDir 为空），属于纯聊天会话，直接返回 nil；
// 若已绑定工作目录，则必须校验目录依然存在且为目录，否则返回错误。
func ValidateSessionWorkingDir(info config.SessionInfo) error {
	if info.WorkingDir == "" {
		return nil
	}
	_, err := ValidateWorkingDir(info.WorkingDir)
	return err
}

// CreateSession 创建新会话。
// 标题允许为空，工作目录允许未绑定（传入空字符串）。
// 若传入非空目录，校验其存在且为目录并保存规范化绝对路径。
// 初始 HeadID 强制为空，不创建 Run、消息或 Agent。
func (s *Service) CreateSession(ctx context.Context, title, workingDir string, memoryEnabled bool) (config.SessionInfo, error) {
	return s.createSession(ctx, title, workingDir, nil, memoryEnabled)
}
func (s *Service) CreateSessionWithModel(ctx context.Context, title, workingDir string, selection config.ModelSelection, memoryEnabled bool) (config.SessionInfo, error) {
	if strings.TrimSpace(selection.Ref) == "" {
		return config.SessionInfo{}, fmt.Errorf("模型引用不能为空")
	}
	return s.createSession(ctx, title, workingDir, &selection, memoryEnabled)
}
func (s *Service) createSession(ctx context.Context, title, workingDir string, selection *config.ModelSelection, memoryEnabled bool) (config.SessionInfo, error) {
	var normDir string
	if workingDir != "" {
		d, err := ValidateWorkingDir(workingDir)
		if err != nil {
			return config.SessionInfo{}, fmt.Errorf("创建会话工作目录无效: %w", err)
		}
		normDir = d
	}

	now := time.Now().UTC()
	info := config.SessionInfo{
		ID:             uuid.NewString(),
		MemoryEnabled:  memoryEnabled,
		ModelSelection: selection,
		Title:          title,
		WorkingDir:     normDir,
		CreatedAt:      now,
		UpdatedAt:      now,
		ArchivedAt:     nil,
		HeadID:         "",
	}

	if err := s.store.CreateSession(ctx, info); err != nil {
		return config.SessionInfo{}, fmt.Errorf("创建会话: %w", err)
	}
	return info, nil
}

// GetSession 根据会话 ID 获取元数据，不加载历史消息。归档会话允许查询。
func (s *Service) GetSession(ctx context.Context, id string) (config.SessionInfo, error) {
	return s.store.GetSession(ctx, id)
}

// ListSessions 查询会话列表。支持归档三态筛选、字面包含搜索、排序与分页组合。
func (s *Service) ListSessions(ctx context.Context, opts config.SessionListOptions) ([]config.SessionInfo, error) {
	if opts.Limit < 0 {
		return nil, fmt.Errorf("limit 不能为负数: %d", opts.Limit)
	}
	if opts.Offset < 0 {
		return nil, fmt.Errorf("offset 不能为负数: %d", opts.Offset)
	}

	if opts.Limit == 0 {
		opts.Limit = 50
	} else if opts.Limit > 200 {
		opts.Limit = 200
	}

	switch opts.ArchiveFilter {
	case "", config.ArchiveFilterActive:
		opts.ArchiveFilter = config.ArchiveFilterActive
	case config.ArchiveFilterArchived, config.ArchiveFilterAll:
		// 合法
	default:
		return nil, fmt.Errorf("未知的归档筛选条件: %q", opts.ArchiveFilter)
	}

	return s.store.ListSessions(ctx, opts)
}

// RenameSession 修改会话标题并更新 UpdatedAt。拒绝空白新标题；归档会话禁止重命名。
func (s *Service) RenameSession(ctx context.Context, id, title string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("重命名会话失败: 新标题不能为空白字符")
	}

	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if info.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，禁止重命名: %w", id, config.ErrSessionArchived)
		}

		return tx.UpdateSessionTitle(ctx, id, title, time.Now().UTC())
	})
}

// SetTitleIfEmpty 仅在当前会话标题为空时设置新标题，避免覆盖用户已手动设置的标题。
func (s *Service) SetTitleIfEmpty(ctx context.Context, id, title string) (bool, error) {
	if strings.TrimSpace(title) == "" {
		return false, fmt.Errorf("设置标题失败: 标题不能为空白字符")
	}
	var updated bool
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if info.ArchivedAt != nil {
			return fmt.Errorf("会话已归档，禁止设置标题: %w", config.ErrSessionArchived)
		}
		updated, err = tx.SetSessionTitleIfEmpty(ctx, id, title, time.Now().UTC())
		return err
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

// BindWorkingDir 为未绑定工作目录的会话首次绑定工作目录。
// 未绑定会话仅可绑定一次；归档会话或存在未结束 Run 时禁止绑定。
func (s *Service) BindWorkingDir(ctx context.Context, id, workingDir string) error {
	normDir, err := ValidateWorkingDir(workingDir)
	if err != nil {
		return fmt.Errorf("首次绑定工作目录失败: %w", err)
	}

	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}

		if info.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，禁止绑定工作目录: %w", id, config.ErrSessionArchived)
		}

		if info.WorkingDir != "" {
			return fmt.Errorf("会话 %s 已经绑定工作目录 %q，禁止重复更换或重新绑定: %w", id, info.WorkingDir, config.ErrWorkingDirAlreadyBound)
		}

		activeRunID, err := tx.GetActiveRunID(ctx, id)
		if err == nil {
			return fmt.Errorf("会话 %s 存在未结束的任务 %s，禁止绑定工作目录: %w", id, activeRunID, config.ErrRunActive)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("检查未结束任务: %w", err)
		}

		return tx.BindSessionWorkingDir(ctx, id, normDir, time.Now().UTC())
	})
}

// ArchiveSession 原子化归档会话。存在未结束 Run 时拒绝归档。
func (s *Service) ArchiveSession(ctx context.Context, id string) error {
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if info.ArchivedAt != nil {
			return nil
		}
		activeID, err := tx.GetActiveRunID(ctx, id)
		if err == nil {
			return fmt.Errorf("会话存在未结束任务 %s，无法归档: %w", activeID, config.ErrRunActive)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.ArchiveSession(ctx, id, time.Now().UTC())
	})
}

// UnarchiveSession 原子化取消归档会话。
func (s *Service) UnarchiveSession(ctx context.Context, id string) error {
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if info.ArchivedAt == nil {
			return nil
		}
		return tx.UnarchiveSession(ctx, id, time.Now().UTC())
	})
}

// OpenSession 检查会话存在且未归档，返回轻量 Session 实例。已归档会话拒绝打开。
func (s *Service) OpenSession(ctx context.Context, id string) (*Session, error) {
	info, err := s.store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("打开会话: %w", err)
	}

	if info.ArchivedAt != nil {
		return nil, fmt.Errorf("会话 %s 已归档，拒绝打开: %w", id, config.ErrSessionArchived)
	}

	return &Session{
		store:     s.store,
		sessionID: id,
	}, nil
}

// ListUnfinishedRuns 查询待确认的任务；读取不会自动中断它们。
func (s *Service) ListUnfinishedRuns(ctx context.Context, sessionID string) ([]config.RunRecord, error) {
	return s.store.ListUnfinishedRuns(ctx, sessionID)
}

// MarkRunAborted 由调用方确认任务不再运行后显式调用，不重放工具或覆盖已结束结果。
func (s *Service) MarkRunAborted(ctx context.Context, runID, errorMessage string) error {
	if errorMessage == "" {
		errorMessage = "程序中断"
	}
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		sessionID, err := tx.GetRunSessionID(ctx, runID)
		if err != nil {
			return err
		}
		rec, err := tx.GetRun(ctx, runID, sessionID)
		if err != nil {
			return err
		}
		if rec.FinishedAt != nil {
			return fmt.Errorf("任务已经结束，不能标记中断: %w", config.ErrRunFinished)
		}
		return tx.FinishRun(ctx, runID, config.RunCompletion{
			State: config.RunStateAborted, ErrorMessage: errorMessage,
		}, time.Now().UTC())
	})
}

func (s *Service) SetModelSelection(ctx context.Context, id string, selection config.ModelSelection) error {
	if strings.TrimSpace(selection.Ref) == "" {
		return fmt.Errorf("模型引用不能为空")
	}
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		info, err := tx.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if info.ArchivedAt != nil {
			return config.ErrSessionArchived
		}
		return tx.UpdateModelSelection(ctx, id, selection, time.Now().UTC())
	})
}
