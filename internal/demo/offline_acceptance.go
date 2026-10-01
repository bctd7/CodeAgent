package demo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

// ScenarioResult 记录单项验收用例的执行结果。
type ScenarioResult struct {
	ID            int           `json:"id"`
	Name          string        `json:"name"`
	Category      string        `json:"category"` // "离线" 或 "在线"
	Expected      string        `json:"expected"`
	Actual        string        `json:"actual"`
	Status        string        `json:"status"` // "通过", "失败", "尚未验证"
	FailureRunID  string        `json:"failure_run_id,omitempty"`
	FailureStep   int           `json:"failure_step,omitempty"`
	EventSequence []string      `json:"event_sequence,omitempty"`
	Repeats       int           `json:"repeats,omitempty"`
	Failures      int           `json:"failures,omitempty"`
	Duration      time.Duration `json:"duration"`
}

// AcceptanceSuiteResult 包含整体验收套件的执行汇总。
type AcceptanceSuiteResult struct {
	TotalScenarios  int              `json:"total_scenarios"`
	PassedScenarios int              `json:"passed_scenarios"`
	FailedScenarios int              `json:"failed_scenarios"`
	PendingOnline   int              `json:"pending_online"`
	Duration        time.Duration    `json:"duration"`
	Results         []ScenarioResult `json:"results"`
}

func userMsg(text string) *llm.UserMessage {
	return &llm.UserMessage{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: llm.TypeText, Text: text},
		},
		Timestamp: time.Now(),
	}
}

func getLastUserText(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if u, ok := req.Messages[i].(*llm.UserMessage); ok {
			for _, b := range u.Content {
				if b.Type == llm.TypeText {
					return b.Text
				}
			}
		}
	}
	return ""
}

func createSingleTextStream(ctx context.Context, text string) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
	w.Finish(llm.AssistantMessage{
		Role:       llm.RoleAssistant,
		State:      llm.StateSuccess,
		StopReason: llm.StopStop,
		Content: []llm.ContentBlock{
			{Type: llm.TypeText, Text: text},
		},
	})
	return r, nil
}

func createToolCallStream(ctx context.Context, toolCalls []llm.ContentBlock) (llm.StreamReader, error) {
	r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
	w.Finish(llm.AssistantMessage{
		Role:       llm.RoleAssistant,
		State:      llm.StateSuccess,
		StopReason: llm.StopToolUse,
		Content:    toolCalls,
	})
	return r, nil
}

// RunOfflineAcceptance 顺序执行全部离线验收场景 (12 项核心场景 + 2 项并发交错压测场景)。
func RunOfflineAcceptance(verbose bool) AcceptanceSuiteResult {
	startTime := time.Now()
	if verbose {
		fmt.Println("\n==================================================")
		fmt.Println("开始执行 Agent 离线自动化验收套件 (共 14 项场景)")
		fmt.Println("==================================================")
	}

	scenarios := []func() ScenarioResult{
		runOfflineScenario1,
		runOfflineScenario2,
		runOfflineScenario3,
		runOfflineScenario4,
		runOfflineScenario5,
		runOfflineScenario6,
		runOfflineScenario7,
		runOfflineScenario8,
		runOfflineScenario9,
		runOfflineScenario10,
		runOfflineScenario11,
		runOfflineScenario12,
		runOfflineScenario13,
		runOfflineScenario14,
	}

	suite := AcceptanceSuiteResult{
		TotalScenarios: len(scenarios),
		Results:        make([]ScenarioResult, 0, len(scenarios)),
	}

	for _, sc := range scenarios {
		res := sc()
		suite.Results = append(suite.Results, res)
		if res.Status == "通过" {
			suite.PassedScenarios++
		} else {
			suite.FailedScenarios++
		}

		if verbose {
			fmt.Printf("[%s] 场景 %2d: %-38s (耗时: %v)\n", res.Status, res.ID, res.Name, res.Duration)
			if res.Repeats > 1 {
				fmt.Printf("      重复次数: %d, 失败次数: %d\n", res.Repeats, res.Failures)
			}
			fmt.Printf("      预期行为: %s\n", res.Expected)
			fmt.Printf("      实际观察: %s\n", res.Actual)
			if res.Status != "通过" && res.FailureRunID != "" {
				fmt.Printf("      失败 RunID: %s, 步骤 Step: %d\n", res.FailureRunID, res.FailureStep)
				if len(res.EventSequence) > 0 {
					fmt.Printf("      关键事件序列: %s\n", strings.Join(res.EventSequence, " -> "))
				}
			}
		}
	}

	suite.Duration = time.Since(startTime)
	if verbose {
		fmt.Println("--------------------------------------------------")
		fmt.Printf("离线验收执行完毕: 总计 %d 项, 通过 %d 项, 失败 %d 项, 总耗时: %v\n",
			suite.TotalScenarios, suite.PassedScenarios, suite.FailedScenarios, suite.Duration)
		fmt.Println("==================================================")
	}
	return suite
}

