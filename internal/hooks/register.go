package hooks

import "sync"

// Registry 可在多个会话间共享；用户回调必须响应 ctx 且快速返回。
// Hook 本身的共享状态由其实现同步，例如示例中的 atomic.Int64。
type Registry struct {
	mu                 sync.Mutex
	completed          []CompletionHook
	beforeTool         []BeforeToolHook
	beforeModelRequest []BeforeModelRequestHook
	afterModelResponse []AfterModelResponseHook
	afterTool          []AfterToolHook
	onError            func(Failure)
}

func New(onError func(Failure)) *Registry { return &Registry{onError: onError} }

// 所有回调应在 Registry 交给 Host 前注册，调用时按各自的注册顺序处理。
// 不同工具或会话的回调可能并发运行；回调应配合 ctx 取消并自行保护共享状态。
func (r *Registry) RegisterBeforeModelRequest(fn BeforeModelRequestHook) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforeModelRequest = append(r.beforeModelRequest, fn)
}
func (r *Registry) RegisterAfterModelResponse(fn AfterModelResponseHook) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.afterModelResponse = append(r.afterModelResponse, fn)
}
func (r *Registry) RegisterAfterTool(fn AfterToolHook) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.afterTool = append(r.afterTool, fn)
}

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
