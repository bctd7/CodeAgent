package agent

import (
	"log"
	"sync"
	"time"

	"CodeAgent/internal/llm"
)

// EventKind 标识 Agent 事件的具体类型。
type EventKind string

const (
	EventRunStart             EventKind = "run_start"
	EventRunEnd               EventKind = "run_end"
	EventStepStart            EventKind = "step_start"
	EventStepEnd              EventKind = "step_end"
	EventLLMEvent             EventKind = "llm_event"
	EventToolStart            EventKind = "tool_start"
	EventToolProgress         EventKind = "tool_progress"
	EventToolEnd              EventKind = "tool_end"
	EventToolSkipped          EventKind = "tool_skipped"
	EventInputReceived        EventKind = "input_received"
	EventInputDropped         EventKind = "input_dropped"
	EventModelSwitchRequested EventKind = "model_switch_requested"
	EventModelSwitched        EventKind = "model_switched"
)

// Event 是 Agent 生命周期与执行过程中的对外通知事件。
type Event struct {
	Kind         EventKind         `json:"kind"`
	RunID        string            `json:"run_id,omitempty"`
	StepIndex    int               `json:"step_index,omitempty"`
	ToolCallID   string            `json:"tool_call_id,omitempty"`
	ToolName     string            `json:"tool_name,omitempty"`
	LLMEvent     *llm.Event        `json:"llm_event,omitempty"`
	Message      llm.Message       `json:"message,omitempty"`
	Result       *RunResult        `json:"result,omitempty"`
	ContentBlock *llm.ContentBlock `json:"content_block,omitempty"`
	Detail       string            `json:"detail,omitempty"`
	Timestamp    time.Time         `json:"timestamp"`
}

// eventDispatcher 串行分发事件，回调重入产生的事件追加到队列。
// 外部回调绝不在 Agent 状态锁内调用；当回调内再次调用 Submit/Steer/SetModel/Cancel 等非等待接口产生新事件时，
// 新事件追加至内部队列，由当前分发循环依次排出，绝不发生递归加锁或自锁。
//
// 使用约定：外部回调中同步等待当前 Run（如 Handle.Wait()）或调用阻塞式 Close() 可能与工作循环形成等待环；
// 外部消费者如需在收到事件后执行此类阻塞操作，应另启 goroutine 执行。
type eventDispatcher struct {
	mu       sync.Mutex
	events   []Event
	emitting bool
	onEvent  func(Event)
}

func newEventDispatcher(onEvent func(Event)) *eventDispatcher {
	return &eventDispatcher{
		onEvent: onEvent,
		events:  make([]Event, 0, 16),
	}
}

func (d *eventDispatcher) emit(evt Event) {
	d.enqueue(evt)
	d.drain()
}

// enqueue 只入队，不调用外部代码，可在 Agent 状态锁内确定事件顺序。
func (d *eventDispatcher) enqueue(evt Event) {
	if d.onEvent == nil {
		return
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}

	d.mu.Lock()
	d.events = append(d.events, evt)
	d.mu.Unlock()
}

// drain 必须在释放 Agent 状态锁后调用；已有分发者时交由其继续处理。
func (d *eventDispatcher) drain() {
	d.mu.Lock()
	if d.emitting {
		d.mu.Unlock()
		return
	}
	d.emitting = true

	for len(d.events) > 0 {
		next := d.events[0]
		d.events[0] = Event{}
		d.events = d.events[1:]
		d.mu.Unlock()

		// 外部回调在锁外执行。回调故障不应被误报为工具执行故障。
		d.deliver(next)

		d.mu.Lock()
	}
	d.emitting = false
	d.mu.Unlock()
}

func (d *eventDispatcher) deliver(evt Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Agent 事件回调发生 panic: %v", r)
		}
	}()
	d.onEvent(evt)
}