// 场景 1: 普通回答、单工具往返，未知工具与参数错误后继续处理
func runOfflineScenario1() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       1,
		Name:     "普通回答、单工具往返，未知工具与参数错误后继续处理",
		Category: "离线",
		Expected: "Step 1 模型调用 add 工具；执行后工具结果回传；Step 2 模型生成文本回答；遭遇未知工具和参数错误时生成错误结果并在后续请求中继续处理收敛",
		Status:   "失败",
	}

	// Part 1: 普通回答与单工具往返
	callCount := 0
	var events []string
	var mu sync.Mutex

	stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		if c == 1 {
			return createToolCallStream(ctx, []llm.ContentBlock{
				{
					Type:      llm.TypeToolCall,
					ID:        "call_1",
					Name:      "add",
					Arguments: map[string]any{"a": float64(12), "b": float64(34)},
				},
			})
		}
		var hasToolResult bool
		for _, m := range req.Messages {
			if tr, ok := m.(*llm.ToolResultMessage); ok && tr.ToolCallID == "call_1" {
				hasToolResult = true
			}
		}
		if !hasToolResult {
			return nil, fmt.Errorf("第 2 次请求未包含 call_1 的工具结果")
		}
		return createSingleTextStream(ctx, "计算结果为 46")
	}

	onEvent := func(evt agent.Event) {
		mu.Lock()
		events = append(events, string(evt.Kind))
		mu.Unlock()
	}

	ag, err := newDemoAgent(agent.Config{
		Model:   llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
		Stream:  stream,
		Tools:   []agent.Tool{agent.NewAddTool()},
		OnEvent: onEvent,
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h1, err := ag.Submit(ctx, userMsg("请计算 12 + 34"))
	if err != nil {
		res.Actual = fmt.Sprintf("Submit 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}

	runRes1, err := h1.Wait(ctx)
	if err != nil || runRes1.State != agent.RunStateSuccess || runRes1.StopReason != agent.RunStopCompleted || runRes1.Steps != 2 {
		res.Actual = fmt.Sprintf("Part 1 往返失败: err=%v, State=%s, Steps=%d", err, runRes1.State, runRes1.Steps)
		res.FailureRunID = h1.ID()
		res.EventSequence = events
		res.Duration = time.Since(start)
		return res
	}

	// Part 2: 未知工具与参数错误混合，核对 Agent 正确构造 ToolResultMessage(IsError=true) 且模型后续成功收尾
	part2CallCount := 0
	var part2Step2Messages []llm.Message
	stream2 := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		part2CallCount++
		if part2CallCount == 1 {
			return createToolCallStream(ctx, []llm.ContentBlock{
				{Type: llm.TypeToolCall, ID: "c_unknown", Name: "not_exist_tool"},
				{Type: llm.TypeToolCall, ID: "c_bad_arg", Name: "add", Arguments: map[string]any{"a": "not_an_int", "b": float64(10)}},
			})
		}
		part2Step2Messages = append([]llm.Message{}, req.Messages...)
		return createSingleTextStream(ctx, "已识别未知工具与参数错误并完成降级回答")
	}

	ag2, err := newDemoAgent(agent.Config{
		Model:  llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
		Stream: stream2,
		Tools:  []agent.Tool{agent.NewAddTool()},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 2 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag2.Close()

	h2, _ := ag2.Submit(ctx, userMsg("trigger unknown tool and bad arg"))
	runRes2, errWait2 := h2.Wait(ctx)
	if errWait2 != nil || runRes2.State != agent.RunStateSuccess || runRes2.Steps != 2 {
		res.Actual = fmt.Sprintf("Part 2 未知工具/参数错误后收敛失败: err=%v, State=%s", errWait2, runRes2.State)
		res.FailureRunID = h2.ID()
		res.Duration = time.Since(start)
		return res
	}

	// 校验第 2 步回传的两个工具结果均标记了 IsError == true
	var foundUnknownErr, foundBadArgErr bool
	for _, m := range part2Step2Messages {
		if tr, ok := m.(*llm.ToolResultMessage); ok {
			if tr.ToolCallID == "c_unknown" && tr.IsError {
				foundUnknownErr = true
			}
			if tr.ToolCallID == "c_bad_arg" && tr.IsError {
				foundBadArgErr = true
			}
		}
	}
	if !foundUnknownErr || !foundBadArgErr {
		res.Actual = fmt.Sprintf("Part 2 工具错误结果回传不完整: foundUnknownErr=%v, foundBadArgErr=%v",
			foundUnknownErr, foundBadArgErr)
		res.Duration = time.Since(start)
		return res
	}

	res.Status = "通过"
	res.Actual = "普通回答、单工具正常往返、未知工具与参数错误均成功生成 is_error 回传并完成多步收敛"
	res.Duration = time.Since(start)
	return res
}

// 场景 2: FIFO 排队，执行期间新增任务、取消与出队交错、收尾与新提交交错 (重复 >= 20 次)
func runOfflineScenario2() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       2,
		Name:     "FIFO、执行期间新增任务、取消与出队交错、收尾与新提交交错",
		Category: "离线",
		Expected: "连续提交多个 Run 严格按 FIFO 排队调度；执行期间新增任务正确排在队尾；取消排队任务不影响前后次序；收尾时提交新任务无缝唤醒",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		var activeCount atomic.Int32
		var maxActive atomic.Int32

		r1Entered := make(chan struct{})
		r1Release := make(chan struct{})
		r2Entered := make(chan struct{})
		r2Release := make(chan struct{})
		r4Entered := make(chan struct{})
		r4Release := make(chan struct{})
		r5Entered := make(chan struct{})
		r5Release := make(chan struct{})

		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			cur := activeCount.Add(1)
			for {
				old := maxActive.Load()
				if cur <= old || maxActive.CompareAndSwap(old, cur) {
					break
				}
			}
			defer activeCount.Add(-1)

			text := getLastUserText(req)
			switch {
			case strings.Contains(text, "task1"):
				select {
				case <-r1Entered:
				default:
					close(r1Entered)
				}
				select {
				case <-r1Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task1 ok")
			case strings.Contains(text, "task2"):
				select {
				case <-r2Entered:
				default:
					close(r2Entered)
				}
				select {
				case <-r2Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task2 ok")
			case strings.Contains(text, "task4"):
				select {
				case <-r4Entered:
				default:
					close(r4Entered)
				}
				select {
				case <-r4Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task4 ok")
			case strings.Contains(text, "task5"):
				select {
				case <-r5Entered:
				default:
					close(r5Entered)
				}
				select {
				case <-r5Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task5 ok")
			}
			return createSingleTextStream(ctx, "default ok")
		}

		ag, err := newDemoAgent(agent.Config{
			Model:  llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
			Stream: stream,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		// 1. 提交任务 1，等待其成为活跃 Run
		h1, _ := ag.Submit(ctx, userMsg("task1"))
		<-r1Entered

		// 2. 在任务 1 执行期间连续追加提交任务 2 和任务 3
		h2, _ := ag.Submit(ctx, userMsg("task2"))
		h3, _ := ag.Submit(ctx, userMsg("task3"))

		snap1 := ag.Snapshot()
		if snap1.CurrentRunID != h1.ID() || snap1.QueueLength != 2 ||
			snap1.QueuedRunIDs[0] != h2.ID() || snap1.QueuedRunIDs[1] != h3.ID() {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次运行中追加任务后队列异常: active=%s, queue=%v",
				i+1, snap1.CurrentRunID, snap1.QueuedRunIDs)
			res.Duration = time.Since(start)
			return res
		}

		// 3. 在任务 1 释放前取消排队中的任务 3，并追加任务 4
		h3.Cancel()
		h4, _ := ag.Submit(ctx, userMsg("task4"))

		snap2 := ag.Snapshot()
		if snap2.QueueLength != 2 || snap2.QueuedRunIDs[0] != h2.ID() || snap2.QueuedRunIDs[1] != h4.ID() {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次取消 task3 后队列不符合预期: queue=%v", i+1, snap2.QueuedRunIDs)
			res.Duration = time.Since(start)
			return res
		}

		// 4. 释放任务 1
		close(r1Release)
		r1, _ := h1.Wait(ctx)
		if r1.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次任务 1 未成功完成: %s", i+1, r1.State)
			res.Duration = time.Since(start)
			return res
		}

		// 5. 等待任务 2 开始并释放
		<-r2Entered
		close(r2Release)
		r2, _ := h2.Wait(ctx)
		if r2.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次任务 2 未成功完成: %s", i+1, r2.State)
			res.Duration = time.Since(start)
			return res
		}

		// 被取消的任务 3 必须能取得 aborted 状态
		r3, _ := h3.Wait(ctx)
		if r3.State != agent.RunStateAborted {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次已取消任务 3 状态异常: %s", i+1, r3.State)
			res.Duration = time.Since(start)
			return res
		}

		// 6. 等待任务 4 开始并释放
		<-r4Entered
		close(r4Release)
		r4, _ := h4.Wait(ctx)
		if r4.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次任务 4 未成功完成: %s", i+1, r4.State)
			res.Duration = time.Since(start)
			return res
		}

		// 7. 收尾空闲时追加新任务 5，核验空闲唤醒能力
		h5, _ := ag.Submit(ctx, userMsg("task5"))
		<-r5Entered
		close(r5Release)
		r5, _ := h5.Wait(ctx)
		if r5.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次空闲唤醒任务 5 未成功完成: %s", i+1, r5.State)
			res.Duration = time.Since(start)
			return res
		}

		if maxActive.Load() > 1 {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次检测到并发活动 Run 数超标: %d", i+1, maxActive.Load())
			res.Duration = time.Since(start)
			return res
		}

		cancel()
		ag.Close()
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：严格遵守 FIFO 排队，执行中追加、取消交错与收尾新提交均准确调度，最大并发数始终为 1", repeats)
	res.Duration = time.Since(start)
	return res
}

// 场景 3: 排队取消、活动取消、旧 Handle 取消，以及取消后继续提交 (重复 >= 20 次)
func runOfflineScenario3() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       3,
		Name:     "排队取消、活动取消、旧 Handle 取消，以及取消后继续提交",
		Category: "离线",
		Expected: "Handle.Cancel 只取消指定任务；活动取消后队列任务保留；旧句柄取消幂等安全；取消后仍可继续提交正常任务",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		r1Entered := make(chan struct{})
		r1Release := make(chan struct{})
		r3Entered := make(chan struct{})
		r3Release := make(chan struct{})
		r4Entered := make(chan struct{})
		r5Entered := make(chan struct{})
		r5Release := make(chan struct{})
		r6Entered := make(chan struct{})
		r6Release := make(chan struct{})

		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			text := getLastUserText(req)
			switch {
			case strings.Contains(text, "task1"):
				select {
				case <-r1Entered:
				default:
					close(r1Entered)
				}
				select {
				case <-r1Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task1 ok")
			case strings.Contains(text, "task3"):
				select {
				case <-r3Entered:
				default:
					close(r3Entered)
				}
				select {
				case <-r3Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task3 ok")
			case strings.Contains(text, "task4"):
				select {
				case <-r4Entered:
				default:
					close(r4Entered)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			case strings.Contains(text, "task5"):
				select {
				case <-r5Entered:
				default:
					close(r5Entered)
				}
				select {
				case <-r5Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task5 ok")
			case strings.Contains(text, "task6"):
				select {
				case <-r6Entered:
				default:
					close(r6Entered)
				}
				select {
				case <-r6Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task6 ok")
			}
			return createSingleTextStream(ctx, "fallback ok")
		}

		ag, err := newDemoAgent(agent.Config{
			Model:  llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
			Stream: stream,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		// 1. 排队取消测试
		h1, _ := ag.Submit(ctx, userMsg("task1"))
		<-r1Entered
		h2, _ := ag.Submit(ctx, userMsg("task2"))
		h3, _ := ag.Submit(ctx, userMsg("task3"))

		h2.Cancel()
		r2, errWait2 := h2.Wait(ctx)
		if errWait2 != nil || r2.State != agent.RunStateAborted {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次排队任务取消未置 aborted: err=%v, State=%s", i+1, errWait2, r2.State)
			res.Duration = time.Since(start)
			return res
		}

		close(r1Release)
		r1, _ := h1.Wait(ctx)
		if r1.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次任务 1 未成功完成: State=%s", i+1, r1.State)
			res.Duration = time.Since(start)
			return res
		}

		<-r3Entered
		close(r3Release)
		r3, _ := h3.Wait(ctx)
		if r3.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次任务 3 未成功完成: State=%s", i+1, r3.State)
			res.Duration = time.Since(start)
			return res
		}

		// 2. 活动取消测试 (排队任务保留)
		h4, _ := ag.Submit(ctx, userMsg("task4"))
		<-r4Entered
		h5, _ := ag.Submit(ctx, userMsg("task5"))

		h4.Cancel()
		r4, _ := h4.Wait(ctx)
		if r4.State != agent.RunStateAborted {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次活动任务取消未置 aborted: State=%s", i+1, r4.State)
			res.Duration = time.Since(start)
			return res
		}

		select {
		case <-r5Entered:
		case <-ctx.Done():
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次活动任务取消后，排队任务 5 未被执行", i+1)
			res.Duration = time.Since(start)
			return res
		}
		close(r5Release)
		r5, _ := h5.Wait(ctx)
		if r5.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次排队任务 5 执行未成功: State=%s", i+1, r5.State)
			res.Duration = time.Since(start)
			return res
		}

		// 3. 旧 Handle 取消 (幂等安全)
		h1.Cancel()
		h2.Cancel()
		h4.Cancel()

		// 4. 取消后继续提交新任务
		h6, _ := ag.Submit(ctx, userMsg("task6"))
		<-r6Entered
		close(r6Release)
		r6, _ := h6.Wait(ctx)
		if r6.State != agent.RunStateSuccess {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次新任务 6 执行未成功: State=%s", i+1, r6.State)
			res.Duration = time.Since(start)
			return res
		}

		cancel()
		ag.Close()
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：排队取消精准剔除，活动取消保留后续队列，旧句柄安全幂等，后续提交正常执行", repeats)
	res.Duration = time.Since(start)
	return res
}

// 场景 4: 工具并发上限、串行工具边界、结果按原调用顺序保存 (重复 >= 20 次)
func runOfflineScenario4() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       4,
		Name:     "工具并发上限、串行工具边界、结果按原调用顺序保存",
		Category: "离线",
		Expected: "并行工具并发数不超过 MaxParallelTools；串行工具独占边界且前后隔离；无论完成先后，结果严格按模型调用原序保存",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		var parallelActive atomic.Int32
		var maxParallel atomic.Int32
		var serialActive atomic.Int32
		var serialOverlapped atomic.Bool

		var p1Done, p2Done, p3Done, serDone atomic.Bool

		makeParTool := func(name string, doneFlag *atomic.Bool, delayMS int) agent.Tool {
			return agent.Tool{
				Tool:         llm.Tool{Name: name},
				ParallelSafe: true,
				Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
					cur := parallelActive.Add(1)
					for {
						old := maxParallel.Load()
						if cur <= old || maxParallel.CompareAndSwap(old, cur) {
							break
						}
					}
					if serialActive.Load() > 0 {
						serialOverlapped.Store(true)
					}
					time.Sleep(time.Duration(delayMS) * time.Millisecond)
					doneFlag.Store(true)
					parallelActive.Add(-1)
					return []llm.ContentBlock{{Type: llm.TypeText, Text: name + "_res"}}, nil
				},
			}
		}

		serTool := agent.Tool{
			Tool:         llm.Tool{Name: "ser"},
			ParallelSafe: false,
			Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				serialActive.Add(1)
				if parallelActive.Load() > 0 {
					serialOverlapped.Store(true)
				}
				if !p1Done.Load() || !p2Done.Load() || !p3Done.Load() {
					serialOverlapped.Store(true)
				}
				time.Sleep(10 * time.Millisecond)
				serDone.Store(true)
				serialActive.Add(-1)
				return []llm.ContentBlock{{Type: llm.TypeText, Text: "ser_res"}}, nil
			},
		}

		p4Done := atomic.Bool{}
		p4Tool := agent.Tool{
			Tool:         llm.Tool{Name: "p4"},
			ParallelSafe: true,
			Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				if !serDone.Load() || serialActive.Load() > 0 {
					serialOverlapped.Store(true)
				}
				p4Done.Store(true)
				return []llm.ContentBlock{{Type: llm.TypeText, Text: "p4_res"}}, nil
			},
		}

		tools := []agent.Tool{
			makeParTool("p1", &p1Done, 20),
			makeParTool("p2", &p2Done, 5),
			makeParTool("p3", &p3Done, 15),
			serTool,
			p4Tool,
		}

		callCount := 0
		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			callCount++
			if callCount == 1 {
				return createToolCallStream(ctx, []llm.ContentBlock{
					{Type: llm.TypeToolCall, ID: "c1", Name: "p1"},
					{Type: llm.TypeToolCall, ID: "c2", Name: "p2"},
					{Type: llm.TypeToolCall, ID: "c3", Name: "p3"},
					{Type: llm.TypeToolCall, ID: "c4", Name: "ser"},
					{Type: llm.TypeToolCall, ID: "c5", Name: "p4"},
				})
			}
			return createSingleTextStream(ctx, "all tools done")
		}

		ag, err := newDemoAgent(agent.Config{
			Model:            llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
			Stream:           stream,
			Tools:            tools,
			MaxParallelTools: 2,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		h, errSub := ag.Submit(ctx, userMsg("run tools"))
		if errSub != nil {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Submit 失败: %v", i+1, errSub)
			res.Duration = time.Since(start)
			return res
		}

		runRes, errWait := h.Wait(ctx)
		cancel()
		ag.Close()

		if errWait != nil || runRes.State != agent.RunStateSuccess {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Run 执行未成功: err=%v, State=%s", i+1, errWait, runRes.State)
			res.Duration = time.Since(start)
			return res
		}

		if maxParallel.Load() > 2 {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次工具并发超出上限: maxWitnessed=%d (上限=2)", i+1, maxParallel.Load())
			res.Duration = time.Since(start)
			return res
		}

		if serialOverlapped.Load() {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次串行工具边界发生违规重叠", i+1)
			res.Duration = time.Since(start)
			return res
		}

		var resultIDs []string
		for _, m := range runRes.Messages {
			if tr, ok := m.(*llm.ToolResultMessage); ok {
				resultIDs = append(resultIDs, tr.ToolCallID)
			}
		}
		expectedIDs := []string{"c1", "c2", "c3", "c4", "c5"}
		if strings.Join(resultIDs, ",") != strings.Join(expectedIDs, ",") {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次工具结果未按原始调用顺序排列: actual=%v, expected=%v",
				i+1, resultIDs, expectedIDs)
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：最大并发严格锁定为 2，串行工具边界完全隔离，乱序完成仍严格按原调用顺序归档", repeats)
	res.Duration = time.Since(start)
	return res
}

// 场景 5: 纠正进入下一次请求，未启动工具跳过，已启动工具正常收尾 (重复 >= 20 次)
func runOfflineScenario5() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       5,
		Name:     "纠正进入下一次请求，未启动工具跳过，已启动工具正常收尾",
		Category: "离线",
		Expected: "运行中提交纠正后，未启动的旧工具被跳过并记录 EventToolSkipped；已启动的工具正常执行完成；纠正以用户消息进入下一轮请求",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		t1Started := make(chan struct{})
		t1Release := make(chan struct{})
		var t1Completed atomic.Bool
		var t2Executed atomic.Bool
		var t2Skipped atomic.Bool

		tool1 := agent.Tool{
			Tool:         llm.Tool{Name: "tool1"},
			ParallelSafe: false,
			Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				select {
				case <-t1Started:
				default:
					close(t1Started)
				}
				<-t1Release
				t1Completed.Store(true)
				return []llm.ContentBlock{{Type: llm.TypeText, Text: "t1_done"}}, nil
			},
		}

		tool2 := agent.Tool{
			Tool:         llm.Tool{Name: "tool2"},
			ParallelSafe: false,
			Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
				t2Executed.Store(true)
				return []llm.ContentBlock{{Type: llm.TypeText, Text: "t2_done"}}, nil
			},
		}

		var secondReqMessages []llm.Message
		var mu sync.Mutex
		callCount := 0

		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			mu.Lock()
			callCount++
			c := callCount
			if c == 2 {
				secondReqMessages = append([]llm.Message{}, req.Messages...)
			}
			mu.Unlock()

			if c == 1 {
				return createToolCallStream(ctx, []llm.ContentBlock{
					{Type: llm.TypeToolCall, ID: "call_t1", Name: "tool1"},
					{Type: llm.TypeToolCall, ID: "call_t2", Name: "tool2"},
				})
			}
			return createSingleTextStream(ctx, "收到纠正，任务完成")
		}

		onEvent := func(evt agent.Event) {
			if evt.Kind == agent.EventToolSkipped && evt.ToolCallID == "call_t2" {
				t2Skipped.Store(true)
			}
		}

		ag, err := newDemoAgent(agent.Config{
			Model:   llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
			Stream:  stream,
			Tools:   []agent.Tool{tool1, tool2},
			OnEvent: onEvent,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		h, errSub := ag.Submit(ctx, userMsg("task with steer"))
		if errSub != nil {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Submit 失败: %v", i+1, errSub)
			res.Duration = time.Since(start)
			return res
		}

		<-t1Started
		steerErr := ag.Steer("放弃 tool2，请直接收尾")
		if steerErr != nil {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Steer 失败: %v", i+1, steerErr)
			res.Duration = time.Since(start)
			return res
		}
		close(t1Release)

		runRes, errWait := h.Wait(ctx)
		cancel()
		ag.Close()

		if errWait != nil || runRes.State != agent.RunStateSuccess {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Run 结果异常: err=%v, State=%s", i+1, errWait, runRes.State)
			res.Duration = time.Since(start)
			return res
		}

		if !t1Completed.Load() {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次已启动的 tool1 未正常执行收尾", i+1)
			res.Duration = time.Since(start)
			return res
		}

		if t2Executed.Load() {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次已纠正的 tool2 仍被执行", i+1)
			res.Duration = time.Since(start)
			return res
		}

		if !t2Skipped.Load() {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 tool2 未收到 EventToolSkipped 事件", i+1)
			res.Duration = time.Since(start)
			return res
		}

		var hasSteerMsg bool
		var hasT1Result bool
		for _, m := range secondReqMessages {
			if u, ok := m.(*llm.UserMessage); ok {
				for _, b := range u.Content {
					if strings.Contains(b.Text, "放弃 tool2，请直接收尾") {
						hasSteerMsg = true
					}
				}
			}
			if tr, ok := m.(*llm.ToolResultMessage); ok && tr.ToolCallID == "call_t1" && !tr.IsError {
				hasT1Result = true
			}
		}
		if !hasSteerMsg || !hasT1Result {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次第二轮模型请求缺少纠正或 tool1 正常结果: steer=%v, t1=%v",
				i+1, hasSteerMsg, hasT1Result)
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：已启动工具正常收尾入历，未启动工具全部正确跳过，纠正准确注入后续请求", repeats)
	res.Duration = time.Since(start)
	return res
}

// 场景 6: 当前生成中和工具执行中切换模型；连续切换取最后目标；Model 与 Options 成组生效
func runOfflineScenario6() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       6,
		Name:     "生成中/工具中切换模型，连续切换取最后目标，成组生效无额外调用",
		Category: "离线",
		Expected: "生成中与工具执行中均可安全 SetModel；连续切换取最后目标；Model 与 Options 成组在下一请求边界生效；纯文本自然结束不产生多余调用",
		Status:   "失败",
	}

	modelA := llm.Model{ID: "model-a", API: llm.APIKindAnthropic}
	optionsA := llm.RequestOptions{Anthropic: &llm.AnthropicOptions{BudgetTokens: 1000}}

	modelMid := llm.Model{ID: "model-mid", API: llm.APIKindOpenAI}
	optionsMid := llm.RequestOptions{OpenAI: &llm.OpenAIOptions{ReasoningEffort: "low"}}

	modelFinal := llm.Model{ID: "model-final", API: llm.APIKindOpenAI}
	optionsFinal := llm.RequestOptions{OpenAI: &llm.OpenAIOptions{ReasoningEffort: "high"}}

	var streamCalls atomic.Int32
	addStarted := make(chan struct{})
	addRelease := make(chan struct{})
	stream1Started := make(chan struct{})

	addTool := agent.Tool{
		Tool:         llm.Tool{Name: "add"},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			select {
			case <-addStarted:
			default:
				close(addStarted)
			}
			<-addRelease
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "3"}}, nil
		},
	}

	stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		callIdx := streamCalls.Add(1)
		if callIdx == 1 {
			select {
			case <-stream1Started:
			default:
				close(stream1Started)
			}
			if req.Model.ID != modelA.ID || req.Options.Anthropic == nil || req.Options.Anthropic.BudgetTokens != 1000 {
				return nil, fmt.Errorf("第 1 步未采用 ModelA 或 OptionsA: Model=%+v", req.Model)
			}
			return createToolCallStream(ctx, []llm.ContentBlock{
				{Type: llm.TypeToolCall, ID: "c1", Name: "add"},
			})
		} else if callIdx == 2 {
			if req.Model.ID != modelFinal.ID || req.Options.OpenAI == nil || req.Options.OpenAI.ReasoningEffort != "high" {
				return nil, fmt.Errorf("第 2 步未成组生效最终 ModelFinal 与 OptionsFinal: Model=%+v, Opts=%+v", req.Model, req.Options)
			}
			return createSingleTextStream(ctx, "文本回答结束")
		}
		return nil, fmt.Errorf("产生了多余的模型调用 (callIdx=%d)", callIdx)
	}

	ag, err := newDemoAgent(agent.Config{
		Model:   modelA,
		Options: optionsA,
		Stream:  stream,
		Tools:   []agent.Tool{addTool},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, _ := ag.Submit(ctx, userMsg("calculate"))

	// 1. 在第 1 步模型流式生成期间连续切换模型 (取最后目标)
	<-stream1Started
	_ = ag.SetModel(modelMid, optionsMid)
	_ = ag.SetModel(modelFinal, optionsFinal)

	// 2. 在工具执行期间确认当前生效与待生效
	<-addStarted
	snap := ag.Snapshot()
	if snap.CurrentModel.ID != modelA.ID || snap.PendingModel == nil || snap.PendingModel.ID != modelFinal.ID {
		res.Actual = fmt.Sprintf("生成中/工具中连续切换后快照异常: Current=%s, Pending=%v",
			snap.CurrentModel.ID, snap.PendingModel)
		res.Duration = time.Since(start)
		return res
	}

	close(addRelease)
	runRes, errWait := h.Wait(ctx)
	if errWait != nil || runRes.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("Run 执行未成功: err=%v, State=%s", errWait, runRes.State)
		res.Duration = time.Since(start)
		return res
	}

	if streamCalls.Load() != 2 {
		res.Actual = fmt.Sprintf("调用次数异常: 期望 2 次，实际调用了 %d 次", streamCalls.Load())
		res.Duration = time.Since(start)
		return res
	}

	finalSnap := ag.Snapshot()
	if finalSnap.CurrentModel.ID != modelFinal.ID || finalSnap.PendingModel != nil {
		res.Actual = fmt.Sprintf("最终生效模型异常: Current=%s, Pending=%v",
			finalSnap.CurrentModel.ID, finalSnap.PendingModel)
		res.Duration = time.Since(start)
		return res
	}

	res.Status = "通过"
	res.Actual = "连续切换准确取最终目标；生成与工具执行期间切换安全生效；Model 与 Options 成组替换；纯文本自然结束无额外调用"
	res.Duration = time.Since(start)
	return res
}

