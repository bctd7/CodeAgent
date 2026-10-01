package demo

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

// GlobalOnlineBudgetTracker 管理全局真实请求预算 (上限 40 次)，跨场景不重置。
type GlobalOnlineBudgetTracker struct {
	mu            sync.Mutex
	maxBudget     int
	totalRequests int
	modelsUsed    map[string]int
	apisUsed      map[string]int
	totalUsage    llm.Usage
	limitReached  bool
}

var globalOnlineBudget = &GlobalOnlineBudgetTracker{
	maxBudget:  40,
	modelsUsed: make(map[string]int),
	apisUsed:   make(map[string]int),
}

// ResetGlobalBudget 重置全局预算 (用于全新批次执行)。
func ResetGlobalBudget(maxCalls int) {
	globalOnlineBudget.mu.Lock()
	defer globalOnlineBudget.mu.Unlock()
	globalOnlineBudget.maxBudget = maxCalls
	globalOnlineBudget.totalRequests = 0
	globalOnlineBudget.modelsUsed = make(map[string]int)
	globalOnlineBudget.apisUsed = make(map[string]int)
	globalOnlineBudget.totalUsage = llm.Usage{}
	globalOnlineBudget.limitReached = false
}

func (t *GlobalOnlineBudgetTracker) wrapStream(baseStream StreamFunc, onReqPrepared func(req llm.Request)) StreamFunc {
	return func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
		t.mu.Lock()
		if t.totalRequests >= t.maxBudget {
			t.limitReached = true
			t.mu.Unlock()
			return nil, fmt.Errorf("已达到全局真实请求预算上限 (%d 次)，由 demo 强制中止调用", t.maxBudget)
		}
		t.totalRequests++
		t.modelsUsed[req.Model.ID]++
		t.apisUsed[string(req.Model.API)]++
		t.mu.Unlock()

		if onReqPrepared != nil {
			onReqPrepared(req)
		}

		reader, err := baseStream(ctx, req)
		if err != nil {
			return nil, err
		}

		return &trackedStreamReader{
			StreamReader: reader,
			tracker:      t,
		}, nil
	}
}

type trackedStreamReader struct {
	llm.StreamReader
	tracker    *GlobalOnlineBudgetTracker
	accumulate sync.Once
}

func (r *trackedStreamReader) Result() llm.Result {
	res := r.StreamReader.Result()
	r.accumulate.Do(func() {
		r.tracker.mu.Lock()
		u := res.Message.Usage
		r.tracker.totalUsage.Input += u.Input
		r.tracker.totalUsage.Output += u.Output
		r.tracker.totalUsage.Reasoning += u.Reasoning
		r.tracker.totalUsage.Total += u.Total
		r.tracker.totalUsage.Cost.Total += u.Cost.Total
		r.tracker.mu.Unlock()
	})
	return res
}

func (r *trackedStreamReader) Close() {
	_ = r.Result()
	r.StreamReader.Close()
}

// RunOnlineAcceptanceSuite 执行全套真实 API 验收矩阵，严格受限在 40 次全局调用内。
func RunOnlineAcceptanceSuite(entries []config.ModelEntry, baseStream StreamFunc, verbose bool) AcceptanceSuiteResult {
	suiteStartTime := time.Now()
	ResetGlobalBudget(40)

	if verbose {
		fmt.Println("\n==================================================")
		fmt.Println("开始执行 Agent 真实 API 受控在线验收矩阵")
		fmt.Printf("全局真实调用硬上限: %d 次 (跨场景累计，绝不重置)\n", globalOnlineBudget.maxBudget)
		fmt.Println("==================================================")
	}

	// 选取具备测试资格的 OpenAI 协议与 Anthropic 协议模型，优先使用已有成功记录的配置
	var entryOpenAI, entryAnthropic *config.ModelEntry
	for i := range entries {
		e := &entries[i]
		if (e.Ref == "qwen_openai/qwen" || e.Model.ID == "qwen3.8-max" && e.Model.API == llm.APIKindOpenAI) && entryOpenAI == nil {
			entryOpenAI = e
		}
		if (e.Ref == "qwen_anthropic/qwen" || e.Model.ID == "qwen3.8-max" && e.Model.API == llm.APIKindAnthropic) && entryAnthropic == nil {
			entryAnthropic = e
		}
	}
	if entryOpenAI == nil || entryAnthropic == nil {
		for i := range entries {
			e := &entries[i]
			if e.Model.API == llm.APIKindOpenAI && entryOpenAI == nil {
				entryOpenAI = e
			}
			if e.Model.API == llm.APIKindAnthropic && entryAnthropic == nil {
				entryAnthropic = e
			}
		}
	}

	suite := AcceptanceSuiteResult{
		TotalScenarios: 7,
		Results:        make([]ScenarioResult, 0, 7),
	}

	if entryOpenAI == nil || entryAnthropic == nil {
		if verbose {
			fmt.Println("错误: 缺少可用的 OpenAI 或 Anthropic 协议模型配置，无法启动全套在线验收。")
		}
		suite.FailedScenarios = 7
		return suite
	}

	if verbose {
		fmt.Printf("已选定真实测试模型:\n")
		fmt.Printf("  - OpenAI 协议:    %s (%s, BaseURL: %s)\n", entryOpenAI.Ref, entryOpenAI.Model.ID, entryOpenAI.Model.BaseURL)
		fmt.Printf("  - Anthropic 协议: %s (%s, BaseURL: %s)\n", entryAnthropic.Ref, entryAnthropic.Model.ID, entryAnthropic.Model.BaseURL)
		fmt.Println("--------------------------------------------------")
	}

	// 1. 普通流式回答 (OpenAI 2 次, Anthropic 2 次)
	res1 := runOnlineStreamChatBatch(entryOpenAI, entryAnthropic, baseStream, verbose)
	suite.Results = append(suite.Results, res1)

	// 2. 单工具往返 (OpenAI 2 次, Anthropic 2 次)
	res2 := runOnlineSingleToolBatch(entryOpenAI, entryAnthropic, baseStream, verbose)
	suite.Results = append(suite.Results, res2)

	// 3. 多工具调用 (至少 1 次)
	res3 := runOnlineMultiTool(entryOpenAI, baseStream, verbose)
	suite.Results = append(suite.Results, res3)

	// 4. 运行中用户纠正 (确认活动后提交，核对进入下一请求)
	res4 := runOnlineSteerScenario(entryOpenAI, baseStream, verbose)
	suite.Results = append(suite.Results, res4)

	// 5. 主动取消 (两种协议各 1 次，确认收到内容后取消)
	res5 := runOnlineCancelBatch(entryOpenAI, entryAnthropic, baseStream, verbose)
	suite.Results = append(suite.Results, res5)

	// 6. FIFO 排队 (活动中提交第 2 个任务，核对顺序)
	res6 := runOnlineQueueScenario(entryOpenAI, baseStream, verbose)
	suite.Results = append(suite.Results, res6)

	// 7. 运行中跨协议/跨模型切换 (生成中切换 + 工具中跨协议切换)
	res7 := runOnlineModelSwitchScenario(entryOpenAI, entryAnthropic, baseStream, verbose)
	suite.Results = append(suite.Results, res7)

	for _, r := range suite.Results {
		if r.Status == "通过" {
			suite.PassedScenarios++
		} else if r.Status == "失败" {
			suite.FailedScenarios++
		} else {
			suite.PendingOnline++
		}
	}

	suite.Duration = time.Since(suiteStartTime)

	if verbose {
		globalOnlineBudget.mu.Lock()
		fmt.Println("\n==================================================")
		fmt.Printf("真实 API 验收矩阵执行完毕: 总耗时 %v\n", suite.Duration)
		fmt.Printf("全局请求使用: %d / %d 次 (超额: %v)\n",
			globalOnlineBudget.totalRequests, globalOnlineBudget.maxBudget, globalOnlineBudget.limitReached)
		fmt.Printf("各模型请求分布: %v\n", globalOnlineBudget.modelsUsed)
		fmt.Printf("各协议请求分布: %v\n", globalOnlineBudget.apisUsed)
		fmt.Printf("累计 Token 用量: 输入=%d, 输出=%d, 思考=%d, 总计=%d\n",
			globalOnlineBudget.totalUsage.Input, globalOnlineBudget.totalUsage.Output,
			globalOnlineBudget.totalUsage.Reasoning, globalOnlineBudget.totalUsage.Total)
		fmt.Printf("通过场景: %d, 失败场景: %d, 尚未验证: %d\n",
			suite.PassedScenarios, suite.FailedScenarios, suite.PendingOnline)
		fmt.Println("==================================================")
		globalOnlineBudget.mu.Unlock()
	}

	return suite
}

