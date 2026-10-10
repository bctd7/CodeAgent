package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"CodeAgent/internal/llm"
)

// Tool 定义 Agent 可执行的具体工具。
// 复用 llm.Tool 保存名称、描述与 Schema，并扩展执行函数与并发安全标记。
//
// Execute 约定：
// - onProgress 只能在 Execute 返回前使用。
// - 工具内部若启动产生进度的 goroutine，必须在 Execute 返回前完成并等待它们退出。
// - 工具必须响应 context 取消。
type Tool struct {
	llm.Tool
	ParallelSafe bool
	Execute      func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error)
}

type ToolStatus string

const (
	ToolSucceeded   ToolStatus = "succeeded"
	ToolFailed      ToolStatus = "failed"
	ToolCanceled    ToolStatus = "canceled"
	ToolDenied      ToolStatus = "denied"
	ToolUnavailable ToolStatus = "unavailable"
	ToolSkipped     ToolStatus = "skipped"
	ToolDeferred    ToolStatus = "deferred"
)

// ToolOutcome 分开保存执行事实和面向模型的投影，不能通过 IsError 推测是否执行。
// ToolFinished 交接时 Result 尚未生成；Driver 确认后，批次和完成事件携带最终 Result。
type ToolOutcome struct {
	Call                  llm.ContentBlock
	Status                ToolStatus
	Executed              bool
	Error                 string
	StartedAt, FinishedAt time.Time
	RawResult             *llm.ToolResultMessage
	Result                *llm.ToolResultMessage
}

// toolCallItem 封装单个待执行的工具调用及在原始响应中的索引位置。
type toolCallItem struct {
	index int
	call  llm.ContentBlock
	tool  Tool
}

// toolGroup 封装一组工具调用（纯串行单一工具，或连续的并发安全工具批次）。
type toolGroup struct {
	parallel bool
	items    []toolCallItem
}

// toolScheduler 管理单次 Step 内部的工具调度与并发控制。
type toolScheduler struct {
	agent            *Agent
	run              *activeRun
	stepIndex        int
	tools            map[string]Tool
	maxParallelTools int
	calls            []llm.ContentBlock
	responseID       CheckpointID

	mu         sync.Mutex
	stopped    bool
	stopReason string
	panicErr   error
	results    []*llm.ToolResultMessage
	outcomes   []ToolOutcome
}

// validateTools 校验工具列表的合法性：检查空名称、缺失执行函数与重复名称。
func validateTools(tools []Tool) (map[string]Tool, []llm.Tool, error) {
	toolMap := make(map[string]Tool, len(tools))
	llmTools := make([]llm.Tool, 0, len(tools))

	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		t.Name = name
		if name == "" {
			return nil, nil, fmt.Errorf("工具名称不能为空")
		}
		if t.Execute == nil {
			return nil, nil, fmt.Errorf("工具 %q 缺少 Execute 执行函数", name)
		}
		if _, exists := toolMap[name]; exists {
			return nil, nil, fmt.Errorf("存在重复注册的工具名称: %q", name)
		}
		toolMap[name] = t
		llmTools = append(llmTools, t.Tool)
	}

	return toolMap, llmTools, nil
}

// validateToolCallBatch 检查整批工具调用的 ID 完整性与唯一性。
// 工具调用 ID 是不透明标识，重复性检查按原始 ID 比较。
func validateToolCallBatch(calls []llm.ContentBlock) error {
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("模型响应中存在空 ToolCallID (工具: %q)", c.Name)
		}
		if seen[c.ID] {
			return fmt.Errorf("模型响应中存在重复的 ToolCallID: %q (工具: %q)", c.ID, c.Name)
		}
		seen[c.ID] = true
	}
	return nil
}

// partitionToolCalls 将模型返回的工具调用序列划分为调度组：
// 连续且声明 ParallelSafe=true 的调用组成并行组；串行工具独立成组。
func partitionToolCalls(calls []llm.ContentBlock, tools map[string]Tool) []toolGroup {
	if len(calls) == 0 {
		return nil
	}

	var groups []toolGroup
	var currentGroup *toolGroup

	for i, c := range calls {
		tool, exists := tools[c.Name]
		item := toolCallItem{index: i, call: c, tool: tool}
		isParallel := exists && tool.ParallelSafe

		if currentGroup == nil {
			currentGroup = &toolGroup{parallel: isParallel, items: []toolCallItem{item}}
		} else if currentGroup.parallel && isParallel {
			currentGroup.items = append(currentGroup.items, item)
		} else {
			groups = append(groups, *currentGroup)
			currentGroup = &toolGroup{parallel: isParallel, items: []toolCallItem{item}}
		}
	}

	if currentGroup != nil {
		groups = append(groups, *currentGroup)
	}

	return groups
}

func newToolScheduler(a *Agent, r *activeRun, stepIndex int, calls []llm.ContentBlock, responseID CheckpointID) *toolScheduler {
	return &toolScheduler{
		agent:            a,
		responseID:       responseID,
		run:              r,
		stepIndex:        stepIndex,
		tools:            a.tools,
		maxParallelTools: a.maxParallelTools,
		calls:            calls,
		results:          make([]*llm.ToolResultMessage, len(calls)),
		outcomes:         make([]ToolOutcome, len(calls)),
	}
}

