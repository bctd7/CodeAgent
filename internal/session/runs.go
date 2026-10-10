package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"

	"github.com/google/uuid"
)

// Session 绑定特定会话 ID，负责一个会话的 Run、消息、历史、分支及工具执行事实。
// 它不是 Agent，不负责执行模型、运行工具或管理输入队列。
type Session struct {
	store     *store.Store
	sessionID string
}

// SessionID 返回当前会话绑定的 ID。
func (s *Session) SessionID() string {
	return s.sessionID
}

// GetInfo 读取当前绑定会话的元数据，不改变目录、模型选择或历史位置。
func (s *Session) GetInfo(ctx context.Context) (config.SessionInfo, error) {
	return s.store.GetSession(ctx, s.sessionID)
}

// ListUnfinishedRuns 只查询当前会话的未结束任务，不推断旧执行者是否已经退出。
func (s *Session) ListUnfinishedRuns(ctx context.Context) ([]config.RunRecord, error) {
	return s.store.ListUnfinishedRuns(ctx, s.sessionID)
}

// StartRun 在当前会话开启一次新的任务。
// 调用方提供非空、跨重启唯一的 RunID 和合法用户消息。
func (s *Session) StartRun(ctx context.Context, runID string, userMsg *llm.UserMessage) (config.RunRecord, error) {
	if runID == "" {
		return config.RunRecord{}, fmt.Errorf("RunID 不能为空")
	}
	if userMsg == nil {
		return config.RunRecord{}, fmt.Errorf("UserMessage 不能为空")
	}
	if userMsg.Role != llm.RoleUser {
		return config.RunRecord{}, fmt.Errorf("UserMessage 角色应为 %q，实际为 %q", llm.RoleUser, userMsg.Role)
	}

	msgData, err := config.EncodeMessage(userMsg)
	if err != nil {
		return config.RunRecord{}, fmt.Errorf("编码用户消息: %w", err)
	}

	var rec config.RunRecord
	err = s.store.WithTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if sess.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，拒绝开始新任务: %w", s.sessionID, config.ErrSessionArchived)
		}

		activeRunID, err := tx.GetActiveRunID(ctx, s.sessionID)
		if err == nil {
			return fmt.Errorf("会话 %s 已存在未结束的任务 %s，禁止并发启动新任务: %w", s.sessionID, activeRunID, config.ErrRunActive)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("检查未结束任务: %w", err)
		}
		if sess.HeadID != "" {
			if _, err := tx.GetEntry(ctx, sess.HeadID, s.sessionID); err != nil {
				return fmt.Errorf("当前 Head 不属于本会话或无法读取: %w", err)
			}
		}

		inputEntryID := uuid.NewString()
		now := time.Now().UTC()

		nextSeq, err := tx.NextEntrySequence(ctx, s.sessionID)
		if err != nil {
			return err
		}

		rec = config.RunRecord{
			ID:           runID,
			SessionID:    s.sessionID,
			StartHeadID:  sess.HeadID,
			InputEntryID: inputEntryID,
			LastEntryID:  inputEntryID,
			StartedAt:    now,
			FinishedAt:   nil,
			State:        config.RunStatePending,
			Steps:        0,
		}
		if err := tx.CreateRun(ctx, rec); err != nil {
			return err
		}

		entry := config.Entry{
			ID:        inputEntryID,
			SessionID: s.sessionID,
			ParentID:  sess.HeadID,
			Sequence:  nextSeq,
			RunID:     runID,
			CreatedAt: now,
			Message:   userMsg,
			Kind:      config.MessageEntry, Processing: config.ProcessingOriginal,
		}
		if err := tx.InsertEntry(ctx, entry, string(msgData), nil); err != nil {
			return err
		}

		if err := tx.UpdateRunLastEntry(ctx, runID, inputEntryID); err != nil {
			return err
		}

		if err := tx.UpdateSessionHead(ctx, s.sessionID, inputEntryID, now); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return config.RunRecord{}, err
	}

	return rec, nil
}

