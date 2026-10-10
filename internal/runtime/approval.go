package runtime

import (
	"CodeAgent/internal/agent"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/permission"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
)

// Approval 是展示快照，含可信身份与完整最终参数，不含 Confirmation 私有凭据。
type Approval struct {
	ID, RunID, ToolName, Arguments string
	Tool                           permission.Tool
	FileTargets                    []string
	HookAsk                        bool
	Reason                         permission.Reason
}
type pendingApproval struct {
	display      Approval
	request      permission.Request
	confirmation *permission.Confirmation
	ctx          context.Context
	wake         chan struct{}
	hookApproved bool
	replied      bool
	rejected     bool
}

func (p *pendingApproval) view() Approval {
	v := p.display
	v.FileTargets = append([]string(nil), v.FileTargets...)
	return v
}
func signal(p *pendingApproval) {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Approve 只能代表宿主取得的真实用户选择；授权成功后由原等待者重新 Evaluate。
func (r *Runtime) Approve(id string, choice permission.Choice) error {
	if choice != permission.Once && choice != permission.SessionTool && choice != permission.Reject {
		return permission.ErrInvalidChoice
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.approvals[id]
	if p == nil || p.replied || p.ctx.Err() != nil || r.closing || r.fault != nil {
		return ErrStaleApproval
	}
	if p.confirmation != nil {
		if err := r.permissions.Grant(*p.confirmation, choice); err != nil {
			return err
		}
		p.confirmation = nil
	}
	p.replied = true
	p.rejected = choice == permission.Reject
	p.hookApproved = !p.rejected
	signal(p)
	if choice == permission.SessionTool {
		for _, other := range r.approvals {
			if other != p && other.request.Tool == p.request.Tool {
				signal(other)
			}
		}
	}
	return nil
}
func (r *Runtime) Revoke(tool permission.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.permissions.Revoke(tool)
	for id, p := range r.approvals {
		if p.request.Tool == tool {
			p.rejected = true
			p.replied = true
			signal(p)
			delete(r.approvals, id)
		}
	}
}

func (r *Runtime) awaitPermission(c agent.Checkpoint, binding *toolBinding, args map[string]any, paths []string, forceAsk bool) (bool, string, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return false, "", err
	}
	request := permission.Request{RequestID: uuid.NewString(), SessionID: r.id, Tool: binding.Identity, Capability: binding.Capability, Arguments: string(encoded), FileTargets: paths}
	defer r.permissions.Discard(request.RequestID)
	// 与 Grant/Revoke 共用边界，避免 Evaluate 到登记等待之间漏掉会话授权唤醒。
	r.mu.Lock()
	if err := c.Context.Err(); err != nil {
		r.mu.Unlock()
		return false, "", err
	}
	if r.closing || r.fault != nil {
		r.mu.Unlock()
		return false, "", ErrClosed
	}
	result := r.permissions.Evaluate(r.policy, request)
	if result.Decision == permission.Deny {
		r.mu.Unlock()
		return false, string(result.Reason), nil
	}
	if result.Decision == permission.Allow && !forceAsk {
		r.mu.Unlock()
		return true, "", nil
	}
	p := &pendingApproval{display: Approval{ID: request.RequestID, RunID: c.RunID, ToolName: binding.Tool.Name, Arguments: string(encoded), Tool: request.Tool, FileTargets: paths, HookAsk: forceAsk, Reason: result.Reason}, request: request, confirmation: result.Confirmation, ctx: c.Context, wake: make(chan struct{}, 1)}
	r.approvals[p.display.ID] = p
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.approvals, p.display.ID); r.mu.Unlock() }()
	view := p.view()
	r.emit(Event{Approval: &view})
	for {
		select {
		case <-c.Context.Done():
			return false, "", c.Context.Err()
		case <-r.life.Done():
			return false, "", ErrClosed
		case <-p.wake:
		}
		r.mu.Lock()
		rejected, approved := p.rejected, p.hookApproved
		if err := c.Context.Err(); err != nil {
			r.mu.Unlock()
			return false, "", err
		}
		if rejected {
			r.mu.Unlock()
			return false, "用户拒绝", nil
		}
		// Hook Ask 尚未满足时不消耗 Once；保持同一版参数及请求身份。
		if forceAsk && !approved {
			r.mu.Unlock()
			continue
		}
		result = r.permissions.Evaluate(r.policy, request)
		if result.Decision == permission.Allow {
			delete(r.approvals, p.display.ID)
			r.mu.Unlock()
			return true, "", nil
		}
		if result.Decision == permission.Deny {
			delete(r.approvals, p.display.ID)
			r.mu.Unlock()
			return false, string(result.Reason), nil
		}
		p.confirmation = result.Confirmation
		p.replied = false
		r.mu.Unlock()
		view = p.view()
		r.emit(Event{Approval: &view})
	}
}