// execute 执行全部工具组调度，返回按模型原始顺序排列的结果列表与内部故障。
func (s *toolScheduler) execute() ([]*llm.ToolResultMessage, error) {
	groups := partitionToolCalls(s.calls, s.tools)

	for _, g := range groups {
		if g.parallel {
			s.executeParallelGroup(g)
		} else {
			s.executeToolCall(g.items[0])
		}
	}

	s.fillSkippedResults()

	s.mu.Lock()
	panicErr := s.panicErr
	s.mu.Unlock()

	return s.results, panicErr
}

// executeParallelGroup 调度并行工具组：先等待并发名额，再创建执行 goroutine。
func (s *toolScheduler) executeParallelGroup(g toolGroup) {
	sem := make(chan struct{}, s.maxParallelTools)
	var wg sync.WaitGroup

schedule:
	for _, item := range g.items {
		select {
		case sem <- struct{}{}:
		case <-s.run.ctx.Done():
			s.recordStop("因取消而未执行")
			break schedule
		}

		// 取得名额后确认是否仍需启动工具。
		if reason, stop := s.checkControlStatus(); stop {
			<-sem
			s.recordStop(reason)
			break
		}

		wg.Add(1)
		go func(it toolCallItem) {
			defer func() {
				<-sem
				wg.Done()
			}()
			s.executeToolCall(it)
		}(item)
	}

	wg.Wait()
}

// executeToolCall 启动单个工具并记录其结果。
func (s *toolScheduler) executeToolCall(item toolCallItem) {
	call := item.call

	if reason, stop := s.checkControlStatus(); stop {
		s.recordStop(reason)
		return
	}

	var blocks []llm.ContentBlock
	var isError bool
	var skipped bool
	outcome := ToolOutcome{Call: call, Status: ToolSucceeded}

	if item.tool.Execute == nil {
		blocks = []llm.ContentBlock{
			{Type: llm.TypeText, Text: fmt.Sprintf("错误: 未知工具 %q", call.Name)},
		}
		isError = true
		outcome.Status = ToolUnavailable
		outcome.Error = fmt.Sprintf("未知工具 %q", call.Name)
	} else {
		_, decision := s.agent.handoff(s.run, Checkpoint{Kind: ToolStarting, StepIndex: s.stepIndex, ResponseID: s.responseID, ToolCall: &call}, s)
		if decision.Kind == Stop {
			s.recordStop("因执行故障而未执行")
			return
		}
		if decision.ToolArguments != nil {
			call.Arguments = *decision.ToolArguments
			outcome.Call = call
		}
		if decision.Kind == Skip || decision.Kind == DeferRun {
			blocks = []llm.ContentBlock{{Type: llm.TypeText, Text: decision.Reason}}
			isError, skipped = true, true
			outcome.Status = ToolDenied
			if decision.Disposition != "" {
				outcome.Status = decision.Disposition
			}
			outcome.Error = decision.Reason
			if decision.Kind == DeferRun {
				outcome.Status = ToolDeferred
			}
		} else {
			s.agent.dispatcher.emit(Event{
				Kind:       EventToolStart,
				RunID:      s.run.id,
				StepIndex:  s.stepIndex,
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Detail:     fmt.Sprintf("工具调用开始: %s", call.Name),
			})

			var execErr error
			var panicVal any
			if err := s.run.ctx.Err(); err != nil {
				execErr = err
			} else {
				outcome.Executed = true
				outcome.StartedAt = time.Now().UTC()
				blocks, execErr, panicVal = s.invokeTool(call, item.tool)
				outcome.FinishedAt = time.Now().UTC()
			}
			switch {
			case panicVal != nil:
				outcome.Status = ToolFailed
				outcome.Error = fmt.Sprint(panicVal)
				s.recordPanic(call.Name, panicVal)
				blocks = []llm.ContentBlock{
					{Type: llm.TypeText, Text: fmt.Sprintf("工具内部故障 (panic): %v", panicVal)},
				}
				isError = true
			case errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded):
				outcome.Status = ToolCanceled
				outcome.Error = execErr.Error()
				blocks = []llm.ContentBlock{
					{Type: llm.TypeText, Text: "执行已中断，副作用可能已发生"},
				}
				isError = true
			case execErr != nil:
				outcome.Status = ToolFailed
				outcome.Error = execErr.Error()
				blocks = []llm.ContentBlock{
					{Type: llm.TypeText, Text: fmt.Sprintf("工具执行错误: %v", execErr)},
				}
				isError = true
			}
		}

	}

	res := &llm.ToolResultMessage{
		Role:       llm.RoleTool,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    blocks,
		IsError:    isError,
		Timestamp:  time.Now(),
	}

	outcome = s.saveResult(item.index, res, outcome)
	kind := EventToolEnd
	if skipped {
		kind = EventToolSkipped
	}
	s.agent.dispatcher.emit(Event{
		Kind:        kind,
		RunID:       s.run.id,
		StepIndex:   s.stepIndex,
		ToolCallID:  call.ID,
		ToolName:    call.Name,
		ToolOutcome: &outcome,
		Detail:      fmt.Sprintf("工具调用完成 (is_error: %t)", res.IsError),
	})
}

