package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Result 是一条流的唯一最终结果。
// Provider 故障（模型报错、用户取消）写入 Message；InternalErr 只记录
// 我们自身代码的不变量错误（例如适配器发了非法事件），不属于用户文案。
type Result struct {
	Message     AssistantMessage
	InternalErr error
}

// StreamReader 是消费者（Agent）拿到的"读卡"：用它读取模型的流式输出。
//
// 消费者只读契约：从 StreamReader 读取到的所有事件、Snapshot 快照及最终 Result.Message
// 均由消费者保持只读，消费者不得在外部修改其内部 Content 切片或参数 Map。
type StreamReader interface {
	// Next 取下一个事件；没有则阻塞等待，直到有新事件、流结束或 ctx 取消。
	// 返回 false 表示"没读到"（流结束，或消费者主动放弃等待，流本身继续）。
	Next(context.Context) (Event, bool)
	// Result 阻塞直到流到达终态，返回最终消息。它不依赖事件消费。
	Result() Result
	// Close 中止这条流：以 aborted 收尾，保留最近发布的消息快照。
	Close()
}

// StreamWriter 是适配器拿到的"写卡"：用它产出事件并终止流。
//
// 快照契约：写卡发出的消息快照（Snapshot/FinalMessage 以及 Finish 的 message）
// 交出去之后即视为不可变——流不再拷贝它们，直接引用。适配器若要原地累积
// 内容（比如把 delta 拼进同一个 ContentBlock），必须自己复制一份再发出，
// 而不是在发出后继续改动同一份。
type StreamWriter interface {
	// Context 返回与流生命周期绑定的 context。适配器用它发 HTTP，
	// 流一取消，网络请求也跟着取消。
	Context() context.Context
	// Emit 发布一个非终止事件。终止事件必须走 Finish/Fail/Abort。
	Emit(event Event) error
	// Finish 以成功终止流（State=success，StopReason 为 stop/length/tool_use）。
	Finish(message AssistantMessage) bool
	// Fail 以失败终止流（State=error）。
	Fail(message AssistantMessage, internalErr error) bool
	// Abort 以取消终止流（State=aborted），保留最近发布的消息快照。
	Abort(cause error) bool
}

// eventStream 是流的共享内核：读卡和写卡指向同一个实例。
// 每个字段都对应某个方法的某个需求，见下方注释。
type eventStream struct {
	mu sync.Mutex

	// 缓冲区：生产者快、消费者慢，事件先堆在这里，readIndex 标记读到哪。
	events    []Event
	readIndex int
	// 门铃：有新事件时 close 一下，唤醒等待的消费者。
	newEventNotify chan struct{}
	// 结局铃：流终止时 close，唤醒等待 Result 的调用方。
	resultNotify chan struct{}

	terminal bool
	result   Result
	// 最近发布的完整消息快照，Abort 时用它保留已生成内容。
	latestMessage AssistantMessage

	// 流的生命线：从 parent 派生，交给适配器发 HTTP；终止时 cancelWork 停掉它。
	workContext context.Context
	cancelWork  context.CancelFunc
}

// NewEventStream 构造一条流，返回读卡（给消费者）和写卡（给适配器）。
//
// initial 是流的元数据模板：适配器在第一个字节到达前就知道 Provider/API/
// Model/ResponseID 等信息，用它构造 initial（Content 为空、State=pending），
// 后续事件只往里填内容。两张卡指向同一个 eventStream。
func NewEventStream(parent context.Context, initial AssistantMessage) (StreamReader, StreamWriter) {
	if parent == nil {
		parent = context.Background()
	}
	workContext, cancelWork := context.WithCancel(parent)
	es := &eventStream{
		newEventNotify: make(chan struct{}),
		resultNotify:   make(chan struct{}),
		latestMessage:  initial,
		workContext:    workContext,
		cancelWork:     cancelWork,
	}

	// watcher：父 ctx 取消时自动 Abort；流正常结束后经 resultNotify 退出，避免泄漏。
	go func() {
		select {
		case <-workContext.Done():
			es.Abort(workContext.Err())
		case <-es.resultNotify:
		}
	}()
	return es, es
}

// --- 读端 ---------------------------------------------------------------

