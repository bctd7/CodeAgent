package session

import (
	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateChildTask 绑定尚未运行的独立子会话；不创建 Agent、不执行任务。
func (s *Session) CreateChildTask(ctx context.Context, input config.ChildTaskInput) (config.ChildTask, error) {
	if strings.TrimSpace(input.ID) == "" || input.ChildSessionID == s.sessionID {
		return config.ChildTask{}, fmt.Errorf("非法子任务身份")
	}
	if input.Role != "explore" && input.Role != "worker" {
		return config.ChildTask{}, fmt.Errorf("非法子任务角色")
	}
	if (input.Role == "explore" && len(input.FileScope) > 0) || len(input.FileScope) > 256 {
		return config.ChildTask{}, fmt.Errorf("非法文件范围")
	}
	for _, p := range input.FileScope {
		if strings.TrimSpace(p) == "" {
			return config.ChildTask{}, fmt.Errorf("文件范围不能为空")
		}
	}
	var result config.ChildTask
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		if _, _, err := s.activeRun(ctx, tx, input.ParentRunID); err != nil {
			return err
		}
		nested, err := tx.IsChildSession(ctx, s.sessionID)
		if err != nil {
			return err
		}
		if nested {
			return fmt.Errorf("子任务不能创建孙任务")
		}
		child, err := tx.GetSession(ctx, input.ChildSessionID)
		if err != nil {
			return err
		}
		if child.HeadID != "" || child.ArchivedAt != nil {
			return fmt.Errorf("子会话必须为空且未归档")
		}
		// 防止把已经作为其他任务父会话的空会话登记为子会话。
		children, err := tx.ListChildTasks(ctx, input.ChildSessionID)
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return fmt.Errorf("子会话不能已有子任务")
		}
		result = config.ChildTask{ChildTaskInput: input, ParentSessionID: s.sessionID, CreatedAt: time.Now().UTC()}
		return tx.InsertChildTask(ctx, result)
	})
	if err != nil {
		return config.ChildTask{}, err
	}
	return result, nil
}
func (s *Session) GetChildTask(ctx context.Context, id string) (config.ChildTask, error) {
	var value config.ChildTask
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		var err error
		value, err = tx.GetChildTask(ctx, id, s.sessionID)
		return err
	})
	return value, err
}
func (s *Session) ListChildTasks(ctx context.Context) ([]config.ChildTask, error) {
	var values []config.ChildTask
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error { var err error; values, err = tx.ListChildTasks(ctx, s.sessionID); return err })
	return values, err
}

// DeliverChildResult 将一次已结束子 Run 的宿主摘要原子写入父 Run。
// (父 Run, child_run, 子 Run) 唯一；同内容重试返回原 Entry，不同内容返回冲突。
// 子 Run 原始历史保留在子会话，父会话不复制整份历史。
func (s *Session) DeliverChildResult(ctx context.Context, parentRunID, taskID, childRunID string, message *llm.UserMessage) (config.Entry, error) {
	input := config.ContextInput{SourceKind: "child_run", SourceID: childRunID, Message: message}
	if err := validateContext(input); err != nil {
		return config.Entry{}, err
	}
	data, err := config.EncodeMessage(message)
	if err != nil {
		return config.Entry{}, err
	}
	var entry config.Entry
	err = s.store.WithTx(ctx, func(tx *store.Tx) error {
		task, err := tx.GetChildTask(ctx, taskID, s.sessionID)
		if err != nil {
			return err
		}
		if _, err := tx.GetRun(ctx, parentRunID, s.sessionID); err != nil {
			return err
		}
		child, err := tx.GetRun(ctx, childRunID, task.ChildSessionID)
		if err != nil {
			return err
		}
		if child.FinishedAt == nil {
			return fmt.Errorf("子 Run 尚未结束")
		}
		id, err := tx.FindContextSource(ctx, parentRunID, "child_run", childRunID)
		if err == nil {
			entry, err = tx.GetEntry(ctx, id, s.sessionID)
			if err != nil {
				return err
			}
			old, err := config.EncodeMessage(entry.Message)
			if err != nil {
				return err
			}
			if string(old) != string(data) {
				return config.ErrSessionConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		entry, err = s.appendContext(ctx, tx, parentRunID, input)
		return err
	})
	if err != nil {
		return config.Entry{}, err
	}
	return entry, nil
}