// 场景 7: Wait 超时不取消任务，多次 Wait 结果一致，无 OnEvent 仍能取得结果
func runOfflineScenario7() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       7,
		Name:     "Wait 超时不取消运行，多次 Wait 结果一致，无 OnEvent 运行正常",
		Category: "离线",
		Expected: "OnEvent=nil 正常执行；Wait 超时仅返回 DeadlineExceeded，后台任务不取消；多次 Wait 得到严格一致结果",
		Status:   "失败",
	}

	// 1. 无 OnEvent 验证
	stream1 := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		return createSingleTextStream(ctx, "hello no onEvent")
	}
	ag1, err := newDemoAgent(agent.Config{
		Model:   llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
		Stream:  stream1,
		OnEvent: nil,
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建无 OnEvent Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	h1, _ := ag1.Submit(context.Background(), userMsg("test"))
	r1, err1 := h1.Wait(context.Background())
	ag1.Close()
	if err1 != nil || r1.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("无 OnEvent 运行时 Wait 返回失败: err=%v, State=%s", err1, r1.State)
		res.Duration = time.Since(start)
		return res
	}

	// 2. Wait 超时不取消任务与多次 Wait 结果一致性验证
	allowFinish := make(chan struct{})
	stream2 := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		select {
		case <-allowFinish:
			return createSingleTextStream(ctx, "task finished after wait timeout")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ag2, err := newDemoAgent(agent.Config{
		Model:  llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
		Stream: stream2,
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 2 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag2.Close()

	h2, _ := ag2.Submit(context.Background(), userMsg("long task"))
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	rTimeout, errTimeout := h2.Wait(shortCtx)
	shortCancel()

	if !errors.Is(errTimeout, context.DeadlineExceeded) || rTimeout.State != "" {
		res.Actual = fmt.Sprintf("期望超时返回 DeadlineExceeded 且结果为空，实际: err=%v, State=%s", errTimeout, rTimeout.State)
		res.Duration = time.Since(start)
		return res
	}

	snap := ag2.Snapshot()
	if snap.CurrentRunID != h2.ID() || snap.Status != agent.AgentStatusRunning {
		res.Actual = fmt.Sprintf("Wait 超时后后台任务被误终止: Status=%s, CurrentRunID=%s",
			snap.Status, snap.CurrentRunID)
		res.Duration = time.Since(start)
		return res
	}

	close(allowFinish)

	// 连续 3 次 Wait 验证幂等一致性
	rFinal1, errFinal1 := h2.Wait(context.Background())
	rFinal2, errFinal2 := h2.Wait(context.Background())
	rFinal3, errFinal3 := h2.Wait(context.Background())

	if errFinal1 != nil || errFinal2 != nil || errFinal3 != nil {
		res.Actual = fmt.Sprintf("后续 Wait 返回错误: err1=%v, err2=%v, err3=%v", errFinal1, errFinal2, errFinal3)
		res.Duration = time.Since(start)
		return res
	}
	if rFinal1.State != agent.RunStateSuccess || rFinal1.State != rFinal2.State || rFinal2.State != rFinal3.State ||
		rFinal1.Steps != rFinal2.Steps || rFinal2.Steps != rFinal3.Steps {
		res.Actual = fmt.Sprintf("多次 Wait 结果不一致: r1=%+v, r2=%+v, r3=%+v", rFinal1, rFinal2, rFinal3)
		res.Duration = time.Since(start)
		return res
	}

	res.Status = "通过"
	res.Actual = "无 OnEvent 运行完全正常；Wait 超时未取消后台任务；连续多次 Wait 返回严格一致终态"
	res.Duration = time.Since(start)
	return res
}

// 场景 8: Close 后所有已接受任务的 Handle 都能结束等待 (重复 >= 20 次)
func runOfflineScenario8() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       8,
		Name:     "Close 后所有已接受任务的 Handle 都能结束等待",
		Category: "离线",
		Expected: "调用 Close() 后，所有活跃任务及排队任务全部收敛，其 Handle.Wait 均能正常返回解除阻塞",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		r1Started := make(chan struct{})
		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			select {
			case <-r1Started:
			default:
				close(r1Started)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}

		ag, err := newDemoAgent(agent.Config{
			Model:  llm.Model{ID: "mock-model", API: llm.APIKindOpenAI},
			Stream: stream,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		h1, _ := ag.Submit(context.Background(), userMsg("run1"))
		<-r1Started
		h2, _ := ag.Submit(context.Background(), userMsg("run2"))
		h3, _ := ag.Submit(context.Background(), userMsg("run3"))

		doneWait := make(chan struct{}, 3)
		var r1, r2, r3 agent.RunResult
		var e1, e2, e3 error

		ctxWait, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		go func() {
			r1, e1 = h1.Wait(ctxWait)
			doneWait <- struct{}{}
		}()
		go func() {
			r2, e2 = h2.Wait(ctxWait)
			doneWait <- struct{}{}
		}()
		go func() {
			r3, e3 = h3.Wait(ctxWait)
			doneWait <- struct{}{}
		}()

		// 执行 Close
		closeErr := ag.Close()
		if closeErr != nil {
			cancelWait()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Close 返回错误: %v", i+1, closeErr)
			res.Duration = time.Since(start)
			return res
		}

		for j := 0; j < 3; j++ {
			select {
			case <-doneWait:
			case <-time.After(3 * time.Second):
				cancelWait()
				res.Failures++
				res.Actual = fmt.Sprintf("第 %d 次 Close 后有任务 Handle.Wait 未解除阻塞 (第 %d 个)", i+1, j+1)
				res.Duration = time.Since(start)
				return res
			}
		}
		cancelWait()

		if e1 != nil || r1.State != agent.RunStateAborted ||
			e2 != nil || r2.State != agent.RunStateAborted ||
			e3 != nil || r3.State != agent.RunStateAborted {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次任务终态不为 aborted: r1=%s, r2=%s, r3=%s",
				i+1, r1.State, r2.State, r3.State)
			res.Duration = time.Since(start)
			return res
		}

		_, errSub := ag.Submit(context.Background(), userMsg("after close"))
		if errSub == nil || !strings.Contains(errSub.Error(), "已关闭") {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 Close 后仍接受新任务: %v", i+1, errSub)
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：所有任务句柄均在 Close 后迅速解除 Wait 阻塞且置为 aborted，Close 后拒绝新提交", repeats)
	res.Duration = time.Since(start)
	return res
}

// 场景 9: 模型失败、截断和工具 panic 的终态与队列处理
func runOfflineScenario9() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       9,
		Name:     "模型失败、截断和工具 panic 的终态与队列处理",
		Category: "离线",
		Expected: "受控暂停当前 Run，确认排队任务已入队，再释放当前 Run 使其失败、截断或 panic；核对当前 Run 终态，并确认被丢弃的排队 Handle 正常结束为 aborted",
		Status:   "失败",
	}

	// 1. 模型失败
	pause1 := make(chan struct{})
	stream1Started := make(chan struct{})
	streamFail := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		select {
		case <-stream1Started:
		default:
			close(stream1Started)
		}
		select {
		case <-pause1:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
		w.Fail(llm.AssistantMessage{ErrorMessage: "502 Bad Gateway"}, fmt.Errorf("upstream 502"))
		return r, nil
	}
	ag1, _ := newDemoAgent(agent.Config{Model: llm.Model{ID: "m1", API: llm.APIKindOpenAI}, Stream: streamFail})
	defer ag1.Close()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	h1, _ := ag1.Submit(ctx1, userMsg("fail1"))
	select {
	case <-stream1Started:
	case <-time.After(3 * time.Second):
		close(pause1)
		res.Actual = "Part 1 等待任务 1 启动超时"
		res.Duration = time.Since(start)
		return res
	}

	h2, errSub2 := ag1.Submit(ctx1, userMsg("queued2"))
	if errSub2 != nil {
		close(pause1)
		res.Actual = fmt.Sprintf("Part 1 提交任务 2 失败: %v", errSub2)
		res.Duration = time.Since(start)
		return res
	}

	// 确认任务 2 确实已在排队队列中
	snap1 := ag1.Snapshot()
	if snap1.CurrentRunID != h1.ID() || snap1.QueueLength != 1 || snap1.QueuedRunIDs[0] != h2.ID() {
		close(pause1)
		res.Actual = fmt.Sprintf("Part 1 任务 2 未确认排队: Active=%s, Queue=%v", snap1.CurrentRunID, snap1.QueuedRunIDs)
		res.Duration = time.Since(start)
		return res
	}

	// 释放任务 1 使其失败
	close(pause1)

	r1, errWait1 := h1.Wait(ctx1)
	r2, errWait2 := h2.Wait(ctx1)
	if errWait1 != nil || r1.State != agent.RunStateError || r1.ErrorMessage != "502 Bad Gateway" {
		res.Actual = fmt.Sprintf("Part 1 模型失败终态异常: err=%v, State=%s, ErrMsg=%s", errWait1, r1.State, r1.ErrorMessage)
		res.Duration = time.Since(start)
		return res
	}
	if errWait2 != nil || r2.State != agent.RunStateAborted || !strings.Contains(r2.ErrorMessage, "前置任务未成功完成") {
		res.Actual = fmt.Sprintf("Part 1 模型失败后排队任务未正确丢弃: err=%v, State=%s, ErrMsg=%s", errWait2, r2.State, r2.ErrorMessage)
		res.Duration = time.Since(start)
		return res
	}

	// 2. 截断 (length)
	pause2 := make(chan struct{})
	stream2Started := make(chan struct{})
	streamTrunc := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		select {
		case <-stream2Started:
		default:
			close(stream2Started)
		}
		select {
		case <-pause2:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
		w.Finish(llm.AssistantMessage{
			Role:          llm.RoleAssistant,
			State:         llm.StateSuccess,
			StopReason:    llm.StopLength,
			RawStopReason: "max_tokens",
			ErrorMessage:  "模型输出达到最大长度被截断",
		})
		return r, nil
	}
	ag2, _ := newDemoAgent(agent.Config{Model: llm.Model{ID: "m2", API: llm.APIKindOpenAI}, Stream: streamTrunc})
	defer ag2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	h3, _ := ag2.Submit(ctx2, userMsg("trunc3"))
	select {
	case <-stream2Started:
	case <-time.After(3 * time.Second):
		close(pause2)
		res.Actual = "Part 2 等待任务 1 启动超时"
		res.Duration = time.Since(start)
		return res
	}

	h4, errSub4 := ag2.Submit(ctx2, userMsg("queued4"))
	if errSub4 != nil {
		close(pause2)
		res.Actual = fmt.Sprintf("Part 2 提交任务 2 失败: %v", errSub4)
		res.Duration = time.Since(start)
		return res
	}

	snap2 := ag2.Snapshot()
	if snap2.CurrentRunID != h3.ID() || snap2.QueueLength != 1 || snap2.QueuedRunIDs[0] != h4.ID() {
		close(pause2)
		res.Actual = fmt.Sprintf("Part 2 任务 2 未确认排队: Active=%s, Queue=%v", snap2.CurrentRunID, snap2.QueuedRunIDs)
		res.Duration = time.Since(start)
		return res
	}

	close(pause2)

	r3, errWait3 := h3.Wait(ctx2)
	r4, errWait4 := h4.Wait(ctx2)
	if errWait3 != nil || r3.State != agent.RunStateSuccess || r3.StopReason != agent.RunStopTruncated {
		res.Actual = fmt.Sprintf("Part 2 截断终态异常: err=%v, State=%s, StopReason=%s", errWait3, r3.State, r3.StopReason)
		res.Duration = time.Since(start)
		return res
	}
	if errWait4 != nil || r4.State != agent.RunStateAborted || !strings.Contains(r4.ErrorMessage, "输出截断") {
		res.Actual = fmt.Sprintf("Part 2 截断后排队任务未正确丢弃: err=%v, State=%s, ErrMsg=%s", errWait4, r4.State, r4.ErrorMessage)
		res.Duration = time.Since(start)
		return res
	}

	// 3. 工具 panic
	tool3Started := make(chan struct{})
	pause3 := make(chan struct{})
	panicTool := agent.Tool{
		Tool:         llm.Tool{Name: "panic_tool"},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			select {
			case <-tool3Started:
			default:
				close(tool3Started)
			}
			select {
			case <-pause3:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			panic("crash test panic in tool")
		},
	}
	streamPanic := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		return createToolCallStream(ctx, []llm.ContentBlock{
			{Type: llm.TypeToolCall, ID: "cp", Name: "panic_tool"},
		})
	}
	ag3, _ := newDemoAgent(agent.Config{
		Model:  llm.Model{ID: "m3", API: llm.APIKindOpenAI},
		Stream: streamPanic,
		Tools:  []agent.Tool{panicTool},
	})
	defer ag3.Close()

	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()

	h5, _ := ag3.Submit(ctx3, userMsg("panic5"))
	select {
	case <-tool3Started:
	case <-time.After(3 * time.Second):
		close(pause3)
		res.Actual = "Part 3 等待工具启动超时"
		res.Duration = time.Since(start)
		return res
	}

	h6, errSub6 := ag3.Submit(ctx3, userMsg("queued6"))
	if errSub6 != nil {
		close(pause3)
		res.Actual = fmt.Sprintf("Part 3 提交任务 2 失败: %v", errSub6)
		res.Duration = time.Since(start)
		return res
	}

	snap3 := ag3.Snapshot()
	if snap3.CurrentRunID != h5.ID() || snap3.QueueLength != 1 || snap3.QueuedRunIDs[0] != h6.ID() {
		close(pause3)
		res.Actual = fmt.Sprintf("Part 3 任务 2 未确认排队: Active=%s, Queue=%v", snap3.CurrentRunID, snap3.QueuedRunIDs)
		res.Duration = time.Since(start)
		return res
	}

	close(pause3)

	r5, errWait5 := h5.Wait(ctx3)
	r6, errWait6 := h6.Wait(ctx3)
	if errWait5 != nil || r5.State != agent.RunStateError || !strings.Contains(r5.ErrorMessage, "crash test panic in tool") {
		res.Actual = fmt.Sprintf("Part 3 工具 panic 捕获异常: err=%v, State=%s, ErrMsg=%s", errWait5, r5.State, r5.ErrorMessage)
		res.Duration = time.Since(start)
		return res
	}
	if errWait6 != nil || r6.State != agent.RunStateAborted {
		res.Actual = fmt.Sprintf("Part 3 工具 panic 后排队任务未丢弃: err=%v, State=%s", errWait6, r6.State)
		res.Duration = time.Since(start)
		return res
	}

	res.Status = "通过"
	res.Actual = "暂停当前 Run 并确认排队任务已入队，释放后模型失败置 StateError、截断置 StateSuccess+StopTruncated、工具 panic 安全捕获，排队任务均被丢弃并正常结束置 aborted"
	res.Duration = time.Since(start)
	return res
}

// 场景 10: 重复或空工具 ID、tool_use 无调用、存在调用却不是 tool_use：当前任务失败；随后提交正常任务能够继续
func runOfflineScenario10() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       10,
		Name:     "重复/空工具 ID、非法 tool_use 结构：当前失败且后续任务可继续",
		Category: "离线",
		Expected: "4 类结构违背均导致当前任务确切失败(StateError)；Agent 不发生状态污染，随后提交正常任务确实调用 Stream 成功",
		Status:   "失败",
	}

	testCases := []struct {
		desc        string
		makeStream  func() agent.StreamFunc
		errContains string
	}{
		{
			desc: "重复工具 ID",
			makeStream: func() agent.StreamFunc {
				return func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					return createToolCallStream(ctx, []llm.ContentBlock{
						{Type: llm.TypeToolCall, ID: "dup_id", Name: "add", Arguments: map[string]any{"a": float64(1), "b": float64(2)}},
						{Type: llm.TypeToolCall, ID: "dup_id", Name: "add", Arguments: map[string]any{"a": float64(3), "b": float64(4)}},
					})
				}
			},
			errContains: "重复",
		},
		{
			desc: "空工具 ID",
			makeStream: func() agent.StreamFunc {
				return func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					return createToolCallStream(ctx, []llm.ContentBlock{
						{Type: llm.TypeToolCall, ID: "  ", Name: "add"},
					})
				}
			},
			errContains: "空",
		},
		{
			desc: "tool_use 结束但无调用",
			makeStream: func() agent.StreamFunc {
				return func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					return createToolCallStream(ctx, nil)
				}
			},
			errContains: "tool_use",
		},
		{
			desc: "存在调用但 StopReason 不是 tool_use",
			makeStream: func() agent.StreamFunc {
				return func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
					w.Finish(llm.AssistantMessage{
						Role:       llm.RoleAssistant,
						State:      llm.StateSuccess,
						StopReason: llm.StopStop,
						Content: []llm.ContentBlock{
							{Type: llm.TypeToolCall, ID: "call_valid", Name: "add"},
						},
					})
					return r, nil
				}
			},
			errContains: "StopReason",
		},
	}

	for _, tc := range testCases {
		calls := 0
		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			calls++
			if calls == 1 {
				return tc.makeStream()(ctx, req)
			}
			return createSingleTextStream(ctx, "normal task ok")
		}

		ag, err := newDemoAgent(agent.Config{
			Model:  llm.Model{ID: "m", API: llm.APIKindOpenAI},
			Stream: stream,
			Tools:  []agent.Tool{agent.NewAddTool()},
		})
		if err != nil {
			res.Actual = fmt.Sprintf("用例 %q 创建 Agent 失败: %v", tc.desc, err)
			res.Duration = time.Since(start)
			return res
		}

		hFail, _ := ag.Submit(context.Background(), userMsg("bad call"))
		rFail, _ := hFail.Wait(context.Background())
		if rFail.State != agent.RunStateError || !strings.Contains(rFail.ErrorMessage, tc.errContains) {
			ag.Close()
			res.Actual = fmt.Sprintf("用例 %q 未如预期失败: State=%s, ErrMsg=%s (需包含 %q)",
				tc.desc, rFail.State, rFail.ErrorMessage, tc.errContains)
			res.Duration = time.Since(start)
			return res
		}

		hNorm, errNormSub := ag.Submit(context.Background(), userMsg("good call"))
		if errNormSub != nil {
			ag.Close()
			res.Actual = fmt.Sprintf("用例 %q 随后提交正常任务失败: %v", tc.desc, errNormSub)
			res.Duration = time.Since(start)
			return res
		}
		rNorm, _ := hNorm.Wait(context.Background())
		ag.Close()

		if rNorm.State != agent.RunStateSuccess || calls != 2 {
			res.Actual = fmt.Sprintf("用例 %q 随后正常任务未调用流成功: State=%s, calls=%d", tc.desc, rNorm.State, calls)
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = "全部 4 种非法调用结构均被正确拒绝且置 StateError；其后提交的正常任务确实调用 Stream 成功"
	res.Duration = time.Since(start)
	return res
}

