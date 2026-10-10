// 本文件负责原生 Go 模型请求和响应回调。
package hooks

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"CodeAgent/internal/llm"
)

// RequestContext 仅包含请求上下文，不包含模型选项或凭据。
// 输入、回调返回的替换值及共享的嵌套数据均按只读约定使用。
type RequestContext struct {
	SystemPrompt string
	// Messages 及其嵌套数据按只读约定共享；转换回调返回替换值。
	Messages []llm.Message
}

// ModelRequest 携带请求上下文及宿主提供的会话、运行和步骤标识。
type ModelRequest struct {
	SessionID, RunID string
	StepIndex        int
	Context          RequestContext
}

// ModelResponse 携带模型响应事实，供观察回调读取。
type ModelResponse struct {
	SessionID, RunID string
	StepIndex        int
	Response         llm.AssistantMessage
}

// BeforeModelRequestHook 可返回替换上下文；返回 nil 表示保持当前上下文。
type BeforeModelRequestHook func(context.Context, ModelRequest) (*RequestContext, error)

// AfterModelResponseHook 观察响应，错误由调用方汇总返回给宿主。
type AfterModelResponseHook func(context.Context, ModelResponse) error

// ModelHooks 提供独立的原生 Go 回调，不构造额外的 Claude 脚本事件。
// 请求转换回调串行执行；响应观察回调只观察结果，不改写事实。
type ModelHooks struct {
	// 锁保护注册列表；执行时复制列表，释放锁后才调用用户回调。
	mu     sync.RWMutex
	before []namedBefore
	after  []namedAfter
}
type namedBefore struct {
	name string
	fn   BeforeModelRequestHook
}
type namedAfter struct {
	name string
	fn   AfterModelResponseHook
}

// RegisterBeforeModelRequest 注册具名请求回调，拒绝空名称、空函数和同类重名。
func (r *ModelHooks) RegisterBeforeModelRequest(name string, fn BeforeModelRequestHook) error {
	if r == nil || name == "" || fn == nil {
		return fmt.Errorf("%w: hook name and function required", ErrConfiguration)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.before {
		if h.name == name {
			return fmt.Errorf("%w: duplicate model hook %s", ErrConfiguration, name)
		}
	}
	r.before = append(r.before, namedBefore{name, fn})
	return nil
}

// RegisterAfterModelResponse 注册具名响应观察回调，拒绝空名称、空函数和同类重名。
func (r *ModelHooks) RegisterAfterModelResponse(name string, fn AfterModelResponseHook) error {
	if r == nil || name == "" || fn == nil {
		return fmt.Errorf("%w: hook name and function required", ErrConfiguration)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.after {
		if h.name == name {
			return fmt.Errorf("%w: duplicate model hook %s", ErrConfiguration, name)
		}
	}
	r.after = append(r.after, namedAfter{name, fn})
	return nil
}

// BeforeModelRequest 快照注册列表后串行转换请求，首个错误终止后续转换。
func (r *ModelHooks) BeforeModelRequest(ctx context.Context, in ModelRequest) (*RequestContext, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	handlers := append([]namedBefore(nil), r.before...)
	r.mu.RUnlock()
	var result *RequestContext
	for _, h := range handlers {
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			next, err := h.fn(ctx, in)
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if next != nil {
				value := *next
				value.Messages = append([]llm.Message(nil), next.Messages...)
				in.Context = value
				result = &value
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("model hook %s: %w", h.name, err)
		}
	}
	return result, nil
}

// AfterModelResponse 按注册顺序观察响应，普通错误不阻止后续回调，取消时停止。
// 每个错误携带回调名称；汇总错误保留 errors.Is/As 所需的错误链。
func (r *ModelHooks) AfterModelResponse(ctx context.Context, in ModelResponse) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInput)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	r.mu.RLock()
	handlers := append([]namedAfter(nil), r.after...)
	r.mu.RUnlock()
	var failures []error
	for _, h := range handlers {
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := h.fn(ctx, in)
			return errors.Join(err, ctx.Err())
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("model hook %s: %w", h.name, err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

// protected 将回调 panic 转换为错误，避免穿透宿主调用边界。
func protected(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("hook panic: %v", p)
		}
	}()
	return fn()
}