// AppendMessage 向当前未结束的任务追加一条普通消息（用户或 Assistant 消息）。
// 普通入口拒绝工具结果，必须使用 AppendToolResult。
func (s *Session) AppendMessage(ctx context.Context, runID string, msg llm.Message, origin *config.MessageModelOrigin) (config.Entry, error) {
	if msg == nil {
		return config.Entry{}, fmt.Errorf("消息不能为空")
	}

	switch m := msg.(type) {
	case *llm.ToolResultMessage:
		return config.Entry{}, fmt.Errorf("普通消息追加入口不接受工具结果，必须使用 AppendToolResult")
	case *llm.UserMessage:
		if m == nil {
			return config.Entry{}, fmt.Errorf("UserMessage 不能为空")
		}
		if m.Role != llm.RoleUser {
			return config.Entry{}, fmt.Errorf("UserMessage 角色应为 %q，实际为 %q", llm.RoleUser, m.Role)
		}
		if origin != nil {
			return config.Entry{}, fmt.Errorf("用户消息不应提供 Origin 来源")
		}
	case *llm.AssistantMessage:
		if m == nil {
			return config.Entry{}, fmt.Errorf("AssistantMessage 不能为空")
		}
		if m.Role != llm.RoleAssistant {
			return config.Entry{}, fmt.Errorf("AssistantMessage 角色应为 %q，实际为 %q", llm.RoleAssistant, m.Role)
		}
		if origin == nil {
			return config.Entry{}, fmt.Errorf("assistant 消息必须提供实际请求 Origin")
		}
	default:
		return config.Entry{}, fmt.Errorf("不支持的消息类型: %T", msg)
	}

	msgData, err := config.EncodeMessage(msg)
	if err != nil {
		return config.Entry{}, fmt.Errorf("编码消息失败: %w", err)
	}

	var originData any
	if origin != nil {
		ob, err := json.Marshal(origin)
		if err != nil {
			return config.Entry{}, fmt.Errorf("序列化 Origin 失败: %w", err)
		}
		originData = string(ob)
	}

	var entry config.Entry
	err = s.store.WithTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if sess.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，拒绝追加消息: %w", s.sessionID, config.ErrSessionArchived)
		}

		runRec, err := tx.GetRun(ctx, runID, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("任务不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询任务状态: %w", err)
		}
		if runRec.FinishedAt != nil {
			return fmt.Errorf("无法向已结束的任务 %s 追加消息: %w", runID, config.ErrRunFinished)
		}
		if sess.HeadID != runRec.LastEntryID || sess.HeadID == "" {
			return fmt.Errorf("当前 Head 与活动任务不一致: %w", config.ErrSessionConflict)
		}
		head, err := tx.GetEntry(ctx, sess.HeadID, s.sessionID)
		if err != nil {
			return err
		}
		if head.RunID != runID {
			return fmt.Errorf("当前 Head 不属于活动任务: %w", config.ErrSessionConflict)
		}

		nextSeq, err := tx.NextEntrySequence(ctx, s.sessionID)
		if err != nil {
			return err
		}

		entryID := uuid.NewString()
		now := time.Now().UTC()

		entry = config.Entry{
			ID:        entryID,
			SessionID: s.sessionID,
			ParentID:  sess.HeadID,
			Sequence:  nextSeq,
			RunID:     runID,
			CreatedAt: now,
			Message:   msg,
			Kind:      config.MessageEntry, Processing: config.ProcessingOriginal,
			Origin: origin,
		}

		if err := tx.InsertEntry(ctx, entry, string(msgData), originData); err != nil {
			return err
		}

		if err := tx.UpdateRunLastEntry(ctx, runID, entryID); err != nil {
			return err
		}

		if err := tx.UpdateSessionHead(ctx, s.sessionID, entryID, now); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return config.Entry{}, err
	}

	return entry, nil
}

// RecordStep 原子增加当前 Run 的模型调用次数并返回新计数值。
func (s *Session) RecordStep(ctx context.Context, runID string) (int, error) {
	var newSteps int
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if sess.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，拒绝记录步数: %w", s.sessionID, config.ErrSessionArchived)
		}

		steps, finishedAt, err := tx.GetRunStepsAndFinishedAt(ctx, runID, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("查询任务失败: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询任务状态: %w", err)
		}
		if finishedAt != nil {
			return fmt.Errorf("任务 %s 已经结束，无法增加步骤: %w", runID, config.ErrRunFinished)
		}

		newSteps = steps + 1
		return tx.IncrementRunSteps(ctx, runID, newSteps)
	})
	if err != nil {
		return 0, err
	}
	return newSteps, nil
}

// FinishRun 结束指定的任务。状态必须为终态；已结束的 Run 重复结束返回冲突错误。
func (s *Session) FinishRun(ctx context.Context, runID string, completion config.RunCompletion) (config.RunRecord, error) {
	switch completion.State {
	case config.RunStateSuccess:
		if completion.StopReason != llm.StopStop && completion.StopReason != llm.StopLength && completion.StopReason != llm.StopToolUse {
			return config.RunRecord{}, fmt.Errorf("成功任务的结束原因无效: %q", completion.StopReason)
		}
	case config.RunStateError:
		if completion.StopReason != "" && completion.StopReason != llm.StopSafety {
			return config.RunRecord{}, fmt.Errorf("失败任务的结束原因无效: %q", completion.StopReason)
		}
	case config.RunStateAborted, config.RunStateDeferred:
		if completion.StopReason != "" {
			return config.RunRecord{}, fmt.Errorf("取消任务不能带结束原因: %q", completion.StopReason)
		}
	default:
		return config.RunRecord{}, fmt.Errorf("结束任务的状态必须为终态，非法值: %q", completion.State)
	}

	var rec config.RunRecord
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if sess.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，拒绝结束任务: %w", s.sessionID, config.ErrSessionArchived)
		}

		runRec, err := tx.GetRun(ctx, runID, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("任务不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询任务信息: %w", err)
		}
		if runRec.FinishedAt != nil {
			return fmt.Errorf("任务 %s 已经处于结束状态，不能重复结束: %w", runID, config.ErrRunFinished)
		}

		now := time.Now().UTC()
		if err := tx.FinishRun(ctx, runID, completion, now); err != nil {
			return err
		}

		runRec.FinishedAt = &now
		runRec.State = completion.State
		runRec.StopReason = completion.StopReason
		runRec.RawStopReason = completion.RawStopReason
		runRec.ErrorMessage = completion.ErrorMessage
		rec = runRec

		return nil
	})
	if err != nil {
		return config.RunRecord{}, err
	}

	return rec, nil
}

