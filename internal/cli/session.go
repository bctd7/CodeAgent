package cli

import (
	"context"
	"fmt"

	runtimeconfig "CodeAgent/internal/runtime/config"
	sessionconfig "CodeAgent/internal/session/config"
)

// open 复用 Host 的创建及恢复能力；目录校验、所有权、模型解析仍由后端负责。
func (c *CLI) open(ctx context.Context, i Invocation) (Runtime, *sessionconfig.SessionInfo, error) {
	if i.SessionID == "" {
		if i.ModelRef == "" {
			return nil, nil, fmt.Errorf("新建会话必须显式指定模型引用")
		}
		info, err := c.backend.CreateSession(ctx, runtimeconfig.CreateSessionOptions{
			Title: i.Title, WorkingDir: i.WorkingDir, Model: sessionconfig.ModelSelection{Ref: i.ModelRef},
		})
		if err != nil {
			return nil, nil, err
		}
		r, err := c.backend.OpenSession(ctx, info.ID, runtimeconfig.OpenSessionOptions{})
		return r, &info, err
	}
	var opts runtimeconfig.OpenSessionOptions
	if i.ModelRef != "" {
		opts.Model = &sessionconfig.ModelSelection{Ref: i.ModelRef}
	}
	r, err := c.backend.OpenSession(ctx, i.SessionID, opts)
	if err != nil {
		return nil, nil, err
	}
	if i.WorkingDir != "" {
		if r.Snapshot().WorkingDir != "" {
			return nil, nil, fmt.Errorf("会话已绑定工作目录；恢复时请省略 --directory，不能重新绑定或更换")
		}
		if err := r.BindWorkingDir(ctx, i.WorkingDir); err != nil {
			return nil, nil, err
		}
	}
	return r, nil, nil
}

func (c *CLI) manageSession(ctx context.Context, i Invocation) (any, error) {
	switch i.Action {
	case "session:list":
		return c.backend.ListSessions(ctx, i.ListOptions)
	case "session:create":
		return c.backend.CreateSession(ctx, runtimeconfig.CreateSessionOptions{
			Title: i.Title, WorkingDir: i.WorkingDir, Model: sessionconfig.ModelSelection{Ref: i.ModelRef},
		})
	case "session:rename":
		if err := c.backend.RenameSession(ctx, i.SessionID, i.Title); err != nil {
			return nil, err
		}
	case "session:archive":
		if err := c.backend.ArchiveSession(ctx, i.SessionID); err != nil {
			return nil, err
		}
	case "session:unarchive":
		if err := c.backend.UnarchiveSession(ctx, i.SessionID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("未知会话管理操作")
	}
	return struct {
		Action string `json:"action"`
		ID     string `json:"id"`
	}{i.Action, i.SessionID}, nil
}
