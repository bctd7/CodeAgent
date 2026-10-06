// hooks 包提供 Runtime 使用的小型业务扩展点，不负责持久化或执行调度。
package hooks

import (
	"context"
	"fmt"
	"log"
	"maps"

	"CodeAgent/internal/llm"
)

// BeforeModelRequest 依次转换本次请求上下文，后续回调看到前序返回的替换。
// 回调或复制失败时报告并返回错误，由 Runtime 停止本轮请求。
func (r *Registry) BeforeModelRequest(ctx context.Context, request ModelRequest) (*llm.RequestContext, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	handlers := append([]BeforeModelRequestHook(nil), r.beforeModelRequest...)
	r.mu.Unlock()
	var final *llm.RequestContext
	for _, handler := range handlers {
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			input := request
			var err error
			input.Context, err = request.Context.Clone()
			if err != nil {
				return err
			}
			next, err := handler(ctx, input)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if next != nil {
				copy, err := next.Clone()
				if err != nil {
					return err
				}
				request.Context, final = copy, &copy
			}
			return nil
		})
		if err != nil {
			r.report(Failure{"before_model_request", request.SessionID, request.RunID, err})
			return nil, err
		}
	}
	return final, nil
}

// AfterModelResponse 只观察完整响应；各回调得到独立副本。
// 观察错误仅报告，不改写响应。
func (r *Registry) AfterModelResponse(ctx context.Context, response ModelResponse) {
	if r == nil {
		return
	}
	r.mu.Lock()
	handlers := append([]AfterModelResponseHook(nil), r.afterModelResponse...)
	r.mu.Unlock()
	for _, handler := range handlers {
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			copy, err := llm.CloneMessage(&response.Response)
			if err != nil {
				return err
			}
			input := response
			input.Response = *copy.(*llm.AssistantMessage)
			if err := handler(ctx, input); err != nil {
				return err
			}
			return ctx.Err()
		})
		if err != nil {
			r.report(Failure{"after_model_response", response.SessionID, response.RunID, err})
		}
	}
}

// AfterTool 依次转换模型可见的工具结果内容，后续回调看到前序替换。
// 转换失败时报告并返回错误，由 Runtime 保留原始结果、清空模型可见内容并结束本轮。
func (r *Registry) AfterTool(ctx context.Context, response ToolResponse) (*[]llm.ContentBlock, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	handlers := append([]AfterToolHook(nil), r.afterTool...)
	r.mu.Unlock()
	var final *[]llm.ContentBlock
	for _, handler := range handlers {
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			copy, err := llm.CloneMessage(&response.Result)
			if err != nil {
				return err
			}
			input := response
			input.Result = *copy.(*llm.ToolResultMessage)
			next, err := handler(ctx, input)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if next != nil {
				blocks, err := llm.CloneContent(*next)
				if err != nil {
					return err
				}
				if blocks == nil {
					blocks = []llm.ContentBlock{}
				}
				response.Result.Content, final = blocks, &blocks
			}
			return nil
		})
		if err != nil {
			r.report(Failure{"after_tool", response.SessionID, response.RunID, err})
			return nil, err
		}
	}
	return final, nil
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
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			copy := event
			messages, err := (llm.RequestContext{Messages: event.Result.Messages}).Clone()
			if err != nil {
				return err
			}
			copy.Result.Messages = messages.Messages
			copy.Result.Usage = maps.Clone(event.Result.Usage)
			return handler(ctx, copy)
		})
		if err == nil {
			err = ctx.Err()
		}
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
	var final ToolDecision
	for _, handler := range handlers {
		var decision ToolDecision
		err := protected(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			input := request
			args, err := llm.CloneArguments(request.Call.Arguments)
			if err != nil {
				return err
			}
			input.Call.Arguments = args
			decision, err = handler(ctx, input)
			if err != nil {
				return err
			}
			if decision.Arguments != nil && decision.Reason == "" {
				args, err = llm.CloneArguments(*decision.Arguments)
				if err != nil {
					return err
				}
				request.Call.Arguments, final.Arguments = args, &args
			}
			return nil
		})
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			r.report(Failure{"before_tool", request.SessionID, request.RunID, err})
			return ToolDecision{Reason: fmt.Sprintf("工具 Hook 拒绝: %v", err)}
		}
		if decision.Reason != "" {
			return ToolDecision{Reason: decision.Reason}
		}
	}
	return final
}

// protected 将回调的 panic 转成错误，交给对应调用点处理。
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