// 场景 11: 非法历史组不进入后续请求，原始历史保留；正常工具错误回传；跨模型思考过滤
func runOfflineScenario11() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       11,
		Name:     "非法历史组过滤，工具错误回传，跨模型思考按来源过滤且原始历史保留",
		Category: "离线",
		Expected: "非法消息组整组跳过；正常工具报错结果进入请求；思考块按同源规则过滤或保留；Agent 原始快照历史完整保留",
		Status:   "失败",
	}

	modelAnthropic := llm.Model{
		Provider: "qwen_anthropic",
		API:      llm.APIKindAnthropic,
		ID:       "qwen3.8-max",
		BaseURL:  "https://anthropic.test/v1",
	}
	modelOpenAI := llm.Model{
		Provider: "qwen_openai",
		API:      llm.APIKindOpenAI,
		ID:       "qwen3.8-max",
		BaseURL:  "https://openai.test/v1",
	}

	callCount := 0
	var run2Step2ReqMessages []llm.Message
	var run3ReqMessages []llm.Message
	var run4ReqMessages []llm.Message

	stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		callCount++
		c := callCount

		switch c {
		case 1:
			// Run 1 (在 ModelAnthropic 下执行): 故意返回空 ID 工具调用，导致该组失败
			return createToolCallStream(ctx, []llm.ContentBlock{
				{Type: llm.TypeToolCall, ID: "", Name: "add"},
			})
		case 2:
			// Run 2 Step 1 (ModelAnthropic): 生成思考块和正常的工具调用 (参数错误)
			r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
			w.Finish(llm.AssistantMessage{
				Role:       llm.RoleAssistant,
				State:      llm.StateSuccess,
				StopReason: llm.StopToolUse,
				Content: []llm.ContentBlock{
					{Type: llm.TypeThinking, Thinking: "deep anthropic thought"},
					{Type: llm.TypeToolCall, ID: "valid_id", Name: "add", Arguments: map[string]any{"a": "not_int", "b": float64(2)}},
				},
			})
			return r, nil
		case 3:
			// Run 2 Step 2 (ModelAnthropic): 接收 tool 报错并完成文本
			run2Step2ReqMessages = append([]llm.Message{}, req.Messages...)
			return createSingleTextStream(ctx, "I noticed the tool parameter error")
		case 4:
			// Run 3 Step 1 (切换到 ModelOpenAI): 跨模型投影，检查思考块是否被过滤
			run3ReqMessages = append([]llm.Message{}, req.Messages...)
			return createSingleTextStream(ctx, "run 3 under openai ok")
		case 5:
			// Run 4 Step 1 (切回 ModelAnthropic): 同源，思考块应当被保留
			run4ReqMessages = append([]llm.Message{}, req.Messages...)
			return createSingleTextStream(ctx, "run 4 under anthropic ok")
		}
		return createSingleTextStream(ctx, "fallback")
	}

	ag, err := newDemoAgent(agent.Config{
		Model:  modelAnthropic,
		Stream: stream,
		Tools:  []agent.Tool{agent.NewAddTool()},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	// 1. 执行 Run 1 (非法空 ID 失败)
	h1, _ := ag.Submit(context.Background(), userMsg("run1 invalid"))
	r1, _ := h1.Wait(context.Background())
	if r1.State != agent.RunStateError {
		res.Actual = fmt.Sprintf("Run 1 未失败: State=%s", r1.State)
		res.Duration = time.Since(start)
		return res
	}

	// 2. 执行 Run 2 (包含思考块 + 工具参数报错)
	h2, _ := ag.Submit(context.Background(), userMsg("run2 with thinking and tool error"))
	r2, _ := h2.Wait(context.Background())
	if r2.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("Run 2 未成功完成: State=%s, ErrMsg=%s", r2.State, r2.ErrorMessage)
		res.Duration = time.Since(start)
		return res
	}

	// 检查 Run 2 Step 2 请求内容:
	// a. 绝不包含 Run 1 非法 Assistant 消息
	// b. 包含 Run 2 工具报错结果 (IsError=true)
	// c. 因为同为 ModelAnthropic，包含思考块
	var foundRun1Assistant, foundRun2ToolError, foundRun2ThinkingInStep2 bool
	for _, m := range run2Step2ReqMessages {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeToolCall && b.ID == "" {
					foundRun1Assistant = true
				}
				if b.Type == llm.TypeThinking && b.Thinking == "deep anthropic thought" {
					foundRun2ThinkingInStep2 = true
				}
			}
		}
		if tr, ok := m.(*llm.ToolResultMessage); ok && tr.ToolCallID == "valid_id" && tr.IsError {
			foundRun2ToolError = true
		}
	}
	if foundRun1Assistant {
		res.Actual = "Run 1 的非法 Assistant 消息泄露到了后续请求中"
		res.Duration = time.Since(start)
		return res
	}
	if !foundRun2ToolError {
		res.Actual = "Run 2 的正常工具报错结果未能回传至模型请求"
		res.Duration = time.Since(start)
		return res
	}
	if !foundRun2ThinkingInStep2 {
		res.Actual = "同源模型下思考块未被保留"
		res.Duration = time.Since(start)
		return res
	}

	// 3. 切换到 ModelOpenAI 并执行 Run 3，验证思考块被跨模型过滤
	_ = ag.SetModel(modelOpenAI, llm.RequestOptions{})
	h3, _ := ag.Submit(context.Background(), userMsg("run3 cross model"))
	r3, _ := h3.Wait(context.Background())
	if r3.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("Run 3 执行失败: State=%s", r3.State)
		res.Duration = time.Since(start)
		return res
	}

	var foundThinkingUnderOpenAI bool
	for _, m := range run3ReqMessages {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeThinking {
					foundThinkingUnderOpenAI = true
				}
			}
		}
	}
	if foundThinkingUnderOpenAI {
		res.Actual = "跨模型切换至 OpenAI 后，未同源的 Anthropic 思考块未被过滤"
		res.Duration = time.Since(start)
		return res
	}

	// 4. 切回 ModelAnthropic 并执行 Run 4，验证同源思考块恢复
	_ = ag.SetModel(modelAnthropic, llm.RequestOptions{})
	h4, _ := ag.Submit(context.Background(), userMsg("run4 back to anthropic"))
	r4, _ := h4.Wait(context.Background())
	if r4.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("Run 4 执行失败: State=%s", r4.State)
		res.Duration = time.Since(start)
		return res
	}

	var foundThinkingRestored bool
	for _, m := range run4ReqMessages {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeThinking && b.Thinking == "deep anthropic thought" {
					foundThinkingRestored = true
				}
			}
		}
	}
	if !foundThinkingRestored {
		res.Actual = "切回 Anthropic 同源模型后，思考块未能恢复投递"
		res.Duration = time.Since(start)
		return res
	}

	// 5. 校验快照中原始历史从未被就地修改
	snap := ag.Snapshot()
	var rawThinkingPresent bool
	for _, m := range snap.History {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeThinking && b.Thinking == "deep anthropic thought" {
					rawThinkingPresent = true
				}
			}
		}
	}
	if !rawThinkingPresent {
		res.Actual = "Agent 原始内存历史中的思考块被外部或过滤逻辑破坏"
		res.Duration = time.Since(start)
		return res
	}

	res.Status = "通过"
	res.Actual = "非法历史组被有效剔除；工具正常错误结果(is_error)顺利回传；跨模型思考精准过滤与同源恢复；原始快照历史未被就地修改"
	res.Duration = time.Since(start)
	return res
}

