package agent

import (
	"context"
	"fmt"
	"maps"
	"time"

	"CodeAgent/internal/llm"
)

// activeRun 表示当前正在后台执行的任务。
type activeRun struct {
	id         string
	handle     *RunHandle
	ctx        context.Context
	cancel     context.CancelFunc
	userMsg    *llm.UserMessage
	steps      int
	messages   []llm.Message
	usage      map[string]llm.Usage
	totalUsage llm.Usage
	finishing  bool
	err        error // 本轮执行错误，不影响后续任务准入
	deferred   string
}

// runLoop 后台工作主循环，作为关闭收尾的唯一负责人。
// 退出时关闭 done 信号，唤醒所有等待 Close() 的调用方。
func (a *Agent) runLoop() {
	defer close(a.done)

	for {
		a.boundary <- struct{}{}
		a.mu.Lock()
		if a.status == AgentStatusClosed {
			a.mu.Unlock()
			<-a.boundary
			return
		}
		run, skipped := a.popNextRunLocked()
		a.mu.Unlock()
		<-a.boundary
		a.notifyDroppedTasks(skipped)

		if run != nil {
			a.executeRun(run)
		} else {
			<-a.workNotify
		}
	}
}

// executeRun 执行单个 Run 的全流程生命周期与状态收敛。
// 只保留任务推进、响应判断、工具执行和结束决策。
func (a *Agent) executeRun(run *activeRun) {
	defer run.cancel()

	_, decision := a.handoff(run, Checkpoint{Kind: RunStarting, UserMessage: run.userMsg}, nil)
	if decision.Kind == Stop {
		a.mu.Lock()
		a.finishRunAndUnlock(run, RunResult{}, nil)
		return
	}
	a.dispatcher.emit(Event{
		Kind:      EventRunStart,
		RunID:     run.id,
		Detail:    "Run 开始执行",
		Timestamp: time.Now(),
	})

	a.mu.Lock()
	a.history = append(a.history, run.userMsg)
	run.messages = append(run.messages, run.userMsg)
	a.mu.Unlock()

	// 工具结果加入历史后继续当前 Run；收尾时返回外层，由 runLoop 取下一个任务。
	for {
		a.boundary <- struct{}{}
		// 检查取消与关闭
		a.mu.Lock()
		if a.status == AgentStatusClosed || a.status == AgentStatusCancelling || run.ctx.Err() != nil {
			<-a.boundary
			a.finishRunAndUnlock(run, RunResult{}, nil)
			return
		}

		// 检查并应用待生效模型切换
		if a.pendingModel != nil {
			a.changeModel()
			a.mu.Unlock()
			<-a.boundary
			a.dispatcher.drain()
			// 回调可能提交新目标或取消任务，重新检查后再准备请求。
			continue
		}

		// 已消费纠正先保存，再进入本次模型请求。
		steers := a.steerQueue
		a.steerQueue = nil
		a.mu.Unlock()
		for _, text := range steers {
			msg := &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "[用户纠正]: " + text}}, Timestamp: time.Now()}
			_, decision := a.handoff(run, Checkpoint{Kind: InputConsumed, UserMessage: msg}, nil)
			if decision.Kind == Stop {
				a.mu.Lock()
				<-a.boundary
				a.finishRunAndUnlock(run, RunResult{}, nil)
				return
			}
			a.mu.Lock()
			a.history = append(a.history, msg)
			run.messages = append(run.messages, msg)
			a.mu.Unlock()
		}
		a.mu.Lock()
		if run.ctx.Err() != nil {
			<-a.boundary
			a.finishRunAndUnlock(run, RunResult{}, nil)
			return
		}
		req, stepIndex, currentModel, err := a.prepareRequest(run)
		if err != nil {
			<-a.boundary
			a.finishRunAndUnlock(run, RunResult{
				State:        RunStateError,
				ErrorMessage: fmt.Sprintf("准备模型请求失败: %v", err),
				Error:        err,
			}, nil)
			return
		}
		a.mu.Unlock()
		<-a.boundary

		_, decision := a.handoff(run, Checkpoint{Kind: ModelRequestReady, StepIndex: stepIndex, Request: &req}, nil)
		if decision.Kind == Stop {
			a.mu.Lock()
			a.finishRunAndUnlock(run, RunResult{}, nil)
			return
		}
		a.mu.Lock()
		if decision.RequestContext != nil {
			req.SystemPrompt = decision.RequestContext.SystemPrompt
			req.Messages = decision.RequestContext.Messages
		}
		if !a.canStartWorkLocked(run) {
			a.finishRunAndUnlock(run, RunResult{}, nil)
			return
		}
		a.mu.Unlock()
		_, decision = a.handoff(run, Checkpoint{Kind: ModelCallStarting, StepIndex: stepIndex, Request: &req}, nil)
		if decision.Kind == Stop {
			a.mu.Lock()
			a.finishRunAndUnlock(run, RunResult{}, nil)
			return
		}
		a.mu.Lock()
		run.steps++
		a.mu.Unlock()
		a.dispatcher.emit(Event{
			Kind:      EventStepStart,
			RunID:     run.id,
			StepIndex: stepIndex,
			Detail:    fmt.Sprintf("第 %d 步开始 (模型: %s)", stepIndex, currentModel.ID),
			Timestamp: time.Now(),
		})

		// 调用底层模型建流并转发事件
		streamResult, err := a.callStreamAndForward(run.ctx, run.id, stepIndex, req)
		if err != nil {
			stepEnd := Event{
				Kind:      EventStepEnd,
				RunID:     run.id,
				StepIndex: stepIndex,
				Detail:    fmt.Sprintf("第 %d 步建流失败: %v", stepIndex, err),
				Timestamp: time.Now(),
			}
			a.mu.Lock()
			a.finishRunAndUnlock(run, RunResult{
				State:        RunStateError,
				ErrorMessage: fmt.Sprintf("模型建流失败: %v", err),
				Error:        err,
			}, &stepEnd)
			return
		}

		assistantMsg := streamResult.Message

		// 集中记录模型最终消息、同源来源与用量
		origin := ModelOrigin{Provider: currentModel.Provider, API: currentModel.API, ModelID: currentModel.ID, BaseURL: currentModel.BaseURL}
		responseID, decision := a.handoff(run, Checkpoint{Kind: ModelResponseReady, StepIndex: stepIndex, Response: &assistantMsg, Origin: &origin}, nil)
		a.recordResponseAndUsage(run, currentModel, &assistantMsg)
		if decision.Kind == Stop {
			stepEnd := Event{Kind: EventStepEnd, RunID: run.id, StepIndex: stepIndex, Detail: decision.Error.Error()}
			a.mu.Lock()
			a.finishRunAndUnlock(run, RunResult{}, &stepEnd)
			return
		}

		result, toolCalls := classifyResponse(&assistantMsg, streamResult.InternalErr)
		stepEnd := Event{Kind: EventStepEnd, RunID: run.id, StepIndex: stepIndex, Timestamp: time.Now()}
		a.mu.Lock()
		if !a.canStartWorkLocked(run) {
			stepEnd.Detail = "任务已停止"
			a.finishRunAndUnlock(run, RunResult{}, &stepEnd)
			return
		}
		if result != nil {
			stepEnd.Detail = result.ErrorMessage
			if stepEnd.Detail == "" {
				stepEnd.Detail = "模型生成回答完成"
			}
			// 正常回答完成前接受的纠正，仍由同一个 Run 继续处理。
			if result.State == RunStateSuccess && result.StopReason == RunStopCompleted && len(a.steerQueue) > 0 {
				a.mu.Unlock()
				a.dispatcher.emit(stepEnd)
				continue
			}
			if result.State == RunStateSuccess && result.StopReason == RunStopCompleted {
				candidateResult := snapshotRunResult(run, *result)
				a.mu.Unlock()
				_, candidate := a.handoff(run, Checkpoint{Kind: RunEnding, StepIndex: stepIndex, Result: &candidateResult}, nil)
				a.mu.Lock()
				if candidate.Kind == Repeat && a.canStartWorkLocked(run) {
					a.history = append(a.history, candidate.ContextMessages...)
					run.messages = append(run.messages, candidate.ContextMessages...)
					a.mu.Unlock()
					a.dispatcher.emit(stepEnd)
					continue
				}
				// 在候选结束等待期间接受的纠正仍属于当前任务。
				if candidate.Kind != Stop && a.canStartWorkLocked(run) && len(a.steerQueue) > 0 {
					a.mu.Unlock()
					a.dispatcher.emit(stepEnd)
					continue
				}
			}
			a.finishRunAndUnlock(run, *result, &stepEnd)
			return
		}
		a.mu.Unlock()

		// 调度执行工具
		scheduler := newToolScheduler(a, run, stepIndex, toolCalls, responseID)
		results, panicErr := scheduler.execute()
		_, batchDecision := a.handoff(run, Checkpoint{Kind: ToolBatchFinished, StepIndex: stepIndex, ResponseID: responseID, ToolBatch: scheduler.outcomes}, nil)

		// 工具结果严格按模型原始调用顺序追加至历史
		a.mu.Lock()
		for _, tr := range results {
			if tr != nil {
				a.history = append(a.history, tr)
				run.messages = append(run.messages, tr)
			}
		}
		if batchDecision.Kind == Continue {
			a.history = append(a.history, batchDecision.ContextMessages...)
			run.messages = append(run.messages, batchDecision.ContextMessages...)
		}
		a.mu.Unlock()

		stepEnd.Detail = fmt.Sprintf("工具执行完毕 (数量: %d)", len(toolCalls))

		a.mu.Lock()
		if panicErr != nil {
			stepEnd.Detail = panicErr.Error()
			a.finishRunAndUnlock(run, RunResult{
				State:        RunStateError,
				ErrorMessage: panicErr.Error(),
				Error:        panicErr,
			}, &stepEnd)
			return
		}
		if a.status == AgentStatusClosed || a.status == AgentStatusCancelling || run.ctx.Err() != nil {
			stepEnd.Detail = "任务已取消"
			a.finishRunAndUnlock(run, RunResult{}, &stepEnd)
			return
		}
		a.mu.Unlock()
		a.dispatcher.emit(stepEnd)
		// 工具结果已加入历史，继续处理纠正或准备下一次模型调用。
	}
}