// GetRun 读取指定任务的完整详情，在同一只读事务中取得一致的数据。
func (s *Session) GetRun(ctx context.Context, runID string) (config.RunDetail, error) {
	var detail config.RunDetail
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		rec, err := tx.GetRun(ctx, runID, s.sessionID)
		if err != nil {
			return err
		}

		entries, err := tx.ListEntriesByRun(ctx, runID, s.sessionID)
		if err != nil {
			return err
		}
		if len(entries) == 0 || entries[0].ID != rec.InputEntryID || entries[len(entries)-1].ID != rec.LastEntryID {
			return fmt.Errorf("Run 的输入或末尾记录缺失、归属不符: %w", config.ErrSessionConflict)
		}

		toolExecs, err := tx.ListToolExecutionsByRun(ctx, runID, s.sessionID)
		if err != nil {
			return err
		}

		usageByOriginMap := make(map[config.MessageModelOrigin]*config.ModelUsage)
		var totalUsage llm.Usage

		for _, ent := range entries {
			msg, ok := ent.Message.(*llm.AssistantMessage)
			if !ok {
				continue
			}
			if ent.Origin == nil {
				return fmt.Errorf("assistant 记录 %s 缺少模型来源", ent.ID)
			}
			origin := *ent.Origin
			item := usageByOriginMap[origin]
			if item == nil {
				item = &config.ModelUsage{Origin: origin}
				usageByOriginMap[origin] = item
			}
			addUsage(&item.Usage, msg.Usage)
			addUsage(&totalUsage, msg.Usage)
		}

		var usageByOrigin []config.ModelUsage
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

		if usageByOrigin == nil {
			usageByOrigin = []config.ModelUsage{}
		}

		detail = config.RunDetail{
			Record:         rec,
			Entries:        entries,
			ToolExecutions: toolExecs,
			UsageByOrigin:  usageByOrigin,
			TotalUsage:     totalUsage,
		}
		return nil
	})
	if err != nil {
		return config.RunDetail{}, err
	}
	return detail, nil
}

// ListRuns 按输入 config.Entry 的 Sequence 倒序返回本会话的所有 config.RunRecord。
func (s *Session) ListRuns(ctx context.Context, limit, offset int) ([]config.RunRecord, error) {
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

	var records []config.RunRecord
	err := s.store.WithReadTx(ctx, func(tx *store.Tx) error {
		var err error
		records, err = tx.ListRuns(ctx, s.sessionID, limit, offset)
		return err
	})
	return records, err
}

// SwitchBranch 切换会话的历史分支至指定已完成 Run 的末尾。
func (s *Session) SwitchBranch(ctx context.Context, runID string) error {
	return s.store.WithTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if sess.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，无法切换分支: %w", s.sessionID, config.ErrSessionArchived)
		}

		activeRunID, err := tx.GetActiveRunID(ctx, s.sessionID)
		if err == nil {
			return fmt.Errorf("当前会话存在未结束任务 %s，无法切换分支: %w", activeRunID, config.ErrRunActive)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("检查未结束任务: %w", err)
		}

		lastEntryID, finishedAt, err := tx.GetRunLastEntryAndFinishedAt(ctx, runID, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("目标任务不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询目标任务: %w", err)
		}

		if finishedAt == nil {
			return fmt.Errorf("目标任务 %s 尚未结束，无法切换分支: %w", runID, config.ErrRunActive)
		}
		if lastEntryID == "" {
			return fmt.Errorf("目标任务 %s 没有有效的记录", runID)
		}
		last, err := tx.GetEntry(ctx, lastEntryID, s.sessionID)
		if err != nil {
			return fmt.Errorf("目标任务末尾记录无效: %w", err)
		}
		if last.RunID != runID {
			return fmt.Errorf("目标末尾记录不属于该任务: %w", config.ErrSessionConflict)
		}

		now := time.Now().UTC()
		return tx.UpdateSessionHead(ctx, s.sessionID, lastEntryID, now)
	})
}

// addUsage 汇总保存时的用量和费用，不重新计费。
func addUsage(dst *llm.Usage, u llm.Usage) {
	dst.Input += u.Input
	dst.Output += u.Output
	dst.Reasoning += u.Reasoning
	dst.CacheRead += u.CacheRead
	dst.CacheWrite += u.CacheWrite
	dst.Total += u.Total
	dst.Cost.Input += u.Cost.Input
	dst.Cost.Output += u.Cost.Output
	dst.Cost.CacheRead += u.Cost.CacheRead
	dst.Cost.CacheWrite += u.Cost.CacheWrite
	dst.Cost.Total += u.Cost.Total
}