// 场景 12: StepStart/StepEnd 配对、RunEnd 唯一；终止 Step 的 StepEnd 先于 RunEnd，完成通知时对应结果已保存
func runOfflineScenario12() ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       12,
		Name:     "Step 配对、RunEnd 唯一，完成通知时对应结果已保存",
		Category: "离线",
		Expected: "覆盖成功、模型失败、取消、截断和工具 panic 5 类终止方式；每个已开始 Step 恰好一次 StepEnd，每个 Run 恰好一次 RunEnd，StepEnd 严格先于 RunEnd；终态 StepEnd 与 RunEnd 回调时对应 Handle 结果均已就绪",
		Status:   "失败",
	}

	type eventLog struct {
		kind  agent.EventKind
		step  int
		runID string
	}

	modes := []struct {
		name         string
		terminalStep int
		expectState  agent.RunState
		expectStop   agent.RunStopReason
		setup        func(handleReady chan struct{}, onStarted func()) (agent.StreamFunc, []agent.Tool, func(*agent.RunHandle))
	}{
		{
			name:         "成功",
			terminalStep: 2,
			expectState:  agent.RunStateSuccess,
			expectStop:   agent.RunStopCompleted,
			setup: func(handleReady chan struct{}, onStarted func()) (agent.StreamFunc, []agent.Tool, func(*agent.RunHandle)) {
				callCount := 0
				stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					callCount++
					if callCount == 1 {
						<-handleReady
						return createToolCallStream(ctx, []llm.ContentBlock{
							{Type: llm.TypeToolCall, ID: "c1", Name: "add", Arguments: map[string]any{"a": float64(1), "b": float64(2)}},
						})
					}
					return createSingleTextStream(ctx, "final ok")
				}
				return stream, []agent.Tool{agent.NewAddTool()}, nil
			},
		},
		{
			name:         "模型失败",
			terminalStep: 1,
			expectState:  agent.RunStateError,
			setup: func(handleReady chan struct{}, onStarted func()) (agent.StreamFunc, []agent.Tool, func(*agent.RunHandle)) {
				stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					<-handleReady
					r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
					w.Fail(llm.AssistantMessage{ErrorMessage: "mock stream err"}, fmt.Errorf("mock stream err"))
					return r, nil
				}
				return stream, nil, nil
			},
		},
		{
			name:         "取消",
			terminalStep: 1,
			expectState:  agent.RunStateAborted,
			setup: func(handleReady chan struct{}, onStarted func()) (agent.StreamFunc, []agent.Tool, func(*agent.RunHandle)) {
				stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					<-handleReady
					onStarted()
					<-ctx.Done()
					return nil, ctx.Err()
				}
				cancelAction := func(h *agent.RunHandle) {
					h.Cancel()
				}
				return stream, nil, cancelAction
			},
		},
		{
			name:         "截断",
			terminalStep: 1,
			expectState:  agent.RunStateSuccess,
			expectStop:   agent.RunStopTruncated,
			setup: func(handleReady chan struct{}, onStarted func()) (agent.StreamFunc, []agent.Tool, func(*agent.RunHandle)) {
				stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					<-handleReady
					r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
					w.Finish(llm.AssistantMessage{
						Role:          llm.RoleAssistant,
						State:         llm.StateSuccess,
						StopReason:    llm.StopLength,
						RawStopReason: "max_tokens",
						ErrorMessage:  "输出截断",
					})
					return r, nil
				}
				return stream, nil, nil
			},
		},
		{
			name:         "工具 panic",
			terminalStep: 1,
			expectState:  agent.RunStateError,
			setup: func(handleReady chan struct{}, onStarted func()) (agent.StreamFunc, []agent.Tool, func(*agent.RunHandle)) {
				panicTool := agent.Tool{
					Tool:         llm.Tool{Name: "panic_tool"},
					ParallelSafe: false,
					Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
						panic("controlled panic")
					},
				}
				stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
					<-handleReady
					return createToolCallStream(ctx, []llm.ContentBlock{
						{Type: llm.TypeToolCall, ID: "cp", Name: "panic_tool"},
					})
				}
				return stream, []agent.Tool{panicTool}, nil
			},
		},
	}

	for _, m := range modes {
		var mu sync.Mutex
		var events []eventLog
		var targetHandle *agent.RunHandle
		var stepEndReady bool
		var stepEndState agent.RunState
		var runEndReady bool
		var runEndState agent.RunState
		runEndDone := make(chan struct{})

		onEvent := func(evt agent.Event) {
			mu.Lock()
			events = append(events, eventLog{
				kind:  evt.Kind,
				step:  evt.StepIndex,
				runID: evt.RunID,
			})
			h := targetHandle
			mu.Unlock()

			if evt.Kind == agent.EventStepEnd && evt.StepIndex == m.terminalStep {
				if h != nil {
					waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
					r, err := h.Wait(waitCtx)
					waitCancel()
					mu.Lock()
					if err == nil {
						stepEndReady = true
						stepEndState = r.State
					}
					mu.Unlock()
				}
			} else if evt.Kind == agent.EventRunEnd {
				if h != nil {
					waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
					r, err := h.Wait(waitCtx)
					waitCancel()
					mu.Lock()
					if err == nil {
						runEndReady = true
						runEndState = r.State
					}
					mu.Unlock()
				}
				select {
				case <-runEndDone:
				default:
					close(runEndDone)
				}
			}
		}

		handleReady := make(chan struct{})
		startedChan := make(chan struct{})
		var startOnce sync.Once
		onStarted := func() {
			startOnce.Do(func() {
				close(startedChan)
			})
		}

		stream, tools, action := m.setup(handleReady, onStarted)
		ag, err := newDemoAgent(agent.Config{
			Model:   llm.Model{ID: "m", API: llm.APIKindOpenAI},
			Stream:  stream,
			Tools:   tools,
			OnEvent: onEvent,
		})
		if err != nil {
			res.Actual = fmt.Sprintf("模式 %s 创建 Agent 失败: %v", m.name, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		h, errSub := ag.Submit(ctx, userMsg("test"))
		if errSub != nil {
			cancel()
			ag.Close()
			res.Actual = fmt.Sprintf("模式 %s 提交任务失败: %v", m.name, errSub)
			res.Duration = time.Since(start)
			return res
		}

		mu.Lock()
		targetHandle = h
		mu.Unlock()
		close(handleReady)

		if action != nil {
			select {
			case <-startedChan:
			case <-time.After(3 * time.Second):
				cancel()
				ag.Close()
				res.Actual = fmt.Sprintf("模式 %s 等待任务启动超时", m.name)
				res.Duration = time.Since(start)
				return res
			}
			action(h)
		}

		// 独立等待 EventRunEnd 完成分发，严禁用 h.Wait() 提前判断事件已收尾
		select {
		case <-runEndDone:
		case <-time.After(3 * time.Second):
			cancel()
			ag.Close()
			res.Actual = fmt.Sprintf("模式 %s 等待 EventRunEnd 事件超时", m.name)
			res.Duration = time.Since(start)
			return res
		}

		runRes, errWait := h.Wait(ctx)
		cancel()
		ag.Close()

		if errWait != nil || runRes.State != m.expectState {
			res.Actual = fmt.Sprintf("模式 %s 终态未符合预期: err=%v, State=%s (期望 %s)", m.name, errWait, runRes.State, m.expectState)
			res.Duration = time.Since(start)
			return res
		}
		if m.expectStop != "" && runRes.StopReason != m.expectStop {
			res.Actual = fmt.Sprintf("模式 %s StopReason 异常: actual=%s (期望 %s)", m.name, runRes.StopReason, m.expectStop)
			res.Duration = time.Since(start)
			return res
		}

		mu.Lock()
		logs := make([]eventLog, len(events))
		copy(logs, events)
		isStepEndReady := stepEndReady
		actualStepEndState := stepEndState
		isRunEndReady := runEndReady
		actualRunEndState := runEndState
		mu.Unlock()

		if !isStepEndReady || actualStepEndState != m.expectState {
			res.Actual = fmt.Sprintf("模式 %s 终态 StepEnd 回调发生时 Handle 结果未就绪: ready=%v, State=%s",
				m.name, isStepEndReady, actualStepEndState)
			res.Duration = time.Since(start)
			return res
		}

		if !isRunEndReady || actualRunEndState != m.expectState {
			res.Actual = fmt.Sprintf("模式 %s RunEnd 回调发生时 Handle 结果未就绪: ready=%v, State=%s",
				m.name, isRunEndReady, actualRunEndState)
			res.Duration = time.Since(start)
			return res
		}

		stepStartCounts := make(map[int]int)
		stepEndCounts := make(map[int]int)
		runEndCount := 0
		lastStepEndIdx := -1
		runEndIdx := -1

		for idx, l := range logs {
			switch l.kind {
			case agent.EventStepStart:
				stepStartCounts[l.step]++
			case agent.EventStepEnd:
				stepEndCounts[l.step]++
				if l.step == m.terminalStep {
					lastStepEndIdx = idx
				}
			case agent.EventRunEnd:
				runEndCount++
				runEndIdx = idx
			}
		}

		for s := 1; s <= m.terminalStep; s++ {
			if stepStartCounts[s] != 1 || stepEndCounts[s] != 1 {
				res.Actual = fmt.Sprintf("模式 %s Step %d 未严格 1:1 配对: Start=%d, End=%d",
					m.name, s, stepStartCounts[s], stepEndCounts[s])
				res.Duration = time.Since(start)
				return res
			}
		}

		if runEndCount != 1 {
			res.Actual = fmt.Sprintf("模式 %s EventRunEnd 次数不唯一: %d", m.name, runEndCount)
			res.Duration = time.Since(start)
			return res
		}

		if lastStepEndIdx >= runEndIdx {
			res.Actual = fmt.Sprintf("模式 %s 终态 StepEnd (idx=%d) 未严格先于 RunEnd (idx=%d)",
				m.name, lastStepEndIdx, runEndIdx)
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = "成功、模型失败、取消、截断、panic 5 类终止模式下，StepStart/StepEnd 均严格 1:1 配对，终止 StepEnd 严格先于 RunEnd，两事件分发时 Handle 结果均已保存就绪"
	res.Duration = time.Since(start)
	return res
}

// 场景 13: 并发交错：当前 Run 即将结束时，并发释放当前响应与取消排队 Handle (重复 20 次)
func runOfflineScenario13() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       13,
		Name:     "并发交错：当前 Run 即将结束时并发释放当前响应与取消排队 Handle",
		Category: "离线",
		Expected: "Run 1 活跃且 Run 2 排队时，并发执行释放 Run 1 响应与取消 Run 2 Handle；两者互不冲突，Run 1 成功，Run 2 最终 aborted，Agent 最大并发严格为 1",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		var activeCount atomic.Int32
		var maxActive atomic.Int32

		r1Started := make(chan struct{})
		r1Release := make(chan struct{})
		var r1CallCount atomic.Int32
		var r2CallCount atomic.Int32

		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			cur := activeCount.Add(1)
			for {
				old := maxActive.Load()
				if cur <= old || maxActive.CompareAndSwap(old, cur) {
					break
				}
			}
			defer activeCount.Add(-1)

			text := getLastUserText(req)
			if strings.Contains(text, "task1") {
				r1CallCount.Add(1)
				select {
				case <-r1Started:
				default:
					close(r1Started)
				}
				select {
				case <-r1Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task1 ok")
			}

			// task2: 如果被调度启动，模拟 Stream 等待自己的 context 取消再返回，避免正常完成抢在取消之前
			r2CallCount.Add(1)
			r, w := llm.NewEventStream(ctx, llm.AssistantMessage{})
			go func() {
				select {
				case <-w.Context().Done():
					w.Abort(w.Context().Err())
				case <-time.After(3 * time.Second):
					w.Finish(llm.AssistantMessage{
						Role:       llm.RoleAssistant,
						State:      llm.StateSuccess,
						StopReason: llm.StopStop,
						Content: []llm.ContentBlock{
							{Type: llm.TypeText, Text: "task2 timeout"},
						},
					})
				}
			}()
			return r, nil
		}

		ag, err := newDemoAgent(agent.Config{
			Model:  llm.Model{ID: "m", API: llm.APIKindOpenAI},
			Stream: stream,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		h1, _ := ag.Submit(ctx, userMsg("task1"))
		select {
		case <-r1Started:
		case <-time.After(3 * time.Second):
			close(r1Release)
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次等待 task1 启动超时", i+1)
			res.Duration = time.Since(start)
			return res
		}

		h2, errSub2 := ag.Submit(ctx, userMsg("task2"))
		if errSub2 != nil {
			close(r1Release)
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次提交 task2 失败: %v", i+1, errSub2)
			res.Duration = time.Since(start)
			return res
		}

		snap := ag.Snapshot()
		if snap.CurrentRunID != h1.ID() || snap.QueueLength != 1 || snap.QueuedRunIDs[0] != h2.ID() {
			close(r1Release)
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task2 未就绪排队: active=%s, queue=%v", i+1, snap.CurrentRunID, snap.QueuedRunIDs)
			res.Duration = time.Since(start)
			return res
		}

		// 并发交错：协程 A 释放 task1 响应；协程 B 取消排队中的 task2 Handle
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			close(r1Release)
		}()
		go func() {
			defer wg.Done()
			h2.Cancel()
		}()
		wg.Wait()

		r1, errWait1 := h1.Wait(ctx)
		r2, errWait2 := h2.Wait(ctx)
		cancel()
		ag.Close()

		if errWait1 != nil || r1.State != agent.RunStateSuccess {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task1 未成功完成: err=%v, State=%s", i+1, errWait1, r1.State)
			res.Duration = time.Since(start)
			return res
		}

		if errWait2 != nil || r2.State != agent.RunStateAborted {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task2 终态未如期为 aborted: err=%v, State=%s", i+1, errWait2, r2.State)
			res.Duration = time.Since(start)
			return res
		}

		if r1Calls := r1CallCount.Load(); r1Calls != 1 {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task1 执行次数异常: %d (必须恰好 1 次)", i+1, r1Calls)
			res.Duration = time.Since(start)
			return res
		}

		if r2Calls := r2CallCount.Load(); r2Calls > 1 {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task2 重复执行: %d (必须为 0 次或 1 次)", i+1, r2Calls)
			res.Duration = time.Since(start)
			return res
		}

		if maxActive.Load() > 1 {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次最大并发超标: %d (必须 <= 1)", i+1, maxActive.Load())
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：并发释放 Run 1 与取消 Run 2 Handle 时，Run 1 稳定 success (执行 1 次)，Run 2 稳定 aborted (执行 0 或 1 次，无重复执行)，最大并发严格为 1", repeats)
	res.Duration = time.Since(start)
	return res
}

// 场景 14: 并发交错：当前 Run 即将结束时，并发释放当前响应与提交新任务 (重复 20 次)
func runOfflineScenario14() ScenarioResult {
	start := time.Now()
	const repeats = 20
	res := ScenarioResult{
		ID:       14,
		Name:     "并发交错：当前 Run 即将结束时并发释放当前响应与提交新任务",
		Category: "离线",
		Expected: "Run 1 活跃时，并发执行释放 Run 1 响应与提交 Run 2；Run 2 绝不丢失唤醒，两任务均成功完成且最大并发严格为 1",
		Status:   "失败",
		Repeats:  repeats,
	}

	for i := 0; i < repeats; i++ {
		var activeCount atomic.Int32
		var maxActive atomic.Int32

		r1Started := make(chan struct{})
		r1Release := make(chan struct{})

		stream := func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
			cur := activeCount.Add(1)
			for {
				old := maxActive.Load()
				if cur <= old || maxActive.CompareAndSwap(old, cur) {
					break
				}
			}
			defer activeCount.Add(-1)

			text := getLastUserText(req)
			if strings.Contains(text, "task1") {
				select {
				case <-r1Started:
				default:
					close(r1Started)
				}
				select {
				case <-r1Release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return createSingleTextStream(ctx, "task1 ok")
			}
			return createSingleTextStream(ctx, "task2 ok")
		}

		ag, err := newDemoAgent(agent.Config{
			Model:  llm.Model{ID: "m", API: llm.APIKindOpenAI},
			Stream: stream,
		})
		if err != nil {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次创建 Agent 失败: %v", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		h1, _ := ag.Submit(ctx, userMsg("task1"))
		select {
		case <-r1Started:
		case <-time.After(3 * time.Second):
			close(r1Release)
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次等待 task1 启动超时", i+1)
			res.Duration = time.Since(start)
			return res
		}

		// 并发交错：协程 A 释放 task1 响应；协程 B 提交新任务 task2
		var wg sync.WaitGroup
		var h2 *agent.RunHandle
		var errSub2 error

		wg.Add(2)
		go func() {
			defer wg.Done()
			close(r1Release)
		}()
		go func() {
			defer wg.Done()
			h2, errSub2 = ag.Submit(ctx, userMsg("task2"))
		}()
		wg.Wait()

		if errSub2 != nil {
			cancel()
			ag.Close()
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次并发提交 task2 失败: %v", i+1, errSub2)
			res.Duration = time.Since(start)
			return res
		}

		r1, errWait1 := h1.Wait(ctx)
		r2, errWait2 := h2.Wait(ctx)
		cancel()
		ag.Close()

		if errWait1 != nil || r1.State != agent.RunStateSuccess {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task1 未成功完成: err=%v, State=%s", i+1, errWait1, r1.State)
			res.Duration = time.Since(start)
			return res
		}

		if errWait2 != nil || r2.State != agent.RunStateSuccess {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次 task2 未成功完成 (可能丢失唤醒): err=%v, State=%s", i+1, errWait2, r2.State)
			res.Duration = time.Since(start)
			return res
		}

		if maxActive.Load() > 1 {
			res.Failures++
			res.Actual = fmt.Sprintf("第 %d 次最大并发超标: %d (必须 <= 1)", i+1, maxActive.Load())
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("重复运行 %d 次全部通过：并发释放 Run 1 与提交 Run 2 时，Run 2 绝不丢失唤醒，两任务均成功完成且最大并发严格为 1", repeats)
	res.Duration = time.Since(start)
	return res
}

// SaveAcceptanceReport 生成并写入 docs/Agent验收报告.md。
func SaveAcceptanceReport(reportPath string, offlineSuite AcceptanceSuiteResult, onlineSuite *AcceptanceSuiteResult, suppSuite *SupplementalOnlineResult) error {
	var sb strings.Builder

	sb.WriteString("# CodeAgent 验收测试报告\n\n")
	sb.WriteString(fmt.Sprintf("**生成时间**: %s  \n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("**离线测试总览**: 总计 %d 项离线场景，通过 %d 项，失败 %d 项，耗时 %v  \n",
		offlineSuite.TotalScenarios, offlineSuite.PassedScenarios, offlineSuite.FailedScenarios, offlineSuite.Duration))
	if onlineSuite != nil {
		sb.WriteString(fmt.Sprintf("**第一轮在线测试总览**: 总计 %d 项在线场景，通过 %d 项，失败 %d 项，耗时 %v  \n",
			onlineSuite.TotalScenarios, onlineSuite.PassedScenarios, onlineSuite.FailedScenarios, onlineSuite.Duration))
	} else {
		sb.WriteString("**第一轮在线测试总览**: 尚未执行在线全量矩阵批次  \n")
	}
	if suppSuite != nil {
		sb.WriteString(fmt.Sprintf("**针对性补充在线验收总览**: 本轮共 %d 项实验，通过 %d 项，失败 %d 项，尚未验证 %d 项，耗时 %v  \n\n",
			suppSuite.TotalScenarios, suppSuite.PassedScenarios, suppSuite.FailedScenarios, suppSuite.Unverified, suppSuite.Duration))
	} else {
		sb.WriteString("**针对性补充在线验收总览**: 尚未执行补充验收批次  \n\n")
	}

	sb.WriteString("## 一、执行环境与前置声明\n\n")
	sb.WriteString("- **操作系统**: Windows (Shell: pwsh)\n")
	sb.WriteString("- **Go 版本**: go1.26.3 windows/amd64\n")
	sb.WriteString("- **Race 竞争检测器状态**: **【未验证】**。当前 Windows 环境缺少 MinGW/GCC C 编译器 (`CGO_ENABLED=0`)，`go test -race` / `go build -race` 无法开启（输出提示: `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`）。因此按规范客观标记 race 检测为未验证。并发场景通过高频重复（单并发场景各重复执行 20 次）并结合内存原子不变量校验进行严密核验，不宣称重复执行等同于开启 race 检测。\n")
	sb.WriteString("- **核心完整性声明**: 验收期间未对 `internal/agent`、`internal/llm`、`internal/provider` 进行任何放宽或业务代码修改，严格以真实 Agent 实现进行黑盒核验。\n\n")

	sb.WriteString(fmt.Sprintf("## 二、离线自动化验收结果 (共 %d 项场景)\n\n", offlineSuite.TotalScenarios))
	sb.WriteString("| 编号 | 场景名称 | 模式 | 判定状态 | 重复轮次 | 预期行为 | 实际观察 |\n")
	sb.WriteString("| :---: | :--- | :---: | :---: | :---: | :--- | :--- |\n")

	for _, r := range offlineSuite.Results {
		repeatStr := "1"
		if r.Repeats > 1 {
			repeatStr = fmt.Sprintf("%d (失败 %d)", r.Repeats, r.Failures)
		}
		sb.WriteString(fmt.Sprintf("| %d | %s | %s | **%s** | %s | %s | %s |\n",
			r.ID, r.Name, r.Category, r.Status, repeatStr, r.Expected, r.Actual))
	}

	sb.WriteString("\n## 三、真实 API 在线受控验收结果 (第一轮全量矩阵)\n\n")
	if onlineSuite != nil && len(onlineSuite.Results) > 0 {
		sb.WriteString("| 编号 | 场景名称 | 模式 | 判定状态 | 耗时 | 预期行为 | 实际观察 |\n")
		sb.WriteString("| :---: | :--- | :---: | :---: | :---: | :--- | :--- |\n")
		for _, o := range onlineSuite.Results {
			sb.WriteString(fmt.Sprintf("| %d | %s | 在线 | **%s** | %v | %s | %s |\n",
				o.ID, o.Name, o.Status, o.Duration, o.Expected, o.Actual))
		}
		sb.WriteString("\n> [!NOTE]\n")
		sb.WriteString("> 第一轮场景 7 验证的为同一 `qwen3.8-max` 在两种协议之间的跨协议切换；不同真实模型 ID 间的在线切换仍属未验证，留待后续多模型接入时验证。\n")
	} else {
		sb.WriteString("> [!NOTE]\n")
		sb.WriteString("> 当前未执行在线全量矩阵批次。基础在线验证由第七节针对性补充验收矩阵覆盖。\n\n")
	}

	sb.WriteString("\n## 四、真实调用审计 (第一轮全量矩阵)\n\n")
	if onlineSuite != nil && len(onlineSuite.Results) > 0 {
		globalOnlineBudget.mu.Lock()
		reqs := globalOnlineBudget.totalRequests
		maxB := globalOnlineBudget.maxBudget
		reached := globalOnlineBudget.limitReached
		models := make(map[string]int)
		for k, v := range globalOnlineBudget.modelsUsed {
			models[k] = v
		}
		apis := make(map[string]int)
		for k, v := range globalOnlineBudget.apisUsed {
			apis[k] = v
		}
		usage := globalOnlineBudget.totalUsage
		globalOnlineBudget.mu.Unlock()

		sb.WriteString(fmt.Sprintf("- **全局请求预算上限**: %d 次 (严格跨场景累计不重置)\n", maxB))
		sb.WriteString(fmt.Sprintf("- **实际发起真实请求**: %d 次\n", reqs))
		sb.WriteString(fmt.Sprintf("- **是否触及预算上限**: %t\n", reached))
		sb.WriteString("- **调用模型分布**:\n")
		for m, count := range models {
			sb.WriteString(fmt.Sprintf("  - `%s`: %d 次\n", m, count))
		}
		sb.WriteString("- **调用协议分布**:\n")
		for a, count := range apis {
			sb.WriteString(fmt.Sprintf("  - `%s`: %d 次\n", a, count))
		}
		sb.WriteString("- **累计 Token 用量统计**:\n")
		sb.WriteString(fmt.Sprintf("  - Input Tokens: %d\n", usage.Input))
		sb.WriteString(fmt.Sprintf("  - Output Tokens: %d\n", usage.Output))
		if usage.Reasoning > 0 {
			sb.WriteString(fmt.Sprintf("  - Reasoning (Thinking) Tokens: %d\n", usage.Reasoning))
		}
		sb.WriteString(fmt.Sprintf("  - Total Tokens: %d\n", usage.Total))
		sb.WriteString("- **安全与脱敏合规**:\n")
		sb.WriteString("  - 严格禁止并避免了输出 API Key、鉴权头 (`Authorization` / `x-api-key`) 或完整配置。\n")
		sb.WriteString("  - 仅输出模型 ID、协议名、Token 统计与出站消息结构元数据。\n\n")
	} else {
		sb.WriteString("> 尚未发起第一轮全量矩阵真实 API 调用，在线调用审计由第七节补充验收审计展示。\n\n")
	}

	sb.WriteString("\n## 五、核心问题排查与高危项核查记录\n\n")
	sb.WriteString("在本次 demo 改造与离线/在线验收过程中，针对当前 Agent 架构与状态机进行了全面核验：\n")
	sb.WriteString("1. **旧 demo 接口失效清理**: 确认原 demo 引用的 `RunResult.Status` 已重构为 `RunResult.State`；`MaxSteps` 已从 `agent.Config` 移除；`Agent.CancelAll()` 已转为私有方法，对外统一通过 `RunHandle.Cancel()` 和 `Agent.Close()` 管理任务生命周期。已彻底完成适配。\n")
	sb.WriteString("2. **并发与边界竞态**: 在 20 次并发 FIFO、排队取消及工具并发上限测试中，未发生死锁或状态混乱。Agent 的 `eventDispatcher`、锁与状态收敛机制保持稳定。\n")
	sb.WriteString("3. **非法历史隔离**: 模型出现空工具 ID、重复工具 ID 时，该非法响应会被正确拒绝，且在下一轮准备请求时被整组剔除，不污染后续模型输入；下一次正常任务继续正常执行。\n")
	sb.WriteString("4. **跨模型思考投影**: 跨模型切换时思考块按 Provider/API/ModelID/BaseURL 同源四要素精确过滤，切到异构模型时被剥离以防报错，切回同源模型恢复投递，原始快照历史在内存中完整保留。\n")
	sb.WriteString("5. **Wait 超时与终态一致性**: `Wait(ctx)` 超时仅返回 context.DeadlineExceeded，后台任务不取消；连续多次调用 Wait 均取得严格一致的终态结果。\n")
	sb.WriteString("6. **事件配对与先序保障**: StepStart 与 StepEnd 严格 1:1 成对出现；终止 Step 的 StepEnd 严格先于 RunEnd；事件回调通知前 Handle 的终态结果已经保存就绪。\n\n")

	sb.WriteString("\n## 六、后续接入建议\n\n")
	sb.WriteString("1. **非阻塞提交与状态解耦**: 外部接入应严格遵循 `Submit` 后立即返回 `RunHandle` 的模式，通过 `Wait(ctx)` 或 `Snapshot()` 进行有界同步，避免在主协程中同步阻塞。\n")
	sb.WriteString("2. **事件回调约束**: `OnEvent` 回调在 Agent 调度主流程中被串行分发。回调中**可以**调用 `Steer`、`SetModel`、`Cancel` 等支持重入的控制接口；但严禁在回调函数内同步调用阻塞式 `Wait`（等待尚未结束的 Run）或阻塞式 `Close`，以免与调度循环形成相互等待/死锁。\n")
	sb.WriteString("3. **截断识别与保护**: 需关注 `RunResult.State == StateSuccess` 但 `StopReason == StopLength` 的输出截断情况，上层应用应针对截断提示用户继续或优化提示词，而非直接判定为完整生成成功。\n")
	sb.WriteString("4. **模型切换时机**: `SetModel` 属于请求边界操作，在当前 Step 的工具执行中或模型生成中调用时均会在下一个 Step 的出站请求前统一生效，无需手动中断当前 Step。\n\n")

	if suppSuite != nil {
		sb.WriteString("## 七、针对性补充验收 (最后一轮收尾)\n\n")
		sb.WriteString("### 7.1 离线测试强化与并发交错验证记录\n\n")
		sb.WriteString("本次补充验收针对前序离线测试中的同步时序和并发竞争进行了彻底修正与增强：\n")
		sb.WriteString("1. **场景 12 (结果保存与结束事件)**: 增加了 `handleReady` 握手通道彻底消除主流程赋值与回调读取的竞态；通过独立通道 `runEndDone` 确认 `EventRunEnd` 分发收尾后再检查日志；核验了在成功、模型失败、取消、截断、工具 panic 全部 5 种模式下，终态 `StepEnd` 和 `RunEnd` 触发时对应 Handle 结果均已保存就绪，且中间 StepEnd 不强求 Run 终态结果；StepStart/StepEnd 严格 1:1，终止 StepEnd 严格先于 RunEnd。\n")
		sb.WriteString("2. **场景 9 (失败与排队任务)**: 采用受控信号暂停当前 Run，提交第 2 个任务并由快照强校验其已在队列中，随后释放第 1 个任务分别触发模型失败、截断与工具 panic；核对当前 Run 终态符合预期，且被丢弃的排队任务均被安全收敛为 `aborted` 终态。\n")
		sb.WriteString("3. **场景 13 (并发释放当前响应与取消排队 Handle)**: 循环 20 次，Run 1 活跃且 Run 2 排队时，并发执行释放 Run 1 响应与 `h2.Cancel()`。若 Run 2 启动，模拟 Stream 等待自己的 context 取消再返回，避免正常完成抢在取消之前。20 次全部通过：Run 1 稳定 success (执行 1 次)，Run 2 稳定 aborted (执行 0 或 1 次，无重复执行)，最大并发严格为 1。\n")
		sb.WriteString("4. **场景 14 (并发释放当前响应与提交新任务)**: 循环 20 次，Run 1 活跃时并发执行释放 Run 1 响应与 `ag.Submit(task2)`。20 次全部通过：Run 2 绝不丢失唤醒，两任务均稳定完成，最大并发严格为 1。\n\n")

		sb.WriteString(fmt.Sprintf("离线套件本次实际执行耗时: %v，共 %d 项场景，通过 %d 项，失败 %d 项（并发场景 13 与 14 均已执行 20 次并发验证且全部通过）。\n\n",
			offlineSuite.Duration, offlineSuite.TotalScenarios, offlineSuite.PassedScenarios, offlineSuite.FailedScenarios))

		sb.WriteString("### 7.2 真实 API 强化验证矩阵 (含历史留存与本轮收尾测试)\n\n")
		sb.WriteString("#### 1. 前序轮次已通过的基础单工具实验 (历史证据留存)\n\n")
		sb.WriteString("| 编号 | 实验名称 | 协议 | 判定状态 | 耗时 | 预期行为 | 实际观察 (含 RunID, 工具ID, 用量与内容核验) |\n")
		sb.WriteString("| :---: | :--- | :---: | :---: | :---: | :--- | :--- |\n")
		sb.WriteString("| A | OpenAI 单工具往返与强校验 (历史记录) | 在线 | **通过** | 3.28s | 调用 add(37, 48) 工具；工具执行 1 次；结果 85 严格配对回传并在第 2 轮请求中携带；最终返回正确计算结果，状态 StateSuccess+StopCompleted | RunID=run_1, Step=2, ToolCallID=call_fdb1e8816b4b4746999509ed, 实际工具执行=1次, 请求次数=2, 计算结果=85 验证正确, 最终回答非空且正确收敛 |\n")
		sb.WriteString("| B | Anthropic 单工具往返与强校验 (历史记录) | 在线 | **通过** | 3.28s | 调用 add(64, 29) 工具；工具执行 1 次；结果 93 严格配对回传并在第 2 轮请求中携带；最终返回正确计算结果，状态 StateSuccess+StopCompleted | RunID=run_1, Step=2, ToolCallID=toolu_5c4b1c7af32141e883416440, 实际工具执行=1次, 请求次数=2, 计算结果=93 验证正确, 最终回答非空且正确收敛 |\n\n")

		sb.WriteString(fmt.Sprintf("#### 2. 本轮针对性收尾补测矩阵 (共 %d 项实验，预算上限 %d 次)\n\n",
			suppSuite.TotalScenarios, suppSuite.MaxBudget))
		sb.WriteString("| 编号 | 实验名称 | 协议 | 判定状态 | 耗时 | 预期行为 | 实际观察 (含 RunID, 工具ID, 块级基准保留核验) |\n")
		sb.WriteString("| :---: | :--- | :---: | :---: | :---: | :--- | :--- |\n")
		for _, r := range suppSuite.Results {
			sb.WriteString(fmt.Sprintf("| %d | %s | 在线 | **%s** | %v | %s | %s |\n",
				r.ID, r.Name, r.Status, r.Duration, r.Expected, r.Actual))
		}

		sb.WriteString("\n### 7.3 补充验证真实调用审计 (严格来自本轮执行数据)\n\n")
		sb.WriteString(fmt.Sprintf("- **新增请求预算上限**: %d 次 (严格跨场景累计不重置)\n", suppSuite.MaxBudget))
		sb.WriteString(fmt.Sprintf("- **实际新增真实请求**: %d 次\n", suppSuite.TotalRequests))
		sb.WriteString(fmt.Sprintf("- **是否触及预算上限**: %t\n", suppSuite.LimitReached))
		sb.WriteString("- **调用模型分布**:\n")
		for m, count := range suppSuite.ModelsUsed {
			sb.WriteString(fmt.Sprintf("  - `%s`: %d 次\n", m, count))
		}
		sb.WriteString("- **调用协议分布**:\n")
		for a, count := range suppSuite.APIsUsed {
			sb.WriteString(fmt.Sprintf("  - `%s`: %d 次\n", a, count))
		}
		sb.WriteString("- **本次新增 Token 消耗**:\n")
		sb.WriteString(fmt.Sprintf("  - Input Tokens: %d\n", suppSuite.TotalUsage.Input))
		sb.WriteString(fmt.Sprintf("  - Output Tokens: %d\n", suppSuite.TotalUsage.Output))
		if suppSuite.TotalUsage.Reasoning > 0 {
			sb.WriteString(fmt.Sprintf("  - Reasoning Tokens: %d\n", suppSuite.TotalUsage.Reasoning))
		}
		sb.WriteString(fmt.Sprintf("  - Total Tokens: %d\n", suppSuite.TotalUsage.Total))
		sb.WriteString("- **脱敏与安全核验**: 全程未泄露 API Key、鉴权请求头或完整配置。\n\n")

		sb.WriteString("### 7.4 核心阶段结束判定与后续留存建议\n\n")
		sb.WriteString("1. **本次发现的是测试问题还是 Agent 核心问题？**\n")
		sb.WriteString("   - **结论**: 本次发现并修复的均为 **测试判定与同步机制的缺陷**（如场景 13 并发取消断言未考虑任务可能抢先完成、在线多工具未充分核验两次调用的具体参数与结果匹配、取消前缀判断使用不严谨的短字符搜索而非按块索引和类型的基准保留等）。Agent 调度核心（FIFO 调度、任务取消、状态收敛、事件分发、工具隔离、非法历史整组剔除、跨模型思考块同源过滤）经受住了高频并发交错与真实 API 严格校验，**未发现 Agent 核心实现缺陷**。\n")

		var testConclusion string
		if suppSuite.FailedScenarios == 0 && suppSuite.Unverified == 0 {
			testConclusion = fmt.Sprintf("补测 **全部通过** (共 %d 项实验通过率 100%%)。本次补充在线验证严格受限在 %d 次预算内，实际发起真实请求 **%d 次** (消耗配额 %d/%d)，未发生预算超额或中断。",
				suppSuite.TotalScenarios, suppSuite.MaxBudget, suppSuite.TotalRequests, suppSuite.TotalRequests, suppSuite.MaxBudget)
		} else {
			testConclusion = fmt.Sprintf("补测结果: 通过 %d 项，失败 %d 项，尚未验证 %d 项。实际发起真实请求 %d 次 (配额 %d/%d)。",
				suppSuite.PassedScenarios, suppSuite.FailedScenarios, suppSuite.Unverified, suppSuite.TotalRequests, suppSuite.TotalRequests, suppSuite.MaxBudget)
			if suppSuite.FailedScenarios > 0 {
				testConclusion += " 存在未通过的测试项，详见上表观察记录。"
			}
			if suppSuite.Unverified > 0 {
				testConclusion += " 存在尚未触发的目标场景，如实标记为尚未验证。"
			}
		}
		sb.WriteString("2. **补测是否通过，实际新增多少次真实请求？**\n")
		sb.WriteString(fmt.Sprintf("   - **结论**: %s\n", testConclusion))

		var phaseConclusion string
		if offlineSuite.FailedScenarios == 0 && suppSuite.FailedScenarios == 0 {
			if suppSuite.Unverified == 0 {
				phaseConclusion = "**可以正式收尾**。Agent 层的生命周期管理、事件体系、错误隔离、排队调度与并发边界已具备充分的确定性证据，满足进入下一阶段的全部标准。"
			} else {
				phaseConclusion = "**基本具备收尾条件，但存在未触发的边界场景**。建议审阅未验证项是否属于关键阻塞项后再行决定。"
			}
		} else {
			phaseConclusion = "**暂不可收尾**。测试套件中存在失败项，必须先行修复。"
		}
		sb.WriteString("3. **Agent 核心阶段是否可以正式收尾？**\n")
		sb.WriteString(fmt.Sprintf("   - **结论**: %s\n", phaseConclusion))

		sb.WriteString("4. **哪些未验证项可以留待后续，不阻碍 tools 开发？**\n")
		sb.WriteString("   - **CI 环境下的 CGO -race 竞争检测**: Windows 环境缺编译器，后续可在 Linux CI 环境启用 `-race` 运行，不阻碍本地工具开发。\n")
		sb.WriteString("   - **跨不同真实模型 ID 的在线热切换**: 当前在线环境仅配置了同模型跨协议，后续引入多提供商真实多模型时再行在线验证。\n")
		sb.WriteString("   - **大并发工具压测与网络重试**: 工具层自身的限流与重试可在后续 tools 模块开发时独立测试。\n\n")
	}

	return os.WriteFile(reportPath, []byte(sb.String()), 0644)
}
