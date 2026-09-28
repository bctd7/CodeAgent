package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"CodeAgent/internal/llm"
)

// Agent 是会话执行核心，负责内存历史维护、后台任务循环、工具调度。Loop才是真正的循环调度。
// 纠正排队、任务取消以及跨模型跨协议切换。
type Agent struct {
	mu sync.Mutex

	// 配置与依赖
	stream       StreamFunc
	systemPrompt string
	tools        map[string]Tool
	toolsList    []llm.Tool
	// 工具并发上限
	maxParallelTools int

	// 模型配置及待生效切换
	currentModel   llm.Model
	currentOptions llm.RequestOptions
	pendingModel   *llm.Model
	pendingOptions *llm.RequestOptions

	// 会话内存历史与思考同源记录
	history       []llm.Message
	messageOrigin map[*llm.AssistantMessage]modelOrigin

	// 运行状态与队列
	status     AgentStatus
	currentRun *activeRun
	// 任务排队队列
	queue []*queuedRun
	// 引导功能的提示词
	steerQueue []string

	// 计数与控制
	runCounter int64
	//检查当前的currentRun
	workNotify chan struct{}
	done       chan struct{}
	closeOnce  sync.Once

	// 串行防死锁事件分发器
	//Event结提交的消息通知结果
	dispatcher *eventDispatcher
}

// New 实例化 Agent，完成配置校验、依赖注入与后台工作循环初始化。
func New(cfg Config) (*Agent, error) {
	if cfg.Stream == nil {
		return nil, fmt.Errorf("Stream 调度函数不能为空")
	}
	if cfg.MaxParallelTools < 0 {
		return nil, fmt.Errorf("MaxParallelTools 不能为负数: %d", cfg.MaxParallelTools)
	}

	maxParallelTools := cfg.MaxParallelTools
	if maxParallelTools == 0 {
		maxParallelTools = 4
	}

	toolMap, llmTools, err := validateTools(cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("工具配置非法: %w", err)
	}

	a := &Agent{
		stream:           cfg.Stream,
		systemPrompt:     cfg.SystemPrompt,
		tools:            toolMap,
		toolsList:        llmTools,
		maxParallelTools: maxParallelTools,
		currentModel:     cfg.Model,
		currentOptions:   cfg.Options,
		status:           AgentStatusIdle,
		history:          copyHistory(cfg.InitialHistory),
		messageOrigin:    make(map[*llm.AssistantMessage]modelOrigin),
		queue:            make([]*queuedRun, 0),
		workNotify:       make(chan struct{}, 1),
		done:             make(chan struct{}),
		dispatcher:       newEventDispatcher(cfg.OnEvent),
	}
	go a.runLoop()
	return a, nil
}

// Submit 接受用户消息并入队，立即返回运行句柄。
func (a *Agent) Submit(ctx context.Context, message *llm.UserMessage) (*RunHandle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if message == nil {
		return nil, fmt.Errorf("用户消息不能为空")
	}

	a.mu.Lock()
	if a.status == AgentStatusClosed {
		a.mu.Unlock()
		return nil, fmt.Errorf("Agent 已关闭，不接受新任务")
	}

	a.runCounter++
	runID := fmt.Sprintf("run_%d", a.runCounter)
	handle := newRunHandle(runID, a)
	queuePos := a.enqueueRunLocked(runID, handle, ctx, message)
	// 与任务接受一起入队，后台才能始终先观察到 InputReceived。
	a.dispatcher.enqueue(Event{
		Kind:      EventInputReceived,
		RunID:     runID,
		Detail:    fmt.Sprintf("任务已进入队列 (排队位置: %d)", queuePos),
		Message:   message,
		Timestamp: time.Now(),
	})
	a.mu.Unlock()

	go a.watchRunContext(ctx, handle)
	select {
	case a.workNotify <- struct{}{}:
	default:
	}
	a.dispatcher.drain()

	return handle, nil
}

// Steer 向当前活动 Run 提交纠正；没有活动 Run 或正在收尾/取消时返回明确错误。
func (a *Agent) Steer(message string) error {
	a.mu.Lock()
	if a.status == AgentStatusClosed {
		a.mu.Unlock()
		return fmt.Errorf("Agent 已关闭")
	}
	if a.currentRun == nil || a.status != AgentStatusRunning {
		a.mu.Unlock()
		return fmt.Errorf("没有活动的 Run，无法提交纠正")
	}

	a.steerQueue = append(a.steerQueue, message)
	runID := a.currentRun.id
	a.dispatcher.enqueue(Event{
		Kind:      EventInputReceived,
		RunID:     runID,
		Detail:    "纠正已接受: " + message,
		Timestamp: time.Now(),
	})
	a.mu.Unlock()
	a.dispatcher.drain()
	return nil
}

// SetModel 请求切换模型及请求选项；由 Agent 在规定边界应用。
func (a *Agent) SetModel(model llm.Model, options llm.RequestOptions) error {
	a.mu.Lock()
	if a.status == AgentStatusClosed {
		a.mu.Unlock()
		return fmt.Errorf("Agent 已关闭")
	}

	a.pendingModel = &model
	a.pendingOptions = &options

	activeRunID := ""
	if a.currentRun != nil {
		activeRunID = a.currentRun.id
	}

	a.dispatcher.enqueue(Event{
		Kind:      EventModelSwitchRequested,
		RunID:     activeRunID,
		Detail:    fmt.Sprintf("请求切换模型至: %s (%s)", model.ID, model.API),
		Timestamp: time.Now(),
	})
	if a.currentRun == nil && a.status == AgentStatusIdle {
		a.changeModel()
	}
	a.mu.Unlock()
	a.dispatcher.drain()
	return nil
}

// Snapshot 读取当前状态、历史和排队概况的只读快照。
func (a *Agent) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	s := Snapshot{
		Status:       a.status,
		CurrentModel: a.currentModel,
		QueueLength:  len(a.queue),
		QueuedRunIDs: make([]string, 0, len(a.queue)),
		HistoryCount: len(a.history),
		History:      copyHistory(a.history),
	}

	if a.currentRun != nil {
		s.CurrentRunID = a.currentRun.id
	}
	if a.pendingModel != nil {
		m := *a.pendingModel
		s.PendingModel = &m
	}
	for _, q := range a.queue {
		s.QueuedRunIDs = append(s.QueuedRunIDs, q.id)
	}

	return s
}

// Close 停止接受新任务，取消所有未完成任务并等待工作循环退出。
// 无论重复或并发调用多少次，完整关闭流程只执行一次，所有调用方均等待首次完整关闭流程结束。
// 使用约定：外部事件回调（OnEvent）中同步调用阻塞式 Close() 或同步等待当前 Run
// 可能与工作循环形成相互等待环；外部消费者如需在收到事件后关闭实例，应另启 goroutine 执行。
func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.status = AgentStatusClosed
		a.mu.Unlock()

		a.cancelAllHandles()
	})

	<-a.done
	return nil
}

// watchRunContext 从提交到结束监听本次任务的 Context 取消。
func (a *Agent) watchRunContext(ctx context.Context, handle *RunHandle) {
	select {
	case <-ctx.Done():
		a.cancelHandle(handle)
	case <-handle.done:
	}
}
