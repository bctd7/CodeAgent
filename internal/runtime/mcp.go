package runtime

import (
	"CodeAgent/internal/hooks"
	core "CodeAgent/internal/mcp"
	"CodeAgent/internal/permission"
	adapter "CodeAgent/internal/tools/extend/mcp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

func (r *Runtime) mcpBindings(bindings []ToolBinding) ([]ToolBinding, error) {
	manager := r.host.options.MCP
	if manager == nil || r.role != "" {
		return bindings, nil
	}
	definitions, err := manager.Definitions()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, binding := range bindings {
		names = append(names, binding.Tool.Name)
	}
	for _, definition := range definitions {
		adapted, err := adapter.New(manager, []core.Definition{definition}, names)
		if err != nil {
			return nil, err
		}
		for _, rejection := range adapted.Rejected {
			r.emit(Event{Diagnostic: fmt.Errorf("MCP %s/%s: %s", rejection.Key.ServerID, rejection.Key.Name, rejection.Reason)})
		}
		for _, tool := range adapted.Tools {
			bindings = append(bindings, ToolBinding{Tool: tool, Identity: permission.Tool{Source: permission.External, SourceID: fmt.Sprintf("%s/%d", definition.Key.ServerID, definition.Generation), ToolID: definition.Key.Name}, Capability: permission.Unknown, HookName: "mcp__" + definition.Key.ServerID + "__" + definition.Key.Name})
			names = append(names, tool.Name)
		}
	}
	return bindings, nil
}
func (r *Runtime) idle(ctx context.Context, fn func() error) error {
	return r.agent.WithExecutionBoundary(ctx, true, func() error {
		r.mu.Lock()
		busy := len(r.approvals) > 0 || r.submitting > 0
		r.mu.Unlock()
		if busy || r.hasActiveChildren() {
			return ErrBusy
		}
		return fn()
	})
}

// RefreshMCP 的旧 Agent 在原子空闲检查后关闭，Submit 被 operation 阻止；新 Agent 从 Session 恢复。
// Agent 没有替换工具目录入口，因此不在活跃实例中修改其工具或历史。
func (r *Runtime) RefreshMCP(ctx context.Context, serverID string, reconnect bool) error {
	if serverID != "" && r.host.options.MCPShared {
		return fmt.Errorf("共享 Manager 的刷新/重连由外部拥有者协调；Runtime 只能应用已有快照")
	}
	ctx, cancel := linkedContext(ctx, r.life)
	defer cancel()
	if err := lockAdmission(ctx, &r.host.catalogMu); err != nil {
		return err
	}
	defer r.host.catalogMu.Unlock()
	r.host.mu.Lock()
	if r.host.closing {
		r.host.mu.Unlock()
		return ErrClosed
	}
	for _, opened := range r.host.runtimes {
		if opened == nil {
			r.host.mu.Unlock()
			return ErrBusy
		}
	}
	r.host.catalogChanging = true
	r.host.mu.Unlock()
	defer func() { r.host.mu.Lock(); r.host.catalogChanging = false; r.host.mu.Unlock() }()
	opened := r.host.Opened()
	var locked []*Runtime
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].operation.Unlock()
		}
	}()
	for _, current := range opened {
		if err := lockAdmission(ctx, &current.operation); err != nil {
			return err
		}
		locked = append(locked, current)
	}
	if err := r.admission(); err != nil {
		return err
	}
	manager := r.host.options.MCP
	if manager == nil {
		return fmt.Errorf("MCP 未启用")
	}
	for _, current := range opened {
		if err := current.admission(); err != nil {
			return err
		}
		if err := current.idle(ctx, func() error { return nil }); err != nil {
			return err
		}
	}
	var refreshErr error
	if serverID != "" {
		if reconnect {
			refreshErr = manager.Reconnect(ctx, serverID)
		} else {
			refreshErr = manager.Refresh(ctx, serverID)
		}
	}
	var failures []error
	failures = append(failures, refreshErr)
	for _, current := range opened {
		old := current.agent
		old.RequestClose()
		failures = append(failures, old.Close())
		<-current.driverDone
		if err := current.assemble(ctx); err != nil {
			current.mu.Lock()
			current.fault = err
			current.mu.Unlock()
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func lockAdmission(ctx context.Context, mu *sync.Mutex) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// SetPolicy applies one project policy at the existing Host-wide idle boundary.
// Role restrictions remain narrower than the project policy.
func (r *Runtime) SetPolicy(ctx context.Context, policy permission.Policy) error {
	if policy.WorkMode != permission.Plan && policy.WorkMode != permission.Build {
		return fmt.Errorf("无效工作模式")
	}
	switch policy.ApprovalMode {
	case permission.Default, permission.AutoEdit, permission.AutoApprove:
	default:
		return fmt.Errorf("无效审批模式")
	}
	if policy.WorkingDir != r.host.options.ProjectDir {
		return fmt.Errorf("策略项目目录必须保持 Host 绑定")
	}
	if policy.PlanDir != "" {
		path, err := realPath(policy.PlanDir)
		if err != nil {
			return err
		}
		policy.PlanDir = path
	}
	policy.DeniedTools = append([]permission.Tool(nil), policy.DeniedTools...)
	ctx, cancel := linkedContext(ctx, r.life)
	defer cancel()
	h := r.host
	if err := lockAdmission(ctx, &h.catalogMu); err != nil {
		return err
	}
	defer h.catalogMu.Unlock()
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return ErrClosed
	}
	for _, current := range h.runtimes {
		if current == nil {
			h.mu.Unlock()
			return ErrBusy
		}
	}
	h.catalogChanging = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.catalogChanging = false; h.mu.Unlock() }()
	opened := h.Opened()
	var locked []*Runtime
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].operation.Unlock()
		}
	}()
	for _, current := range opened {
		if err := lockAdmission(ctx, &current.operation); err != nil {
			return err
		}
		locked = append(locked, current)
	}
	if err := r.admission(); err != nil {
		return err
	}
	for _, current := range opened {
		if err := current.admission(); err != nil {
			return err
		}
		if err := current.idle(ctx, func() error { return nil }); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return ErrClosed
	}
	h.options.Policy = policy
	for _, current := range opened {
		current.policy = policy
		if current.role == "explore" {
			current.policy.WorkMode = permission.Plan
		}
		current.permissions.Clear()
	}
	h.mu.Unlock()
	return nil
}
func (r *Runtime) SetHooks(ctx context.Context, engine *hooks.Engine, model *hooks.ModelHooks) error {
	r.operation.Lock()
	defer r.operation.Unlock()
	if err := r.admission(); err != nil {
		return err
	}
	return r.idle(ctx, func() error { r.hooks = engine; r.modelHooks = model; return nil })
}