// invokeTool 调用工具，并限制进度回调只在执行期间有效。
func (s *toolScheduler) invokeTool(call llm.ContentBlock, tool Tool) (blocks []llm.ContentBlock, execErr error, panicVal any) {
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)

	onProgress := func(b llm.ContentBlock) {
		if !active.Load() {
			return
		}
		s.agent.dispatcher.emit(Event{
			Kind:         EventToolProgress,
			RunID:        s.run.id,
			StepIndex:    s.stepIndex,
			ToolCallID:   call.ID,
			ToolName:     call.Name,
			ContentBlock: &b,
		})
	}

	defer func() {
		panicVal = recover()
	}()
	blocks, execErr = tool.Execute(s.run.ctx, call.ID, call.Arguments, onProgress)
	return
}

// fillSkippedResults 在所有在途工具退出后，为没有结果的调用补齐未执行结果。
// 工具有可能没调用就先跳过执行
func (s *toolScheduler) fillSkippedResults() {
	reason := s.stopReason
	if reason == "" {
		reason = "因取消而未执行"
	}

	for i, res := range s.results {
		if res != nil {
			continue
		}
		call := s.calls[i]
		result := &llm.ToolResultMessage{
			Role:       llm.RoleTool,
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Content: []llm.ContentBlock{
				{Type: llm.TypeText, Text: reason},
			},
			IsError:   true,
			Timestamp: time.Now(),
		}
		outcome := s.saveResult(i, result, ToolOutcome{Call: call, Status: ToolSkipped, Error: reason})
		s.agent.dispatcher.emit(Event{
			Kind:        EventToolSkipped,
			RunID:       s.run.id,
			StepIndex:   s.stepIndex,
			ToolCallID:  call.ID,
			ToolName:    call.Name,
			ToolOutcome: &outcome,
			Detail:      reason,
		})
	}
}

// saveResult 交付每个最终结果，不等待同组慢工具。
func (s *toolScheduler) saveResult(index int, result *llm.ToolResultMessage, outcome ToolOutcome) ToolOutcome {
	outcome.RawResult = result
	_, decision := s.agent.handoff(s.run, Checkpoint{Kind: ToolFinished, StepIndex: s.stepIndex, ResponseID: s.responseID, ToolCall: &outcome.Call, ToolResult: result, ToolOutcome: &outcome}, nil)
	visible := *result
	outcome.Result = &visible
	if decision.Kind == Stop {
		// 加工失败后不得向模型暴露原始输出，后续任务也须遵守此规则。
		visible.Content = []llm.ContentBlock{}
		s.recordStop("因执行故障而未执行")
	} else if decision.ToolContent != nil {
		visible.Content = *decision.ToolContent
		if visible.Content == nil {
			visible.Content = []llm.ContentBlock{}
		}
	}
	s.mu.Lock()
	s.results[index] = &visible
	s.outcomes[index] = outcome
	s.mu.Unlock()
	return outcome
}

// recordStop 记录调度停止原因，仅在尚未停止时记录首次原因。
func (s *toolScheduler) recordStop(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.stopReason = reason
	}
}

// recordPanic 记录工具内部 panic，保留首次发生的 panic 并停止后续调度。
func (s *toolScheduler) recordPanic(toolName string, panicVal any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panicErr == nil {
		s.panicErr = fmt.Errorf("工具 %q 执行发生内部故障 (panic): %v", toolName, panicVal)
	}
	s.stopped = true
	s.stopReason = "因前序工具内部故障而未执行"
}

// checkControlStatus 检查是否应该停止启动新工具。
// 这是调度前检查；等待 Hook/落库后仍由 startPermissionLocked 原子确认启动资格。
func (s *toolScheduler) checkControlStatus() (string, bool) {
	s.mu.Lock()
	if s.stopped {
		reason := s.stopReason
		s.mu.Unlock()
		return reason, true
	}
	s.mu.Unlock()
	if s.run.ctx.Err() != nil {
		return "因取消而未执行", true
	}
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	if s.agent.status == AgentStatusCancelling || s.agent.status == AgentStatusClosed {
		return "因取消而未执行", true
	}
	if len(s.agent.steerQueue) > 0 {
		return "因用户纠正而未执行", true
	}
	return "", false
}

// startPermissionLocked 是工具启动预约的原子边界。调用者持有 Agent 状态锁。
func (s *toolScheduler) startPermissionLocked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return s.stopReason
	}
	if s.agent.executionErr != nil {
		return "因执行故障而未执行"
	}
	if s.run.ctx.Err() != nil || s.agent.status == AgentStatusClosed || s.agent.status == AgentStatusCancelling {
		return "因取消而未执行"
	}
	if len(s.agent.steerQueue) > 0 {
		return "因用户纠正而未执行"
	}
	return ""
}