// prepareRequest 在持有 a.mu 时投影历史并固定本次请求；尚不计入调用次数。
func (a *Agent) prepareRequest(run *activeRun) (llm.Request, int, llm.Model, error) {
	req, err := prepareRequestHistory(
		a.history,
		a.currentModel,
		a.toolsList,
		a.currentOptions,
		a.systemPrompt,
		a.messageOrigin,
	)
	if err != nil {
		return llm.Request{}, 0, a.currentModel, err
	}
	return req, run.steps + 1, a.currentModel, nil
}

// callStreamAndForward 调用底层模型建流并实时转发流式事件，流关闭由 defer 保证。
func (a *Agent) callStreamAndForward(ctx context.Context, runID string, stepIndex int, req llm.Request) (llm.Result, error) {
	reader, err := a.stream(ctx, req)
	if err != nil {
		return llm.Result{}, err
	}
	defer reader.Close()

	for {
		evt, ok := reader.Next(ctx)
		if !ok {
			break
		}
		a.dispatcher.emit(Event{
			Kind:      EventLLMEvent,
			RunID:     runID,
			StepIndex: stepIndex,
			LLMEvent:  &evt,
			Timestamp: time.Now(),
		})
	}
	if ctx.Err() != nil {
		reader.Close()
	}
	return reader.Result(), nil
}

