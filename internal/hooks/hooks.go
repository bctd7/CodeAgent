// Package hooks 提供 Runtime 使用的小型业务扩展点，不负责持久化或执行调度。
package hooks

import (
	"context"
	"fmt"
	"log"
	"sync"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

// Hook 输入保持只读，不包含数据库连接、凭据或可变 Agent。
type Completion struct {
	SessionID string
	Result    agent.RunResult
}
type ToolRequest struct {
	SessionID, RunID string
	StepIndex        int
	Call             llm.ContentBlock
}

// 空 Reason 允许执行；非空 Reason 拒绝并形成未执行工具结果。
type ToolDecision struct{ Reason string }
type CompletionHook func(context.Context, Completion) error
type BeforeToolHook func(context.Context, ToolRequest) (ToolDecision, error)
type Failure struct {
	Point, SessionID, RunID string
	Error                   error
}

// Registry 可在多个会话间共享；用户回调必须响应 ctx 且快速返回。
// Hook 本身的共享状态由其实现同步，例如示例中的 atomic.Int64。
type Registry struct {
	mu         sync.Mutex
	completed  []CompletionHook
	beforeTool []BeforeToolHook
	onError    func(Failure)
}

func New(onError func(Failure)) *Registry { return &Registry{onError: onError} }
func (r *Registry) RegisterCompletion(fn CompletionHook) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.completed = append(r.completed, fn)
	r.mu.Unlock()
}
func (r *Registry) RegisterBeforeTool(fn BeforeToolHook) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.beforeTool = append(r.beforeTool, fn)
	r.mu.Unlock()
}

// Completed 在终态提交成功后调用。异常可观察，不改写已提交的 Run。
func (r *Registry) Completed(ctx context.Context, event Completion) {
	if r == nil {
		return
	}
	r.mu.Lock()
	handlers := append([]CompletionHook(nil), r.completed...)
	r.mu.Unlock()
	for _, handler := range handlers {
		err := protected(func() error { return handler(ctx, event) })
		if err != nil {
			r.report(Failure{"completion", event.SessionID, event.Result.RunID, err})
		}
	}
}

// BeforeTool 按注册顺序执行，首次拒绝即停止。异常按拒绝处理。
func (r *Registry) BeforeTool(ctx context.Context, request ToolRequest) ToolDecision {
	if r == nil {
		return ToolDecision{}
	}
	r.mu.Lock()
	handlers := append([]BeforeToolHook(nil), r.beforeTool...)
	r.mu.Unlock()
	for _, handler := range handlers {
		var decision ToolDecision
		err := protected(func() error { var err error; decision, err = handler(ctx, request); return err })
		if err != nil {
			r.report(Failure{"before_tool", request.SessionID, request.RunID, err})
			return ToolDecision{Reason: fmt.Sprintf("工具 Hook 拒绝: %v", err)}
		}
		if decision.Reason != "" {
			return decision
		}
	}
	return ToolDecision{}
}

func protected(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Hook panic: %v", p)
		}
	}()
	return fn()
}
func (r *Registry) report(failure Failure) {
	if r.onError == nil {
		log.Printf("Hook %s (%s): %v", failure.Point, failure.RunID, failure.Error)
		return
	}
	if err := protected(func() error { r.onError(failure); return nil }); err != nil {
		log.Printf("Hook 错误通知失败: %v", err)
	}
}