func toolInput(binding *toolBinding, callID string, args map[string]any) (hooks.ToolInput, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return hooks.ToolInput{}, err
	}
	name := binding.HookName
	if name == "" {
		name = binding.Tool.Name
	}
	return hooks.ToolInput{IsMCP: binding.Identity.Source == permission.External, ToolName: name, ToolUseID: callID, ToolInput: raw}, nil
}

func (r *Runtime) prepareTool(c agent.Checkpoint, assistant string) (decision agent.Decision) {
	decision = agent.Decision{Kind: agent.Continue}
	defer func() {
		if value := recover(); value != nil {
			decision = agent.Decision{Kind: agent.Stop, Error: fmt.Errorf("工具交接 panic: %v", value)}
		}
	}()
	binding := r.bindings[c.ToolCall.Name]
	if binding == nil {
		return agent.Decision{Kind: agent.Stop, Error: fmt.Errorf("未知工具绑定")}
	}
	args := c.ToolCall.Arguments
	forceAsk := false
	if r.hooks != nil {
		input, err := toolInput(binding, c.ToolCall.ID, args)
		if err != nil {
			return agent.Decision{Kind: agent.Stop, Error: err}
		}
		out, err := r.hooks.PreToolUse(c.Context, hooks.PreToolUseInput{CommonInput: r.common(), ToolInput: input})
		r.emit(Event{Notices: &out.Notices})
		if err = hookError(out.Notices, err); err != nil {
			return agent.Decision{Kind: agent.Stop, Error: err}
		}
		if out.UpdatedInput != nil {
			var rewritten map[string]any
			if err = json.Unmarshal(*out.UpdatedInput, &rewritten); err != nil || rewritten == nil {
				return agent.Decision{Kind: agent.Stop, Error: fmt.Errorf("Hook 最终参数必须是 JSON 对象")}
			}
			args = rewritten
		}
		additions := contexts("hook_pre_tool", fmt.Sprint(c.ID), out.AdditionalContext)
		r.mu.Lock()
		r.feedback[c.RunID] = append(r.feedback[c.RunID], feedbacks(additions)...)
		r.mu.Unlock()
		reason := fmt.Sprint(out.Reasons)
		if out.StopProcessing {
			return agent.Decision{Kind: agent.Stop, Error: fmt.Errorf("PreToolUse 停止: %v", out.StopReasons)}
		}
		if out.Permission == hooks.Deny {
			return agent.Decision{Kind: agent.Skip, Disposition: agent.ToolDenied, Reason: reason, ToolArguments: &args}
		}
		if out.Permission == hooks.Defer {
			return agent.Decision{Kind: agent.DeferRun, Reason: reason, ToolArguments: &args}
		}
		forceAsk = out.Permission == hooks.Ask
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := binding.schema.Validate(args); err != nil {
		return agent.Decision{Kind: agent.Skip, Disposition: agent.ToolDenied, Reason: fmt.Sprintf("最终参数 schema: %v", err), ToolArguments: &args}
	}
	paths, err := r.targets(binding, args)
	if err != nil {
		return agent.Decision{Kind: agent.Skip, Disposition: agent.ToolDenied, Reason: err.Error(), ToolArguments: &args}
	}
	allowed, reason, err := r.awaitPermission(c, binding, args, paths, forceAsk)
	if err != nil {
		return agent.Decision{Kind: agent.Skip, Disposition: agent.ToolCanceled, Reason: err.Error(), ToolArguments: &args}
	}
	if !allowed {
		return agent.Decision{Kind: agent.Skip, Disposition: agent.ToolDenied, Reason: reason, ToolArguments: &args}
	}
	if err = c.Context.Err(); err != nil {
		return agent.Decision{Kind: agent.Skip, Disposition: agent.ToolCanceled, Reason: err.Error(), ToolArguments: &args}
	}
	if binding.Capability == permission.FileWrite || r.role == "worker" && binding.Capability == permission.Unknown {
		if err = r.host.claim(c.Context, r.claimOwner(c.RunID), paths, r.parentOwner); err != nil {
			status := agent.ToolDenied
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = agent.ToolCanceled
			}
			return agent.Decision{Kind: agent.Skip, Disposition: status, Reason: err.Error(), ToolArguments: &args}
		}
	}
	ctx, cancel := r.recordContext()
	_, err = r.session.RecordToolStart(ctx, assistant, c.ToolCall.ID, args)
	cancel()
	if err != nil {
		r.persistFault(err)
		return agent.Decision{Kind: agent.Stop, Error: err}
	}
	return agent.Decision{Kind: agent.Continue, ToolArguments: &args}
}
