package session

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service 负责多会话的管理：创建、查询、列表、重命名、归档与打开。
type Service struct {
	store *Store
}

// NewService 创建会话管理服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// CreateSession 创建新的会话。传入标题和工作目录，生成唯一 ID 并初始化时间，初始 HeadID 为空。
func (s *Service) CreateSession(ctx context.Context, title, workingDir string) (SessionInfo, error) {
	now := time.Now().UTC()
	info := SessionInfo{
		ID:         uuid.NewString(),
		Title:      title,
		WorkingDir: workingDir,
		CreatedAt:  now,
		UpdatedAt:  now,
		ArchivedAt: nil,
		HeadID:     "",
	}

	if err := s.store.CreateSession(ctx, info); err != nil {
		return SessionInfo{}, fmt.Errorf("创建会话服务: %w", err)
	}
	return info, nil
}

// GetSession 根据 ID 获取会话元数据。
func (s *Service) GetSession(ctx context.Context, id string) (SessionInfo, error) {
	return s.store.GetSession(ctx, id)
}

// ListSessions 获取会话列表。默认 50 项，最大 200 项；拒绝负数 limit 与 offset。默认不包含归档记录。
func (s *Service) ListSessions(ctx context.Context, limit, offset int, includeArchived bool) ([]SessionInfo, error) {
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

	return s.store.ListSessions(ctx, limit, offset, includeArchived)
}

// RenameSession 修改会话标题，更新 UpdatedAt。不存在时返回包含 sql.ErrNoRows 的错误。
func (s *Service) RenameSession(ctx context.Context, id, title string) error {
	now := time.Now().UTC()
	return s.store.UpdateSessionTitle(ctx, id, title, now)
}

// ArchiveSession 归档会话。同一事务内完成检查与更新。
func (s *Service) ArchiveSession(ctx context.Context, id string) error {
	return s.store.ArchiveSession(ctx, id, time.Now().UTC())
}

// UnarchiveSession 取消归档会话。同一事务内完成检查与更新。
func (s *Service) UnarchiveSession(ctx context.Context, id string) error {
	return s.store.UnarchiveSession(ctx, id, time.Now().UTC())
}

// OpenSession 返回绑定 SessionID 的 Session 实例；不加载模型配置、不启动 Agent。
// 若会话不存在，返回包含 sql.ErrNoRows 的错误。
func (s *Service) OpenSession(ctx context.Context, id string) (*Session, error) {
	_, err := s.store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("打开会话: %w", err)
	}

	return &Session{
		store:     s.store,
		sessionID: id,
	}, nil
}