// snapshotRunResult 在持有 Agent 状态锁时补全本轮结果。
// 复制消息切片和用量表，使结束候选在后续续跑时保持不变；消息对象仍按协议共享只读。
func snapshotRunResult(run *activeRun, result RunResult) RunResult {
	result.RunID = run.id
	result.Steps = run.steps
	result.Messages = copyHistory(run.messages)
	result.Usage = maps.Clone(run.usage)
	result.TotalUsage = run.totalUsage
	return result
}

// finishRunAndUnlock 持锁确定终态，交接确认后保存内存结果并分发完成事件。
// 调用方调用后必须立即退出 executeRun。
func (a *Agent) finishRunAndUnlock(run *activeRun, result RunResult, stepEnd *Event) {
	var droppedTasks []droppedTaskInfo

	result = snapshotRunResult(run, result)

	// 未消费的纠正属于当前 Run，不能留给下一任务。
	droppedSteers := a.steerQueue
	a.steerQueue = nil

	// 已接受的关闭或取消优先于模型调用结果。
	ctxErr := run.ctx.Err()
	if a.status == AgentStatusClosed || a.status == AgentStatusCancelling || ctxErr != nil {
		result.State = RunStateAborted
		result.StopReason = ""
		result.RawStopReason = ""
		result.Error = ctxErr
		if result.Error == nil {
			result.Error = context.Canceled
		}
		switch {
		case a.status == AgentStatusClosed:
			result.ErrorMessage = "Agent 已关闭"
		case ctxErr == context.DeadlineExceeded:
			result.ErrorMessage = "任务超时"
		default:
			result.ErrorMessage = "任务已被取消"
		}
	}

	if run.err != nil {
		result.State = RunStateError
		result.StopReason, result.RawStopReason = "", ""
		result.ErrorMessage, result.Error = run.err.Error(), run.err
	}
	if run.deferred != "" && run.err == nil && a.status != AgentStatusCancelling && a.status != AgentStatusClosed {
		result.State = RunStateDeferred
		result.StopReason = ""
		result.RawStopReason = ""
		result.Error = nil
		result.ErrorMessage = run.deferred
	}
	if a.executionErr != nil {
		result.State = RunStateError
		result.StopReason, result.RawStopReason = "", ""
		result.ErrorMessage, result.Error = a.executionErr.Error(), a.executionErr
	}
	run.finishing = true
	a.mu.Unlock()
	_, decision := a.handoff(run, Checkpoint{Kind: RunFinishing, Result: &result}, nil)
	a.mu.Lock()
	// 正常确认表示终态已经提交；之后的实例故障不能改写该结果。
	if decision.Kind == Stop {
		result.State = RunStateError
		result.StopReason = ""
		result.RawStopReason = ""
		result.ErrorMessage = decision.Error.Error()
		result.Error = decision.Error
	}
	// 成功或单任务取消后保留排队任务；失败或截断则丢弃。
	truncated := result.State == RunStateSuccess && result.StopReason == RunStopTruncated
	a.currentRun = nil
	if a.status != AgentStatusClosed {
		a.status = AgentStatusIdle
		if truncated || (result.State != RunStateAborted && result.State != RunStateSuccess) {
			reason := "前置任务未成功完成或排队已取消，后续任务未执行"
			if truncated {
				reason = "前置任务输出截断，后续任务未执行"
			}
			droppedTasks = a.dropAllQueuedLocked(reason)
		}
	}

	// 进入空闲时应用待切换模型，不额外发起请求。
	if a.status == AgentStatusIdle && a.pendingModel != nil {
		a.changeModel()
	}
	// 先保存结果并解除 Wait 等待，再发布完成事件。
	run.handle.setResult(result)
	if stepEnd != nil {
		a.dispatcher.enqueue(*stepEnd)
	}
	a.dispatcher.enqueue(Event{
		Kind:      EventRunEnd,
		RunID:     run.id,
		Result:    &result,
		Detail:    fmt.Sprintf("Run 执行完成 (状态: %s)", result.State),
		Timestamp: time.Now(),
	})
	a.enqueueDroppedTasks(droppedTasks)
	a.mu.Unlock()
	a.dispatcher.drain()

	// 通知被丢弃的纠正
	for _, st := range droppedSteers {
		a.dispatcher.emit(Event{
			Kind:      EventInputDropped,
			RunID:     run.id,
			Detail:    "任务已终止，未消费纠正被丢弃: " + st,
			Timestamp: time.Now(),
		})
	}

}

