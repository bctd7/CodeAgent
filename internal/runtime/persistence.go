package runtime

import (
	"context"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	sessionconfig "CodeAgent/internal/session/config"
)

// recorder 在执行路径同步保存，不依赖展示事件。每次失败立即关闭任务准入。
type recorder struct{ runtime *Runtime }

var _ agent.Recorder = (*recorder)(nil)

func (p *recorder) checked(err error) error {
	if err != nil {
		p.runtime.persistenceFault(err)
	}
	return err
}

func (p *recorder) StartRun(ctx context.Context, id string, msg *llm.UserMessage) error {
	_, err := p.runtime.session.StartRun(ctx, id, msg)
	if err != nil {
		return p.checked(err)
	}
	// 标题是辅助信息；保存失败不改变已经提交的任务。
	for _, b := range msg.Content {
		if b.Type != llm.TypeText || strings.TrimSpace(b.Text) == "" {
			continue
		}
		title := []rune(strings.TrimSpace(b.Text))
		if len(title) > 80 {
			title = title[:80]
		}
		_, _ = p.runtime.manager.service.SetTitleIfEmpty(ctx, p.runtime.SessionID(), string(title))
		break
	}
	return nil
}
func (p *recorder) RecordStep(ctx context.Context, id string) error {
	_, err := p.runtime.session.RecordStep(ctx, id)
	return p.checked(err)
}
func (p *recorder) AppendMessage(ctx context.Context, id string, msg llm.Message, origin *agent.ModelOrigin) (string, error) {
	var source *sessionconfig.MessageModelOrigin
	if origin != nil {
		source = &sessionconfig.MessageModelOrigin{Provider: origin.Provider, API: origin.API, ModelID: origin.ModelID, BaseURL: origin.BaseURL}
	}
	entry, err := p.runtime.session.AppendMessage(ctx, id, msg, source)
	return entry.ID, p.checked(err)
}
func (p *recorder) RecordToolStart(ctx context.Context, assistantID, callID string) error {
	_, err := p.runtime.session.RecordToolStart(ctx, assistantID, callID)
	return p.checked(err)
}
func (p *recorder) AppendToolResult(ctx context.Context, assistantID string, msg *llm.ToolResultMessage) error {
	_, err := p.runtime.session.AppendToolResult(ctx, assistantID, msg)
	return p.checked(err)
}
func (p *recorder) FinishRun(ctx context.Context, result agent.RunResult) error {
	_, err := p.runtime.session.FinishRun(ctx, result.RunID, sessionconfig.RunCompletion{
		State: sessionconfig.RunState(result.State), StopReason: llm.StopReason(result.StopReason), RawStopReason: result.RawStopReason, ErrorMessage: result.ErrorMessage,
	})
	return p.checked(err)
}