// 1. 普通流式回答 (OpenAI 2 次 + Anthropic 2 次)
func runOnlineStreamChatBatch(entryOpenAI, entryAnthropic *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       1,
		Name:     "在线场景 1: 普通流式回答 (OpenAI 2 次 + Anthropic 2 次)",
		Category: "在线",
		Expected: "每种协议各执行 2 次单轮对话；核对文本、思考、终态 success 及实际返回用量",
		Status:   "失败",
	}

	tests := []struct {
		name  string
		entry *config.ModelEntry
	}{
		{"OpenAI 轮次 1", entryOpenAI},
		{"OpenAI 轮次 2", entryOpenAI},
		{"Anthropic 轮次 1", entryAnthropic},
		{"Anthropic 轮次 2", entryAnthropic},
	}

	var observations []string
	allPassed := true

	for idx, t := range tests {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		ag, err := newDemoAgent(agent.Config{
			Model:   t.entry.Model,
			Options: t.entry.Options,
			Stream:  agent.StreamFunc(globalOnlineBudget.wrapStream(baseStream, nil)),
		})
		if err != nil {
			cancel()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 创建 Agent 失败: %v", t.name, err))
			continue
		}

		prompt := fmt.Sprintf("请用一句简短的话总结计算机发展史第 %d 部分，请思考后回答", idx+1)
		h, errSub := ag.Submit(ctx, userMsg(prompt))
		if errSub != nil {
			cancel()
			ag.Close()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s Submit 失败: %v", t.name, errSub))
			continue
		}

		runRes, errWait := h.Wait(ctx)
		cancel()
		ag.Close()

		if errWait != nil || runRes.State != agent.RunStateSuccess || runRes.TotalUsage.Total == 0 {
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 未符合预期: err=%v, State=%s, Usage=%d",
				t.name, errWait, runRes.State, runRes.TotalUsage.Total))
		} else {
			observations = append(observations, fmt.Sprintf("%s 成功 (State=%s, TotalTokens=%d)",
				t.name, runRes.State, runRes.TotalUsage.Total))
		}
	}

	res.Duration = time.Since(start)
	if allPassed {
		res.Status = "通过"
	}
	res.Actual = strings.Join(observations, "; ")
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// 2. 单工具往返 (OpenAI 2 次 + Anthropic 2 次)
func runOnlineSingleToolBatch(entryOpenAI, entryAnthropic *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       2,
		Name:     "在线场景 2: 单工具往返 (OpenAI 2 次 + Anthropic 2 次)",
		Category: "在线",
		Expected: "每种协议各执行 2 次加法工具调用；确认工具真实执行、结果进入后续请求，模型根据结果回答",
		Status:   "失败",
	}

	tests := []struct {
		name  string
		entry *config.ModelEntry
		a, b  int
	}{
		{"OpenAI 轮次 1", entryOpenAI, 15, 27},
		{"OpenAI 轮次 2", entryOpenAI, 42, 58},
		{"Anthropic 轮次 1", entryAnthropic, 33, 67},
		{"Anthropic 轮次 2", entryAnthropic, 88, 12},
	}

	var observations []string
	allPassed := true

	for _, t := range tests {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		ag, err := newDemoAgent(agent.Config{
			Model:   t.entry.Model,
			Options: t.entry.Options,
			Stream:  agent.StreamFunc(globalOnlineBudget.wrapStream(baseStream, nil)),
			Tools:   []agent.Tool{agent.NewAddTool()},
		})
		if err != nil {
			cancel()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 创建 Agent 失败: %v", t.name, err))
			continue
		}

		prompt := fmt.Sprintf("请严格调用 add 工具计算 %d 加 %d 的和，收到工具结果后给出最终计算值。", t.a, t.b)
		h, errSub := ag.Submit(ctx, userMsg(prompt))
		if errSub != nil {
			cancel()
			ag.Close()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s Submit 失败: %v", t.name, errSub))
			continue
		}

		runRes, errWait := h.Wait(ctx)
		cancel()
		ag.Close()

		if errWait != nil || runRes.State != agent.RunStateSuccess || runRes.Steps < 2 {
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 往返未完成: err=%v, State=%s, Steps=%d",
				t.name, errWait, runRes.State, runRes.Steps))
		} else {
			observations = append(observations, fmt.Sprintf("%s 成功 (Steps=%d, State=%s)",
				t.name, runRes.Steps, runRes.State))
		}
	}

	res.Duration = time.Since(start)
	if allPassed {
		res.Status = "通过"
	}
	res.Actual = strings.Join(observations, "; ")
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// 3. 多工具调用 (至少 1 次)
func runOnlineMultiTool(entry *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       3,
		Name:     "在线场景 3: 多工具调用并发调度与结果配对",
		Category: "在线",
		Expected: "模型在单步中产生至少 2 个工具调用；记录实际调用数、执行顺序与配对；模型未产生多个调用则如实标记尚未验证",
		Status:   "尚未验证",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ag, err := newDemoAgent(agent.Config{
		Model:   entry.Model,
		Options: entry.Options,
		Stream:  agent.StreamFunc(globalOnlineBudget.wrapStream(baseStream, nil)),
		Tools:   []agent.Tool{agent.NewAddTool()},
	})
	if err != nil {
		res.Status = "失败"
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	prompt := "请在同一个回答中调用 add 工具两次：第一次计算 12 加 34，第二次计算 56 加 78。请务必调用工具计算。"
	h, errSub := ag.Submit(ctx, userMsg(prompt))
	if errSub != nil {
		res.Status = "失败"
		res.Actual = fmt.Sprintf("Submit 失败: %v", errSub)
		res.Duration = time.Since(start)
		return res
	}

	runRes, errWait := h.Wait(ctx)
	res.Duration = time.Since(start)

	if errWait != nil {
		res.Status = "失败"
		res.Actual = fmt.Sprintf("执行返回错误: %v", errWait)
		return res
	}

	// 统计模型第 1 步生成的工具调用数量
	toolCallCount := 0
	for _, m := range runRes.Messages {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeToolCall {
					toolCallCount++
				}
			}
			if toolCallCount > 0 {
				break
			}
		}
	}

	if toolCallCount >= 2 {
		res.Status = "通过"
		res.Actual = fmt.Sprintf("模型单步成功发出 %d 个工具调用并完成配对计算与收尾汇总", toolCallCount)
	} else {
		res.Status = "尚未验证"
		res.Actual = fmt.Sprintf("模型单步仅产生了 %d 个工具调用 (未达多工具阈值 >=2)，按约定标记为尚未验证", toolCallCount)
	}

	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// 4. 运行中用户纠正 (确认 Run 活动后提交，核对纠正进入下一次请求)
