package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"

	"github.com/google/uuid"
)

// RecordToolStart 记录一次工具调用的启动事实，arguments 必须是最终执行参数。
// 调用期间借用只读参数；返回记录共享该参数 map，调用方不得修改。
// 按 AssistantEntryID + ToolCallID 定位，验证归属与未结束状态，检查防止重复启动或重复结果。
func (s *Session) RecordToolStart(ctx context.Context, assistantEntryID, toolCallID string, arguments map[string]any) (config.ToolExecution, error) {
	if assistantEntryID == "" {
		return config.ToolExecution{}, fmt.Errorf("assistantEntryID 不能为空")
	}
	if toolCallID == "" {
		return config.ToolExecution{}, fmt.Errorf("toolCallID 不能为空")
	}

	var te config.ToolExecution
	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, s.sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("会话不存在: %w", sql.ErrNoRows)
			}
			return fmt.Errorf("查询会话状态: %w", err)
		}
		if sess.ArchivedAt != nil {
			return fmt.Errorf("会话 %s 已归档，拒绝记录工具启动: %w", s.sessionID, config.ErrSessionArchived)
		}

		assistant, err := s.loadToolCall(ctx, tx, assistantEntryID, toolCallID)
		if err != nil {
			return err
		}
		runID := assistant.RunID

		_, finishedAt, err := tx.GetRunStepsAndFinishedAt(ctx, runID, s.sessionID)
		if err != nil {
			return fmt.Errorf("查询任务状态: %w", err)
		}
		if finishedAt != nil {
			return fmt.Errorf("所属任务 %s 已经结束，无法启动工具: %w", runID, config.ErrRunFinished)
		}

		// 检查该调用是否已保存过结果
		hasResult, err := tx.ToolResultEntryExists(ctx, assistantEntryID, toolCallID)
		if err != nil {
			return fmt.Errorf("检查工具结果存在性: %w", err)
		}
		if hasResult {
			return fmt.Errorf("工具调用 (%s, %s) 已经保存过结果，无法记录启动: %w", assistantEntryID, toolCallID, config.ErrToolResultConflict)
		}

		// 检查是否已存在开始记录
		hasStarted, err := tx.ToolExecutionExists(ctx, assistantEntryID, toolCallID)
		if err != nil {
			return fmt.Errorf("检查工具调用存在性: %w", err)
		}
		if hasStarted {
			return fmt.Errorf("工具调用 (%s, %s) 已经存在开始记录: %w", assistantEntryID, toolCallID, config.ErrToolResultConflict)
		}

		now := time.Now().UTC()
		te = config.ToolExecution{
			SessionID:        s.sessionID,
			RunID:            runID,
			AssistantEntryID: assistantEntryID,
			ToolCallID:       toolCallID,
			Arguments:        &arguments,
			StartedAt:        &now,
			Status:           config.ToolStarted,
			FinishedAt:       nil,
			ResultEntryID:    "",
		}

		return tx.InsertToolExecution(ctx, te)
	})
	if err != nil {
		return config.ToolExecution{}, err
	}

	return te, nil
}