func (es *eventStream) Next(ctx context.Context) (Event, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		es.mu.Lock()
		if es.readIndex < len(es.events) {
			event := es.events[es.readIndex]
			es.events[es.readIndex] = Event{} // 清空槽位，让大对象可被 GC 回收
			es.readIndex++
			if es.readIndex == len(es.events) {
				es.events = es.events[:0]
				es.readIndex = 0
			}
			es.mu.Unlock()
			return event, true
		}
		if es.terminal {
			es.mu.Unlock()
			return Event{}, false
		}
		// 锁内抓取当前门铃引用，再解锁等待；否则生产者进不来会死锁。
		notify := es.newEventNotify
		es.mu.Unlock()

		select {
		case <-notify:
			// 有新事件，回到循环顶部再查
		case <-ctx.Done():
			// 消费者放弃等待（不是取消流），流继续
			return Event{}, false
		}
	}
}

func (es *eventStream) Result() Result {
	<-es.resultNotify
	es.mu.Lock()
	defer es.mu.Unlock()
	// 直接返回：快照按契约不可变，调用方不应改动。
	return es.result
}

// Close 是消费者"我不玩了，收摊"的信号，等价于 Abort(context.Canceled)。
func (es *eventStream) Close() {
	es.Abort(context.Canceled)
}

// --- 写端 ---------------------------------------------------------------

func (es *eventStream) Context() context.Context {
	return es.workContext
}

func (es *eventStream) Emit(event Event) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.terminal {
		return fmt.Errorf("流已经终止，不能发布 %s", event.Type)
	}
	// 终止事件不能走 Emit，必须走 Finish/Fail/Abort。
	if event.Type == EventMessageComplete || event.Type == EventMessageError {
		err := fmt.Errorf("终止事件必须通过 Finish、Fail 或 Abort 发布")
		es.failInvariantLocked(err)
		return err
	}
	// 每个非终止事件必须带消息快照。
	if event.Snapshot == nil {
		err := fmt.Errorf("事件 %s 缺少消息快照", event.Type)
		es.failInvariantLocked(err)
		return err
	}

	es.latestMessage = *event.Snapshot
	es.appendEventLocked(event)
	return nil
}

func (es *eventStream) Finish(message AssistantMessage) bool {
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.terminal {
		return false
	}
	if message.State != StateSuccess {
		err := fmt.Errorf("Finish 要求 State=success，实际为 %q", message.State)
		es.failInvariantLocked(err)
		return false
	}
	switch message.StopReason {
	case StopStop, StopLength, StopToolUse:
	default:
		err := fmt.Errorf("Finish 必须有明确且合法的成功结束原因，实际为 %q", message.StopReason)
		es.failInvariantLocked(err)
		return false
	}
	return es.terminateLocked(message, nil)
}

func (es *eventStream) Fail(message AssistantMessage, internalErr error) bool {
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.terminal {
		return false
	}
	message.State = StateError
	message.StopReason = ""
	if internalErr != nil && message.ErrorMessage == "" {
		message.ErrorMessage = "模型流内部状态错误"
	}
	return es.terminateLocked(message, internalErr)
}

func (es *eventStream) Abort(cause error) bool {
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.terminal {
		return false
	}
	message := es.latestMessage
	message.State = StateAborted
	message.StopReason = ""
	if errors.Is(cause, context.DeadlineExceeded) {
		message.ErrorMessage = "模型请求超时"
	} else {
		message.ErrorMessage = "模型请求已取消"
	}
	return es.terminateLocked(message, nil)
}

// --- 内部 ---------------------------------------------------------------

// terminateLocked 是所有终止路径（Finish/Fail/Abort/failInvariantLocked）的唯一收口。
// 调用方必须持有 es.mu。
// 它保证：只第一次生效（幂等）、InternalErr 不丢。
func (es *eventStream) terminateLocked(message AssistantMessage, internalErr error) bool {
	if es.terminal {
		return false
	}

	eventType := EventMessageError
	if message.State == StateSuccess {
		eventType = EventMessageComplete
	}

	es.latestMessage = message
	es.result = Result{Message: message, InternalErr: internalErr}
	es.terminal = true
	es.appendEventLocked(Event{Type: eventType, FinalMessage: &message})
	close(es.resultNotify) // 先关结局铃，让 watcher 走正常退出
	es.cancelWork()        // 再停掉适配器的网络
	return true
}

// appendEventLocked 事件入队，并敲门铃唤醒消费者。调用方需已持锁。
func (es *eventStream) appendEventLocked(event Event) {
	es.events = append(es.events, event)
	close(es.newEventNotify)
	es.newEventNotify = make(chan struct{})
}

// failInvariantLocked 处理流契约违反错误。调用方需已持锁。
func (es *eventStream) failInvariantLocked(internalErr error) {
	if es.terminal {
		return
	}
	message := es.latestMessage
	message.State = StateError
	message.StopReason = ""
	message.ErrorMessage = "模型流内部状态错误"
	es.terminateLocked(message, internalErr)
}
