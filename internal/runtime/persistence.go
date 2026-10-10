package runtime

import (
	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	sc "CodeAgent/internal/session/config"
	"context"
	"fmt"
)

// 仅保存当前 Run 的节点身份关联，不镜像持久消息或工具结果。
type runRecords struct {
	responses  map[agent.CheckpointID]string
	model      ModelConfig
	stopActive bool
}
type feedback struct{ input sc.ContextInput }

func (r *Runtime) startRun(c agent.Checkpoint) error {
	ctx, cancel := r.recordContext()
	defer cancel()
	if _, err := r.session.StartRun(ctx, c.RunID, c.UserMessage); err != nil {
		r.persistFault(err)
		return err
	}
	r.records[c.RunID] = &runRecords{responses: map[agent.CheckpointID]string{}, model: r.host.options.Models[r.modelRef]}
	r.mu.Lock()
	additions := append(r.initial, r.inputs[c.UserMessage]...)
	r.initial = nil
	delete(r.inputs, c.UserMessage)
	r.mu.Unlock()
	if _, err := r.saveContexts(ctx, c.RunID, additions); err != nil {
		r.persistFault(err)
		return err
	}
	if r.role == "worker" && len(r.scope) > 0 {
		return r.host.claim(c.Context, r.claimOwner(c.RunID), r.scope, r.parentOwner)
	}
	return nil
}
func feedbacks(inputs []sc.ContextInput) []feedback {
	out := make([]feedback, len(inputs))
	for i, input := range inputs {
		out[i] = feedback{input}
	}
	return out
}
func (r *Runtime) saveContexts(ctx context.Context, runID string, inputs []sc.ContextInput) ([]llm.Message, error) {
	var messages []llm.Message
	for _, input := range inputs {
		if _, err := r.session.AppendContext(ctx, runID, input); err != nil {
			return nil, err
		}
		messages = append(messages, input.Message)
	}
	return messages, nil
}
func (r *Runtime) saveResponse(c agent.Checkpoint) error {
	if c.Response == nil || c.Origin == nil {
		return fmt.Errorf("模型响应节点缺少实际来源")
	}
	ctx, cancel := r.recordContext()
	defer cancel()
	o := c.Origin
	entry, err := r.session.AppendMessage(ctx, c.RunID, c.Response, &sc.MessageModelOrigin{Provider: o.Provider, API: o.API, ModelID: o.ModelID, BaseURL: o.BaseURL})
	if err != nil {
		return err
	}
	records := r.records[c.RunID]
	if records == nil {
		return fmt.Errorf("模型响应缺少已开始的 Run")
	}
	records.responses[c.ID] = entry.ID
	return nil
}
func (r *Runtime) assistantID(c agent.Checkpoint) (string, error) {
	records := r.records[c.RunID]
	if records == nil || records.responses[c.ResponseID] == "" {
		return "", fmt.Errorf("工具节点缺少已保存的响应关联: %d", c.ResponseID)
	}
	return records.responses[c.ResponseID], nil
}
func (r *Runtime) claimOwner(runID string) string { return r.id + "/" + runID }
func (r *Runtime) finishRun(c agent.Checkpoint) error {
	if c.Result == nil {
		return fmt.Errorf("结束节点没有终态")
	}
	r.cancelRunChildren(c.RunID)
	ctx, cancel := r.recordContext()
	defer cancel()
	result := c.Result
	_, err := r.session.FinishRun(ctx, c.RunID, sc.RunCompletion{State: sc.RunState(result.State), StopReason: llm.StopReason(result.StopReason), RawStopReason: result.RawStopReason, ErrorMessage: result.ErrorMessage})
	claimErr := r.host.releaseClaims(r.claimOwner(c.RunID))
	if claimErr != nil {
		r.emit(Event{Diagnostic: claimErr})
	}
	records := r.records[c.RunID]
	delete(r.records, c.RunID)
	r.mu.Lock()
	delete(r.feedback, c.RunID)
	r.mu.Unlock()
	if err == nil && records != nil && r.role == "" && result.State == agent.RunStateSuccess && result.StopReason == agent.RunStopCompleted {
		r.triggerMemory(records.model)
	}
	return err
}