// AppendToolResult 追加工具执行结果消息。
// 调用期间借用只读消息；返回 Entry 共享该消息，调用方不得修改。
// 验证调用配对、会话和 Run 归属及未结束状态，禁止重复结果。
func (s *Session) AppendToolResult(ctx context.Context, assistantEntryID string, input config.ToolResultInput) (config.Entry, error) {
	resultMsg := input.Result
	if err := validateToolResult(input); err != nil {
		return config.Entry{}, err
	}
	if assistantEntryID == "" {
		return config.Entry{}, fmt.Errorf("assistantEntryID 不能为空")
	}
	if resultMsg == nil {
		return config.Entry{}, fmt.Errorf("ToolResultMessage 不能为空")
	}
	if resultMsg.Role != llm.RoleTool {
		return config.Entry{}, fmt.Errorf("ToolResultMessage 角色应为 %q，实际为 %q", llm.RoleTool, resultMsg.Role)
	}
	if resultMsg.ToolCallID == "" {
		return config.Entry{}, fmt.Errorf("ToolResultMessage.ToolCallID 不能为空")
	}

	msgData, err := config.EncodeMessage(resultMsg)
	if err != nil {
		return config.Entry{}, fmt.Errorf("编码工具结果消息: %w", err)
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
			return fmt.Errorf("会话 %s 已归档，拒绝追加工具结果: %w", s.sessionID, config.ErrSessionArchived)
		}

		assistant, err := s.loadToolCall(ctx, tx, assistantEntryID, resultMsg.ToolCallID)
		if err != nil {
			return err
		}
		for _, call := range assistant.Message.(*llm.AssistantMessage).Content {
			if call.Type == llm.TypeToolCall && call.ID == resultMsg.ToolCallID && call.Name != resultMsg.ToolName {
				return fmt.Errorf("工具结果名称与所属调用不一致: %w", config.ErrToolResultConflict)
			}
		}
		runID := assistant.RunID

		_, finishedAt, err := tx.GetRunStepsAndFinishedAt(ctx, runID, s.sessionID)
		if err != nil {
			return fmt.Errorf("查询任务状态: %w", err)
		}
		if finishedAt != nil {
			return fmt.Errorf("所属任务 %s 已经结束，无法追加工具结果: %w", runID, config.ErrRunFinished)
		}

		// 检查 entries 是否已存在该工具结果
		hasResult, err := tx.ToolResultEntryExists(ctx, assistantEntryID, resultMsg.ToolCallID)
		if err != nil {
			return fmt.Errorf("查询工具结果存在性: %w", err)
		}
		if hasResult {
			return fmt.Errorf("工具调用 (%s, %s) 已经保存过结果，禁止重复保存: %w", assistantEntryID, resultMsg.ToolCallID, config.ErrToolResultConflict)
		}

		// 检查 tool_executions 记录
		hasStartRecord, existExecResID, err := tx.GetToolExecutionResultEntry(ctx, assistantEntryID, resultMsg.ToolCallID)
		if err != nil {
			return fmt.Errorf("查询工具执行记录: %w", err)
		}
		if hasStartRecord && existExecResID != "" {
			return fmt.Errorf("工具调用 (%s, %s) 已经保存过结果，禁止重复保存: %w", assistantEntryID, resultMsg.ToolCallID, config.ErrToolResultConflict)
		}

		run, err := tx.GetRun(ctx, runID, s.sessionID)
		if err != nil {
			return err
		}
		if sess.HeadID == "" || sess.HeadID != run.LastEntryID {
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
			ID:               entryID,
			SessionID:        s.sessionID,
			ParentID:         sess.HeadID,
			Sequence:         nextSeq,
			RunID:            runID,
			CreatedAt:        now,
			Message:          resultMsg,
			Kind:             config.MessageEntry,
			Processing:       config.ProcessingPending,
			Origin:           nil,
			AssistantEntryID: assistantEntryID,
			ToolCallID:       resultMsg.ToolCallID,
		}

		if err := tx.InsertEntry(ctx, entry, string(msgData), nil); err != nil {
			return fmt.Errorf("写入工具结果 config.Entry: %w", err)
		}

		if input.Executed && !hasStartRecord {
			return fmt.Errorf("已执行结果缺少启动记录: %w", config.ErrSessionConflict)
		}
		if !hasStartRecord {
			args := input.Arguments
			te := config.ToolExecution{SessionID: s.sessionID, RunID: runID, AssistantEntryID: assistantEntryID, ToolCallID: resultMsg.ToolCallID, Arguments: &args, Status: input.Status}
			if err := tx.InsertToolExecution(ctx, te); err != nil {
				return err
			}
		} else if err := tx.MatchToolArguments(ctx, assistantEntryID, resultMsg.ToolCallID, input.Arguments); err != nil {
			return err
		}
		if err := tx.UpdateToolExecutionResult(ctx, assistantEntryID, resultMsg.ToolCallID, entryID, now, input); err != nil {
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

// loadToolCall 在当前事务内验证 assistant 归属及调用 ID 的唯一性。
func (s *Session) loadToolCall(ctx context.Context, tx *store.Tx, assistantEntryID, toolCallID string) (config.Entry, error) {
	assistant, err := tx.GetEntry(ctx, assistantEntryID, s.sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return config.Entry{}, fmt.Errorf("未找到对应的 assistant 记录: %w", sql.ErrNoRows)
		}
		return config.Entry{}, fmt.Errorf("查询 assistant 记录: %w", err)
	}

	asstMsg, ok := assistant.Message.(*llm.AssistantMessage)
	if !ok {
		return config.Entry{}, fmt.Errorf("指定的记录 %s 不是 assistant 消息", assistantEntryID)
	}

	foundCount := 0
	for _, b := range asstMsg.Content {
		if b.Type == llm.TypeToolCall && b.ID == toolCallID {
			foundCount++
		}
	}
	if foundCount == 0 {
		return config.Entry{}, fmt.Errorf("assistant 消息中未找到 ToolCallID %q: %w", toolCallID, config.ErrToolCallNotFound)
	}
	if foundCount > 1 {
		return config.Entry{}, fmt.Errorf("assistant 消息中存在重复的 ToolCallID %q", toolCallID)
	}

	return assistant, nil
}

func validateToolResult(input config.ToolResultInput) error {
	if input.Result == nil {
		return fmt.Errorf("tool result required")
	}
	switch input.Status {
	case config.ToolSucceeded, config.ToolFailed:
		if !input.Executed {
			return fmt.Errorf("成功或失败执行结果必须标明已执行")
		}
	case config.ToolDenied, config.ToolUnavailable, config.ToolSkipped, config.ToolDeferred:
		if input.Executed {
			return fmt.Errorf("未执行状态不能标明已执行")
		}
	case config.ToolCanceled:
	default:
		return fmt.Errorf("非法工具终态 %q", input.Status)
	}
	if (input.Status == config.ToolSucceeded) == input.Result.IsError {
		return fmt.Errorf("工具终态与 IsError 不一致")
	}
	return nil
}