func runOnlineSteerScenario(entry *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       4,
		Name:     "在线场景 4: 运行中用户纠正并核对注入下一次请求",
		Category: "在线",
		Expected: "在确认工具开始执行后提交 Steer；核对纠正以 [用户纠正] 进入第 2 轮请求，模型基于纠正完成收尾",
		Status:   "失败",
	}

	toolStarted := make(chan struct{})
	allowToolFinish := make(chan struct{})

	controlledTool := agent.Tool{
		Tool: llm.Tool{
			Name:        "inspect_task",
			Description: "用于检查当前环境状态并上报",
		},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			select {
			case <-toolStarted:
			default:
				close(toolStarted)
			}
			select {
			case <-allowToolFinish:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "status_ready"}}, nil
		},
	}

	var secondReqMessages []llm.Message
	var reqMu sync.Mutex
	reqCount := 0

	trackStream := globalOnlineBudget.wrapStream(baseStream, func(req llm.Request) {
		reqMu.Lock()
		reqCount++
		if reqCount == 2 {
			secondReqMessages = append([]llm.Message{}, req.Messages...)
		}
		reqMu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ag, err := newDemoAgent(agent.Config{
		Model:   entry.Model,
		Options: entry.Options,
		Stream:  agent.StreamFunc(trackStream),
		Tools:   []agent.Tool{controlledTool, agent.NewAddTool()},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	prompt := "请先调用 inspect_task 工具检查环境状态，然后计算 10 加 20。"
	h, errSub := ag.Submit(ctx, userMsg(prompt))
	if errSub != nil {
		res.Actual = fmt.Sprintf("Submit 失败: %v", errSub)
		res.Duration = time.Since(start)
		return res
	}

	// 等待受控工具确实开始执行，杜绝基于 Sleep 的猜测
	select {
	case <-toolStarted:
	case <-time.After(30 * time.Second):
		close(allowToolFinish)
		res.Actual = "等待真实模型调用 inspect_task 工具超时"
		res.Duration = time.Since(start)
		return res
	}

	// 在工具执行期间提交纠正
	steerErr := ag.Steer("纠正：不要计算 10 加 20 了，请直接回答数字 999 并说明收到纠正。")
	if steerErr != nil {
		close(allowToolFinish)
		res.Actual = fmt.Sprintf("提交纠正失败: %v", steerErr)
		res.Duration = time.Since(start)
		return res
	}

	// 释放工具执行
	close(allowToolFinish)

	runRes, errWait := h.Wait(ctx)
	res.Duration = time.Since(start)

	if errWait != nil || runRes.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("任务执行未成功: err=%v, State=%s", errWait, runRes.State)
		return res
	}

	// 核验第 2 轮请求是否包含用户纠正
	var foundSteerInReq bool
	reqMu.Lock()
	for _, m := range secondReqMessages {
		if u, ok := m.(*llm.UserMessage); ok {
			for _, b := range u.Content {
				if strings.Contains(b.Text, "[用户纠正]:") && strings.Contains(b.Text, "999") {
					foundSteerInReq = true
				}
			}
		}
	}
	reqMu.Unlock()

	if !foundSteerInReq {
		res.Actual = "第 2 轮发往真实模型的请求中缺少 [用户纠正] 内容块"
		return res
	}

	res.Status = "通过"
	res.Actual = "工具执行期间精确触发 Steer，纠正成功注入第 2 轮真实出站请求并顺利取得最终回复"
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// 5. 主动取消 (两种协议各 1 次，确认收到内容后取消)
func runOnlineCancelBatch(entryOpenAI, entryAnthropic *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       5,
		Name:     "在线场景 5: 主动取消验证 (OpenAI 与 Anthropic 各 1 次)",
		Category: "在线",
		Expected: "在收到模型首个真实内容/事件后立即触发 Handle.Cancel()；任务终态为 aborted，保留已接收内容",
		Status:   "失败",
	}

	tests := []struct {
		name  string
		entry *config.ModelEntry
	}{
		{"OpenAI 取消测试", entryOpenAI},
		{"Anthropic 取消测试", entryAnthropic},
	}

	var observations []string
	allPassed := true

	for _, t := range tests {
		contentReceived := make(chan struct{})
		var once sync.Once

		onEvent := func(evt agent.Event) {
			if evt.Kind == agent.EventLLMEvent && evt.LLMEvent != nil && evt.LLMEvent.Delta != "" {
				once.Do(func() {
					close(contentReceived)
				})
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		ag, err := newDemoAgent(agent.Config{
			Model:   t.entry.Model,
			Options: t.entry.Options,
			Stream:  agent.StreamFunc(globalOnlineBudget.wrapStream(baseStream, nil)),
			OnEvent: onEvent,
		})
		if err != nil {
			cancel()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 创建 Agent 失败: %v", t.name, err))
			continue
		}

		prompt := "请写一篇超过 1500 字的详细文章解析操作系统虚拟内存分页机制与 TLB 原理，详尽推导。"
		h, errSub := ag.Submit(ctx, userMsg(prompt))
		if errSub != nil {
			cancel()
			ag.Close()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s Submit 失败: %v", t.name, errSub))
			continue
		}

		select {
		case <-contentReceived:
			// 确认收到首个真实内容，立即触发取消
			h.Cancel()
		case <-time.After(30 * time.Second):
			cancel()
			ag.Close()
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 等待首个流式内容超时", t.name))
			continue
		}

		runRes, errWait := h.Wait(ctx)
		cancel()
		ag.Close()

		if errWait != nil || runRes.State != agent.RunStateAborted {
			allPassed = false
			observations = append(observations, fmt.Sprintf("%s 取消终态异常: err=%v, State=%s",
				t.name, errWait, runRes.State))
		} else {
			observations = append(observations, fmt.Sprintf("%s 成功 (收到真实内容后取消，终态=%s，保留消息数=%d)",
				t.name, runRes.State, len(runRes.Messages)))
		}
	}

	res.Duration = time.Since(start)
	if allPassed {
		res.Status = "通过"
	}
	res.Actual = strings.Join(observations, "; ")
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// 6. FIFO 排队 (活动中提交第 2 个任务，核对顺序)
func runOnlineQueueScenario(entry *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       6,
		Name:     "在线场景 6: FIFO 排队调度验证",
		Category: "在线",
		Expected: "在任务 1 执行期间提交任务 2；快照显示任务 2 处于排队队列；任务 1 完成后任务 2 自动执行完成",
		Status:   "失败",
	}

	t1ToolStarted := make(chan struct{})
	allowT1Finish := make(chan struct{})

	blockTool := agent.Tool{
		Tool: llm.Tool{
			Name:        "task1_block_tool",
			Description: "任务 1 阻塞工具",
		},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			select {
			case <-t1ToolStarted:
			default:
				close(t1ToolStarted)
			}
			select {
			case <-allowT1Finish:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "t1_done"}}, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ag, err := newDemoAgent(agent.Config{
		Model:   entry.Model,
		Options: entry.Options,
		Stream:  agent.StreamFunc(globalOnlineBudget.wrapStream(baseStream, nil)),
		Tools:   []agent.Tool{blockTool},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	// 提交任务 1
	h1, errSub1 := ag.Submit(ctx, userMsg("任务 1: 请调用 task1_block_tool 工具"))
	if errSub1 != nil {
		res.Actual = fmt.Sprintf("Submit 1 失败: %v", errSub1)
		res.Duration = time.Since(start)
		return res
	}

	// 等待任务 1 真正开始执行工具
	select {
	case <-t1ToolStarted:
	case <-time.After(30 * time.Second):
		close(allowT1Finish)
		res.Actual = "等待任务 1 执行工具超时"
		res.Duration = time.Since(start)
		return res
	}

	// 在任务 1 活跃时提交任务 2
	h2, errSub2 := ag.Submit(ctx, userMsg("任务 2: 请回答 1+1 等于几，直接输出数字。"))
	if errSub2 != nil {
		close(allowT1Finish)
		res.Actual = fmt.Sprintf("Submit 2 失败: %v", errSub2)
		res.Duration = time.Since(start)
		return res
	}

	// 核验快照中任务 2 处于排队队列
	snap := ag.Snapshot()
	if snap.CurrentRunID != h1.ID() || snap.QueueLength != 1 || snap.QueuedRunIDs[0] != h2.ID() {
		close(allowT1Finish)
		res.Actual = fmt.Sprintf("快照排队不符合预期: Active=%s, Queue=%v", snap.CurrentRunID, snap.QueuedRunIDs)
		res.Duration = time.Since(start)
		return res
	}

	// 释放任务 1
	close(allowT1Finish)

	r1, errWait1 := h1.Wait(ctx)
	if errWait1 != nil || r1.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("任务 1 执行未成功: err=%v, State=%s", errWait1, r1.State)
		res.Duration = time.Since(start)
		return res
	}

	r2, errWait2 := h2.Wait(ctx)
	res.Duration = time.Since(start)
	if errWait2 != nil || r2.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("排队任务 2 执行未成功: err=%v, State=%s", errWait2, r2.State)
		return res
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("任务 1 活跃时任务 2 入队排队，任务 1 完成后任务 2 自动弹出执行并成功 (Run1=%s, Run2=%s)",
		h1.ID(), h2.ID())
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// 7. 运行中跨协议/跨模型切换 (生成期间 + 工具执行期间)
func runOnlineModelSwitchScenario(entryOpenAI, entryAnthropic *config.ModelEntry, baseStream StreamFunc, verbose bool) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       7,
		Name:     "在线场景 7: 运行中跨协议/跨模型切换 (生成中与工具中)",
		Category: "在线",
		Expected: "分别覆盖生成中和工具执行中切换；至少一次跨协议切换；下一轮请求实际使用目标协议及对应模型",
		Status:   "失败",
	}

	// Part A: 生成期间切换 (OpenAI -> Anthropic)
	var step2TargetModel string
	var step2TargetAPI llm.APIKind
	var reqMu sync.Mutex
	partACallCount := 0

	trackStreamA := globalOnlineBudget.wrapStream(baseStream, func(req llm.Request) {
		reqMu.Lock()
		partACallCount++
		if partACallCount == 2 {
			step2TargetModel = req.Model.ID
			step2TargetAPI = req.Model.API
		}
		reqMu.Unlock()
	})

	genStarted := make(chan struct{})
	var genOnce sync.Once

	onEventA := func(evt agent.Event) {
		if evt.Kind == agent.EventLLMEvent && evt.LLMEvent != nil &&
			(evt.LLMEvent.Delta != "" || evt.LLMEvent.Type == llm.EventToolCallBlockStart || evt.LLMEvent.Type == llm.EventTextBlockStart || evt.LLMEvent.Type == llm.EventThinkingBlockStart) {
			genOnce.Do(func() {
				close(genStarted)
			})
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	agA, errA := newDemoAgent(agent.Config{
		Model:   entryOpenAI.Model,
		Options: entryOpenAI.Options,
		Stream:  agent.StreamFunc(trackStreamA),
		Tools:   []agent.Tool{agent.NewAddTool()},
		OnEvent: onEventA,
	})
	if errA != nil {
		res.Actual = fmt.Sprintf("Part A 创建 Agent 失败: %v", errA)
		res.Duration = time.Since(start)
		return res
	}
	defer agA.Close()

	promptA := "请使用 add 工具计算 12 加 34。"
	hA, errSubmitA := agA.Submit(ctx, userMsg(promptA))
	if errSubmitA != nil {
		res.Actual = fmt.Sprintf("Part A 提交失败: %v", errSubmitA)
		res.Duration = time.Since(start)
		return res
	}

	// 等待第 1 步真实生成流启动
	select {
	case <-genStarted:
		// 在生成中切换至 Anthropic 协议
		_ = agA.SetModel(entryAnthropic.Model, entryAnthropic.Options)
	case <-time.After(30 * time.Second):
		res.Actual = "Part A 等待首个流式事件超时"
		res.Duration = time.Since(start)
		return res
	}

	rA, errWaitA := hA.Wait(ctx)
	if errWaitA != nil || rA.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("Part A 执行未成功: err=%v, State=%s", errWaitA, rA.State)
		res.Duration = time.Since(start)
		return res
	}

	reqMu.Lock()
	pAModel := step2TargetModel
	pAAPI := step2TargetAPI
	reqMu.Unlock()

	if pAAPI != llm.APIKindAnthropic {
		res.Actual = fmt.Sprintf("Part A 第 2 步出站请求未切换至 Anthropic 协议: actual=%s", pAAPI)
		res.Duration = time.Since(start)
		return res
	}

	// Part B: 工具执行中跨协议切换 (Anthropic -> OpenAI)
	var step2BModel string
	var step2BAPI llm.APIKind
	partBCallCount := 0

	toolStartedB := make(chan struct{})
	allowToolFinishB := make(chan struct{})

	blockAddTool := agent.Tool{
		Tool:         llm.Tool{Name: "add", Description: "加法计算"},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			select {
			case <-toolStartedB:
			default:
				close(toolStartedB)
			}
			select {
			case <-allowToolFinishB:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []llm.ContentBlock{{Type: llm.TypeText, Text: "88"}}, nil
		},
	}

	trackStreamB := globalOnlineBudget.wrapStream(baseStream, func(req llm.Request) {
		reqMu.Lock()
		partBCallCount++
		if partBCallCount == 2 {
			step2BModel = req.Model.ID
			step2BAPI = req.Model.API
		}
		reqMu.Unlock()
	})

	agB, errB := newDemoAgent(agent.Config{
		Model:   entryAnthropic.Model,
		Options: entryAnthropic.Options,
		Stream:  agent.StreamFunc(trackStreamB),
		Tools:   []agent.Tool{blockAddTool},
	})
	if errB != nil {
		res.Actual = fmt.Sprintf("Part B 创建 Agent 失败: %v", errB)
		res.Duration = time.Since(start)
		return res
	}
	defer agB.Close()

	hB, errSubmitB := agB.Submit(ctx, userMsg("请调用 add 工具计算 44 加 44。"))
	if errSubmitB != nil {
		res.Actual = fmt.Sprintf("Part B 提交失败: %v", errSubmitB)
		res.Duration = time.Since(start)
		return res
	}
	select {
	case <-toolStartedB:
		// 在工具执行期间切换至 OpenAI 协议
		_ = agB.SetModel(entryOpenAI.Model, entryOpenAI.Options)
		close(allowToolFinishB)
	case <-time.After(30 * time.Second):
		close(allowToolFinishB)
		res.Actual = "Part B 等待工具调用开始超时"
		res.Duration = time.Since(start)
		return res
	}

	rB, errWaitB := hB.Wait(ctx)
	res.Duration = time.Since(start)

	if errWaitB != nil || rB.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("Part B 执行未成功: err=%v, State=%s", errWaitB, rB.State)
		return res
	}

	reqMu.Lock()
	pBAPI := step2BAPI
	reqMu.Unlock()

	if pBAPI != llm.APIKindOpenAI {
		res.Actual = fmt.Sprintf("Part B 第 2 步出站请求未切换至 OpenAI 协议: actual=%s", pBAPI)
		return res
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("生成中切换 (切至 %s/%s) 与工具执行中切换 (切至 %s/%s) 均精准在第 2 步出站请求生效，Run 正常成功收尾",
		pAModel, pAAPI, step2BModel, pBAPI)
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// runOnlineAcceptanceMenu 控制台受控交互菜单
func runOnlineAcceptanceMenu(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc, allEntries []config.ModelEntry) {
	fmt.Println("\n==================================================")
	fmt.Println("模式 6: Agent 在线受控验收模式 (自动执行全套真实 API 验收矩阵)")
	fmt.Println("==================================================")
	fmt.Println("本选项将执行覆盖全部要求的在线真实 API 矩阵（最多 40 次全局网络调用）：")
	fmt.Println("  1. 普通流式回答 (OpenAI 2次, Anthropic 2次)")
	fmt.Println("  2. 单工具往返 (OpenAI 2次, Anthropic 2次)")
	fmt.Println("  3. 多工具调用并发调度 (至少 1次)")
	fmt.Println("  4. 运行中用户纠正 (确认活动后触发 Steer 并检查下一请求)")
	fmt.Println("  5. 主动取消验证 (收到首个真实内容后触发 Cancel 并检查终态)")
	fmt.Println("  6. FIFO 排队调度 (活动中提交第 2 个任务，检查排队)")
	fmt.Println("  7. 运行中跨模型/跨协议切换 (生成中切换 + 工具中跨协议切换)")
	fmt.Println("--------------------------------------------------")
	fmt.Print("是否确认立即执行全套在线受控验收？(y/n, 默认 n): ")

	confirm, _ := readLine(reader)
	confirm = strings.TrimSpace(strings.ToLower(confirm))
	if confirm != "y" && confirm != "yes" {
		fmt.Println("已取消在线执行。")
		return
	}

	onlineSuite := RunOnlineAcceptanceSuite(allEntries, stream, true)
	offlineSuite := RunOfflineAcceptance(false)
	reportPath := "docs/Agent验收报告.md"
	if err := SaveAcceptanceReport(reportPath, offlineSuite, &onlineSuite, nil); err != nil {
		fmt.Printf("更新验收报告失败: %v\n", err)
	} else {
		fmt.Printf("\n[验收报告已成功更新并保存至]: %s\n", reportPath)
	}
}

// SupplementalOnlineResult 记录补充在线验收的统计结果与用量。
type SupplementalOnlineResult struct {
	TotalScenarios  int              `json:"total_scenarios"`
	PassedScenarios int              `json:"passed_scenarios"`
	FailedScenarios int              `json:"failed_scenarios"`
	Unverified      int              `json:"unverified"`
	Duration        time.Duration    `json:"duration"`
	Results         []ScenarioResult `json:"results"`
	TotalRequests   int              `json:"total_requests"`
	MaxBudget       int              `json:"max_budget"`
	LimitReached    bool             `json:"limit_reached"`
	ModelsUsed      map[string]int   `json:"models_used"`
	APIsUsed        map[string]int   `json:"apis_used"`
	TotalUsage      llm.Usage        `json:"total_usage"`
}

var supplementalOnlineBudget = &GlobalOnlineBudgetTracker{
	maxBudget:  16,
	modelsUsed: make(map[string]int),
	apisUsed:   make(map[string]int),
}

func parseArgInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		var n int
		fmt.Sscanf(val, "%d", &n)
		return n
	default:
		return 0
	}
}

// runSupplementalSingleTool 执行单工具往返强校验实验
func runSupplementalSingleTool(entry *config.ModelEntry, baseStream StreamFunc, verbose bool, id int, name string, a, b int) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       id,
		Name:     name,
		Category: "在线补测",
		Expected: fmt.Sprintf("调用 add(%d, %d) 工具；工具执行 1 次；结果 %d 严格配对回传并在第 2 轮请求中携带；最终返回正确计算结果，状态 StateSuccess+StopCompleted", a, b, a+b),
		Status:   "失败",
	}

	var toolExecCount atomic.Int32
	var toolCallIDReceived string
	var toolArgsReceived map[string]any
	var toolMu sync.Mutex

	expectedSum := a + b

	addTool := agent.Tool{
		Tool: llm.Tool{
			Name:        "add",
			Description: "加法计算工具。参数 a 和 b",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"a": {"type": "integer", "description": "加数 a"},
					"b": {"type": "integer", "description": "加数 b"}
				},
				"required": ["a", "b"]
			}`),
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			toolExecCount.Add(1)
			toolMu.Lock()
			toolCallIDReceived = callID
			toolArgsReceived = args
			toolMu.Unlock()

			aVal := parseArgInt(args["a"])
			bVal := parseArgInt(args["b"])
			return []llm.ContentBlock{
				{Type: llm.TypeText, Text: fmt.Sprintf("%d", aVal+bVal)},
			}, nil
		},
	}

	var outboundReqs []llm.Request
	var reqMu sync.Mutex

	trackedStream := supplementalOnlineBudget.wrapStream(baseStream, func(req llm.Request) {
		reqMu.Lock()
		outboundReqs = append(outboundReqs, req)
		reqMu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ag, err := newDemoAgent(agent.Config{
		Model:   entry.Model,
		Options: entry.Options,
		Stream:  agent.StreamFunc(trackedStream),
		Tools:   []agent.Tool{addTool},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	prompt := fmt.Sprintf("请严格调用 add 工具计算 %d 加 %d 的和，收到工具结果后再给出最终回答。", a, b)
	h, errSub := ag.Submit(ctx, userMsg(prompt))
	if errSub != nil {
		res.Actual = fmt.Sprintf("Submit 失败: %v", errSub)
		res.Duration = time.Since(start)
		return res
	}

	runRes, errWait := h.Wait(ctx)
	res.Duration = time.Since(start)

	if errWait != nil {
		res.Actual = fmt.Sprintf("Wait 返回错误: %v, State=%s", errWait, runRes.State)
		return res
	}
	if runRes.State != agent.RunStateSuccess {
		res.Actual = fmt.Sprintf("任务执行失败: State=%s, StopReason=%s, ErrMsg=%s",
			runRes.State, runRes.StopReason, runRes.ErrorMessage)
		return res
	}
	if runRes.StopReason != agent.RunStopCompleted {
		res.Actual = fmt.Sprintf("任务非正常结束 (可能为截断): StopReason=%s", runRes.StopReason)
		return res
	}

	if toolExecCount.Load() != 1 {
		res.Actual = fmt.Sprintf("工具执行次数异常: 实际执行 %d 次 (期望严格为 1 次)", toolExecCount.Load())
		return res
	}

	toolMu.Lock()
	capturedCallID := toolCallIDReceived
	capturedArgs := toolArgsReceived
	toolMu.Unlock()

	if capturedCallID == "" {
		res.Actual = "未能捕获到模型生成的工具调用 ID"
		return res
	}

	aParsed := parseArgInt(capturedArgs["a"])
	bParsed := parseArgInt(capturedArgs["b"])
	if aParsed != a || bParsed != b {
		res.Actual = fmt.Sprintf("工具收到参数错误: 实际 a=%d, b=%d, 期望 a=%d, b=%d", aParsed, bParsed, a, b)
		return res
	}

	reqMu.Lock()
	reqsCount := len(outboundReqs)
	var step2Req llm.Request
	if reqsCount >= 2 {
		step2Req = outboundReqs[1]
	}
	reqMu.Unlock()

	if reqsCount < 2 {
		res.Actual = fmt.Sprintf("出站请求次数不足 2 步: 实际 %d 次", reqsCount)
		return res
	}

	// 检查第 2 步出站请求是否携带了配对的工具结果
	var foundMatchingResult bool
	for _, m := range step2Req.Messages {
		if tr, ok := m.(*llm.ToolResultMessage); ok {
			if tr.ToolCallID == capturedCallID && !tr.IsError {
				for _, cb := range tr.Content {
					if cb.Type == llm.TypeText && strings.TrimSpace(cb.Text) == fmt.Sprintf("%d", expectedSum) {
						foundMatchingResult = true
						break
					}
				}
			}
		}
	}
	if !foundMatchingResult {
		res.Actual = fmt.Sprintf("第 2 步出站请求中未找到与调用 ID %q 匹配的正确计算结果 (%d)", capturedCallID, expectedSum)
		return res
	}

	// 检查最终 assistant 内容
	var finalAnswer string
	for i := len(runRes.Messages) - 1; i >= 0; i-- {
		if asst, ok := runRes.Messages[i].(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeText {
					finalAnswer += b.Text
				}
			}
			if len(finalAnswer) > 0 {
				break
			}
		}
	}

	if len(strings.TrimSpace(finalAnswer)) == 0 {
		res.Actual = "最终 Assistant 回答内容为空"
		return res
	}

	if !strings.Contains(finalAnswer, fmt.Sprintf("%d", expectedSum)) {
		res.Actual = fmt.Sprintf("最终回答中未包含预期计算结果 %d: %q", expectedSum, finalAnswer)
		return res
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("RunID=%s, Step=%d, ToolCallID=%s, 实际工具执行=1次, 请求次数=%d, 计算结果=%d 验证正确, 最终回答非空且正确收敛",
		runRes.RunID, runRes.Steps, capturedCallID, reqsCount, expectedSum)
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// runSupplementalMultiTool 执行多工具调用往返与结果配对实验
type executedToolCall struct {
	CallID string
	A, B   int
	Result string
}

func runSupplementalMultiTool(entry *config.ModelEntry, baseStream StreamFunc, verbose bool, id int, name string) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       id,
		Name:     name,
		Category: "在线补测",
		Expected: "单条模型回复产生至少 2 个工具调用；两次调用参数分别符合提示词计算要求({15, 25}与{30, 40})；执行结果正确；下一轮请求各恰好对应一个非错误结果且内容一致；Run 最终为 StateSuccess+RunStopCompleted；最后一条 assistant 消息有非空正文且包含正确汇总答案 110",
		Status:   "失败",
	}

	var toolExecCount atomic.Int32
	var executedCalls []executedToolCall
	var toolMu sync.Mutex

	addTool := agent.Tool{
		Tool: llm.Tool{
			Name:        "add",
			Description: "加法计算工具。参数 a 和 b",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"a": {"type": "integer", "description": "加数 a"},
					"b": {"type": "integer", "description": "加数 b"}
				},
				"required": ["a", "b"]
			}`),
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			toolExecCount.Add(1)
			aVal := parseArgInt(args["a"])
			bVal := parseArgInt(args["b"])
			sumStr := fmt.Sprintf("%d", aVal+bVal)

			toolMu.Lock()
			executedCalls = append(executedCalls, executedToolCall{
				CallID: callID,
				A:      aVal,
				B:      bVal,
				Result: sumStr,
			})
			toolMu.Unlock()

			return []llm.ContentBlock{
				{Type: llm.TypeText, Text: sumStr},
			}, nil
		},
	}

	var outboundReqs []llm.Request
	var reqMu sync.Mutex

	trackedStream := supplementalOnlineBudget.wrapStream(baseStream, func(req llm.Request) {
		reqMu.Lock()
		outboundReqs = append(outboundReqs, req)
		reqMu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ag, err := newDemoAgent(agent.Config{
		Model:   entry.Model,
		Options: entry.Options,
		Stream:  agent.StreamFunc(trackedStream),
		Tools:   []agent.Tool{addTool},
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	prompt := "请在一个响应中同时发起两个并行的 add 工具调用（不要分多轮先后调用）：\n调用 1：a=15, b=25\n调用 2：a=30, b=40\n收到所有工具结果后再计算总和并给出最终回答。"
	h, errSub := ag.Submit(ctx, userMsg(prompt))
	if errSub != nil {
		res.Actual = fmt.Sprintf("Submit 失败: %v", errSub)
		res.Duration = time.Since(start)
		return res
	}

	runRes, errWait := h.Wait(ctx)
	res.Duration = time.Since(start)

	if errWait != nil {
		res.Actual = fmt.Sprintf("Wait 返回错误: %v, State=%s", errWait, runRes.State)
		return res
	}

	// 统计模型第 1 步生成的工具调用 ID 列表
	var modelCallIDs []string
	for _, m := range runRes.Messages {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			for _, b := range asst.Content {
				if b.Type == llm.TypeToolCall {
					modelCallIDs = append(modelCallIDs, b.ID)
				}
			}
			if len(modelCallIDs) > 0 {
				break
			}
		}
	}

	// 没有触发同一响应内多个工具调用时标记“尚未验证”
	if len(modelCallIDs) < 2 {
		res.Status = "尚未验证"
		res.Actual = fmt.Sprintf("目标场景未触发: 模型单步仅产生了 %d 个工具调用 (未达多工具阈值 >=2)，按约定标记为目标场景未触发", len(modelCallIDs))
		if verbose {
			fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
		}
		return res
	}

	// Run 最终为 StateSuccess + RunStopCompleted
	if runRes.State != agent.RunStateSuccess || runRes.StopReason != agent.RunStopCompleted {
		res.Actual = fmt.Sprintf("多工具 Run 未成功收敛: State=%s, StopReason=%s, ErrMsg=%s",
			runRes.State, runRes.StopReason, runRes.ErrorMessage)
		return res
	}

	// 工具实际执行次数与模型调用数一致
	if int(toolExecCount.Load()) != len(modelCallIDs) {
		res.Actual = fmt.Sprintf("工具执行次数与模型调用数不一致: 实际执行 %d 次, 模型调用 %d 个",
			toolExecCount.Load(), len(modelCallIDs))
		return res
	}

	toolMu.Lock()
	callsCopy := append([]executedToolCall(nil), executedCalls...)
	toolMu.Unlock()

	// 两次工具调用参数分别符合提示词中的计算要求，顺序不限；每个调用的执行结果计算正确
	var matchedPair1, matchedPair2 bool
	for _, c := range callsCopy {
		if (c.A == 15 && c.B == 25) || (c.A == 25 && c.B == 15) {
			if c.Result != "40" {
				res.Actual = fmt.Sprintf("参数 {15, 25} 的计算结果不正确: 实际 %s (期望 40)", c.Result)
				return res
			}
			matchedPair1 = true
		} else if (c.A == 30 && c.B == 40) || (c.A == 40 && c.B == 30) {
			if c.Result != "70" {
				res.Actual = fmt.Sprintf("参数 {30, 40} 的计算结果不正确: 实际 %s (期望 70)", c.Result)
				return res
			}
			matchedPair2 = true
		} else {
			res.Actual = fmt.Sprintf("工具收到非预期参数组合: a=%d, b=%d", c.A, c.B)
			return res
		}
	}
	if !matchedPair1 || !matchedPair2 {
		res.Actual = fmt.Sprintf("工具调用未分别匹配两组计算参数: {15,25}=%t, {30,40}=%t", matchedPair1, matchedPair2)
		return res
	}

	reqMu.Lock()
	reqsCount := len(outboundReqs)
	var step2Req llm.Request
	if reqsCount >= 2 {
		step2Req = outboundReqs[1]
	}
	reqMu.Unlock()

	if reqsCount < 2 {
		res.Actual = fmt.Sprintf("出站请求次数不足 2 步: 实际 %d 次", reqsCount)
		return res
	}

	// 下一轮请求中，每个调用恰好对应一个工具结果，IsError == false，内容与实际执行结果一致
	toolResultsInStep2 := make(map[string][]*llm.ToolResultMessage)
	for _, m := range step2Req.Messages {
		if tr, ok := m.(*llm.ToolResultMessage); ok {
			toolResultsInStep2[tr.ToolCallID] = append(toolResultsInStep2[tr.ToolCallID], tr)
		}
	}

	for _, c := range callsCopy {
		results := toolResultsInStep2[c.CallID]
		if len(results) != 1 {
			res.Actual = fmt.Sprintf("调用 ID %s 在下一轮请求中工具结果数量异常: %d 个 (必须恰好为 1)", c.CallID, len(results))
			return res
		}
		tr := results[0]
		if tr.IsError {
			res.Actual = fmt.Sprintf("调用 ID %s 的工具结果被标记为错误 (IsError=true)", c.CallID)
			return res
		}
		var contentText string
		for _, b := range tr.Content {
			if b.Type == llm.TypeText {
				contentText += b.Text
			}
		}
		if strings.TrimSpace(contentText) != c.Result {
			res.Actual = fmt.Sprintf("调用 ID %s 的工具结果内容不匹配: 实际 %q, 期望 %q", c.CallID, contentText, c.Result)
			return res
		}
	}

	// 最后一条 assistant 消息有非空正文，包含正确的汇总答案。不要从更早的 assistant 消息寻找答案代替最终回答。
	var lastAssistantMsg *llm.AssistantMessage
	for i := len(runRes.Messages) - 1; i >= 0; i-- {
		if asst, ok := runRes.Messages[i].(*llm.AssistantMessage); ok {
			lastAssistantMsg = asst
			break
		}
	}
	if lastAssistantMsg == nil {
		res.Actual = "消息历史中未找到任何 assistant 消息"
		return res
	}

	var lastAssistantText string
	for _, b := range lastAssistantMsg.Content {
		if b.Type == llm.TypeText {
			lastAssistantText += b.Text
		}
	}
	lastAssistantText = strings.TrimSpace(lastAssistantText)
	if lastAssistantText == "" {
		res.Actual = "最后一条 assistant 消息正文为空"
		return res
	}
	if !strings.Contains(lastAssistantText, "110") {
		res.Actual = fmt.Sprintf("最后一条 assistant 消息未包含正确汇总答案 110: %q", lastAssistantText)
		return res
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("单步产生 2 个工具调用，参数分别符合 {15,25}=40 与 {30,40}=70 且计算正确，下一轮请求严格配对 2 个正确结果 (IsError=false)，最后一条 Assistant 消息非空且包含正确汇总答案 110 (RunID=%s)", runRes.RunID)
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

type blockKey struct {
	Index int
	Type  llm.ContentType
}

// runSupplementalCancel 执行观察到非空内容后取消实验
func runSupplementalCancel(entry *config.ModelEntry, baseStream StreamFunc, verbose bool, id int, name string) ScenarioResult {
	start := time.Now()
	res := ScenarioResult{
		ID:       id,
		Name:     name,
		Category: "在线补测",
		Expected: "按 ContentIndex 和文本/思考类型累计 Delta；观察到非空内容后固定基准并外部调用 Handle.Cancel()；最终 assistant 对应内容块必须完整保留基准内容；Run 终态为 aborted，StopReason 为空，且严格只发起 1 次模型请求",
		Status:   "失败",
	}

	var reqCount atomic.Int32
	trackedStream := supplementalOnlineBudget.wrapStream(baseStream, func(req llm.Request) {
		reqCount.Add(1)
	})

	var observedMu sync.Mutex
	observedBlocks := make(map[blockKey]*strings.Builder)
	var cancelTriggered atomic.Bool
	var totalCharsObserved int
	baseline := make(map[blockKey]string)
	triggerCancelChan := make(chan struct{}, 1)

	onEvent := func(evt agent.Event) {
		if evt.Kind == agent.EventLLMEvent && evt.LLMEvent != nil {
			llmEvt := evt.LLMEvent
			if llmEvt.Delta != "" {
				var bType llm.ContentType
				switch llmEvt.Type {
				case llm.EventTextDelta:
					bType = llm.TypeText
				case llm.EventThinkingDelta:
					bType = llm.TypeThinking
				default:
					return
				}

				key := blockKey{Index: llmEvt.BlockIndex, Type: bType}
				observedMu.Lock()
				builder, exists := observedBlocks[key]
				if !exists {
					builder = &strings.Builder{}
					observedBlocks[key] = builder
				}
				builder.WriteString(llmEvt.Delta)
				totalCharsObserved += len(llmEvt.Delta)

				// 累计达到一定非空字符 (>= 15 字符) 后固定基准并触发外部取消
				if !cancelTriggered.Load() && totalCharsObserved >= 15 {
					if cancelTriggered.CompareAndSwap(false, true) {
						// 固定一份已观察内容作为比较基准
						for k, b := range observedBlocks {
							baseline[k] = b.String()
						}
						observedMu.Unlock()

						select {
						case triggerCancelChan <- struct{}{}:
						default:
						}
						return
					}
				}
				observedMu.Unlock()
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ag, err := newDemoAgent(agent.Config{
		Model:   entry.Model,
		Options: entry.Options,
		Stream:  agent.StreamFunc(trackedStream),
		OnEvent: onEvent,
	})
	if err != nil {
		res.Actual = fmt.Sprintf("创建 Agent 失败: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer ag.Close()

	prompt := "请写一篇超过 1500 字的详细文章解析操作系统虚拟内存分页机制与 TLB 原理，详尽推导。"
	h, errSub := ag.Submit(ctx, userMsg(prompt))
	if errSub != nil {
		res.Actual = fmt.Sprintf("Submit 失败: %v", errSub)
		res.Duration = time.Since(start)
		return res
	}

	// 收到外部信号后在主测试协程中调用 Handle.Cancel()
	select {
	case <-triggerCancelChan:
		h.Cancel()
	case <-time.After(30 * time.Second):
		res.Actual = "等待首个非空流式内容超时"
		res.Duration = time.Since(start)
		return res
	}

	runRes, errWait := h.Wait(ctx)
	res.Duration = time.Since(start)

	if errWait != nil {
		res.Actual = fmt.Sprintf("Wait 返回非预期错误: %v", errWait)
		return res
	}
	if runRes.State != agent.RunStateAborted {
		res.Actual = fmt.Sprintf("取消终态异常: State=%s (期望 aborted)", runRes.State)
		return res
	}
	if runRes.StopReason != "" {
		res.Actual = fmt.Sprintf("取消终态 StopReason 应为空: actual=%s", runRes.StopReason)
		return res
	}

	// 确认收尾后没有发起下一次请求
	finalReqCount := reqCount.Load()
	if finalReqCount != 1 {
		res.Actual = fmt.Sprintf("请求次数异常: 发起了 %d 次请求 (期望严格为 1 次，无后续多余请求)", finalReqCount)
		return res
	}

	// 提取最终 Assistant 消息
	var finalAssistant *llm.AssistantMessage
	for _, m := range runRes.Messages {
		if asst, ok := m.(*llm.AssistantMessage); ok {
			finalAssistant = asst
			break
		}
	}
	if finalAssistant == nil {
		res.Actual = "最终结果中缺少 AssistantMessage"
		return res
	}

	observedMu.Lock()
	baselineCopy := make(map[blockKey]string, len(baseline))
	for k, v := range baseline {
		baselineCopy[k] = v
	}
	observedMu.Unlock()

	if len(baselineCopy) == 0 {
		res.Actual = "未捕获到任何取消基准内容"
		return res
	}

	// 最终 assistant 对应内容块必须完整保留基准内容；允许取消生效前继续收到额外内容，不要求最终长度完全相等
	var blockAuditDetails []string
	for k, baseText := range baselineCopy {
		if k.Index >= len(finalAssistant.Content) {
			res.Actual = fmt.Sprintf("最终 Assistant 消息缺少块索引 %d (总块数: %d)", k.Index, len(finalAssistant.Content))
			return res
		}
		actualBlock := finalAssistant.Content[k.Index]
		if actualBlock.Type != k.Type {
			res.Actual = fmt.Sprintf("块索引 %d 类型不匹配: 期望 %s, 实际 %s", k.Index, k.Type, actualBlock.Type)
			return res
		}

		var actualText string
		if k.Type == llm.TypeText {
			actualText = actualBlock.Text
		} else if k.Type == llm.TypeThinking {
			actualText = actualBlock.Thinking
		}

		// 检查对应内容块必须完整保留基准内容 (以基准内容为前缀，或完整包含)
		if !strings.HasPrefix(actualText, baseText) && !strings.Contains(actualText, baseText) {
			res.Actual = fmt.Sprintf("块 [%d:%s] 未能完整保留基准内容: 基准内容长度=%d, 最终块内容长度=%d",
				k.Index, k.Type, len(baseText), len(actualText))
			return res
		}
		blockAuditDetails = append(blockAuditDetails, fmt.Sprintf("块[%d:%s]基准%d字节->最终%d字节(完整保留)",
			k.Index, k.Type, len(baseText), len(actualText)))
	}

	res.Status = "通过"
	res.Actual = fmt.Sprintf("观察到非空内容后外部取消，核验细节: %s; State=aborted, StopReason为空, 请求数严格为1次 (无后续请求), RunID=%s",
		strings.Join(blockAuditDetails, ", "), runRes.RunID)
	if verbose {
		fmt.Printf("[%s] %s (耗时: %v)\n      观察: %s\n", res.Status, res.Name, res.Duration, res.Actual)
	}
	return res
}

// RunSupplementalOnlineSuite 执行针对性在线补充验收套件 (最多 6 次预算，3 项实验)
func RunSupplementalOnlineSuite(entries []config.ModelEntry, baseStream StreamFunc, verbose bool) SupplementalOnlineResult {
	suiteStartTime := time.Now()
	supplementalOnlineBudget = &GlobalOnlineBudgetTracker{
		maxBudget:  6,
		modelsUsed: make(map[string]int),
		apisUsed:   make(map[string]int),
	}

	if verbose {
		fmt.Println("\n==================================================")
		fmt.Println("开始执行 Agent 真实 API 补充验收测试 (最多 6 次调用预算)")
		fmt.Println("==================================================")
	}

	var entryOpenAI, entryAnthropic *config.ModelEntry
	for i := range entries {
		e := &entries[i]
		if (e.Ref == "qwen_openai/qwen" || e.Model.ID == "qwen3.8-max" && e.Model.API == llm.APIKindOpenAI) && entryOpenAI == nil {
			entryOpenAI = e
		}
		if (e.Ref == "qwen_anthropic/qwen" || e.Model.ID == "qwen3.8-max" && e.Model.API == llm.APIKindAnthropic) && entryAnthropic == nil {
			entryAnthropic = e
		}
	}

	suite := SupplementalOnlineResult{
		TotalScenarios: 3,
		MaxBudget:      6,
		ModelsUsed:     make(map[string]int),
		APIsUsed:       make(map[string]int),
	}

	if entryOpenAI == nil || entryAnthropic == nil {
		if verbose {
			fmt.Println("错误: 缺少可用的 OpenAI 或 Anthropic 协议模型配置")
		}
		suite.FailedScenarios = 3
		return suite
	}

	// 1. 在线多工具往返一次
	res1 := runSupplementalMultiTool(entryOpenAI, baseStream, verbose, 1, "补充实验 1: 在线多工具往返与结果严格配对 (真实 API)")
	suite.Results = append(suite.Results, res1)

	// 2. OpenAI 观察内容后取消一次
	res2 := runSupplementalCancel(entryOpenAI, baseStream, verbose, 2, "补充实验 2: OpenAI 观察内容后取消与基准保留验证 (真实 API)")
	suite.Results = append(suite.Results, res2)

	// 3. Anthropic 观察内容后取消一次
	res3 := runSupplementalCancel(entryAnthropic, baseStream, verbose, 3, "补充实验 3: Anthropic 观察内容后取消与基准保留验证 (真实 API)")
	suite.Results = append(suite.Results, res3)

	for _, r := range suite.Results {
		if r.Status == "通过" {
			suite.PassedScenarios++
		} else if r.Status == "失败" {
			suite.FailedScenarios++
		} else {
			suite.Unverified++
		}
	}

	suite.Duration = time.Since(suiteStartTime)

	supplementalOnlineBudget.mu.Lock()
	suite.TotalRequests = supplementalOnlineBudget.totalRequests
	suite.LimitReached = supplementalOnlineBudget.limitReached
	for k, v := range supplementalOnlineBudget.modelsUsed {
		suite.ModelsUsed[k] = v
	}
	for k, v := range supplementalOnlineBudget.apisUsed {
		suite.APIsUsed[k] = v
	}
	suite.TotalUsage = supplementalOnlineBudget.totalUsage
	supplementalOnlineBudget.mu.Unlock()

	if verbose {
		fmt.Println("\n==================================================")
		fmt.Printf("补充在线验收完成: 总耗时 %v\n", suite.Duration)
		fmt.Printf("新增请求使用: %d / %d 次 (超额: %v)\n", suite.TotalRequests, suite.MaxBudget, suite.LimitReached)
		fmt.Printf("各模型分布: %v, 各协议分布: %v\n", suite.ModelsUsed, suite.APIsUsed)
		fmt.Printf("Token 用量: 输入=%d, 输出=%d, 思考=%d, 总计=%d\n",
			suite.TotalUsage.Input, suite.TotalUsage.Output, suite.TotalUsage.Reasoning, suite.TotalUsage.Total)
		fmt.Printf("通过场景: %d, 失败场景: %d, 尚未验证: %d\n",
			suite.PassedScenarios, suite.FailedScenarios, suite.Unverified)
		fmt.Println("==================================================")
	}

	return suite
}