// 切换模型并且发布event
func (a *Agent) changeModel() {
	a.currentModel = *a.pendingModel
	a.currentOptions = *a.pendingOptions
	a.pendingModel = nil
	a.pendingOptions = nil
	a.dispatcher.enqueue(Event{
		Kind:      EventModelSwitched,
		Detail:    fmt.Sprintf("新模型已生效: %s (%s)", a.currentModel.ID, a.currentModel.API),
		Timestamp: time.Now(),
	})
}

// recordResponseAndUsage 集中记录模型最终消息、同源来源与用量统计。
func (a *Agent) recordResponseAndUsage(run *activeRun, model llm.Model, msg *llm.AssistantMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.messageOrigin[msg] = ModelOrigin{
		Provider: model.Provider,
		API:      model.API,
		ModelID:  model.ID,
		BaseURL:  model.BaseURL,
	}
	a.history = append(a.history, msg)
	run.messages = append(run.messages, msg)

	sourceKey := fmt.Sprintf("%s/%s/%s@%s", model.Provider, model.API, model.ID, model.BaseURL)
	accumulateUsage(run, sourceKey, msg.Usage)
}

// accumulateUsage 累加单次调用的用量与费用统计。
func accumulateUsage(run *activeRun, sourceKey string, u llm.Usage) {
	if u.Total == 0 && u.Input == 0 && u.Output == 0 {
		return
	}

	entry := run.usage[sourceKey]
	addUsage(&entry, u)
	run.usage[sourceKey] = entry
	addUsage(&run.totalUsage, u)
}

// canStartWorkLocked 与控制入口共享状态锁，确认模型启动的执行边界。
func (a *Agent) canStartWorkLocked(run *activeRun) bool {
	return a.executionErr == nil && run.ctx.Err() == nil && a.status == AgentStatusRunning
}

// addUsage 累加已报告的统计，不根据 Reasoning 重算 Output 或 Total。
func addUsage(dst *llm.Usage, u llm.Usage) {
	dst.Input += u.Input
	dst.Output += u.Output
	dst.Reasoning += u.Reasoning
	dst.CacheRead += u.CacheRead
	dst.CacheWrite += u.CacheWrite
	dst.Total += u.Total
	dst.Cost.Input += u.Cost.Input
	dst.Cost.Output += u.Cost.Output
	dst.Cost.CacheRead += u.Cost.CacheRead
	dst.Cost.CacheWrite += u.Cost.CacheWrite
	dst.Cost.Total += u.Cost.Total
}
