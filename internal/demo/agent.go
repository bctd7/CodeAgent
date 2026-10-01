package demo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

// runAgentExperiment 提供 Agent 核心任务执行的手动交互控制台。
func runAgentExperiment(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc, allEntries []config.ModelEntry) {
	fmt.Println("\n==================================================")
	fmt.Println("模式 4: Agent 核心任务执行与交互控制台")
	fmt.Println("==================================================")
	fmt.Printf("初始模型: %s (协议: %s, 真实 ID: %s)\n", entry.Ref, entry.Model.API, entry.Model.ID)
	fmt.Println("说明: Agent 在后台管理任务队列、内存历史、工具调度、纠正、取消与跨模型切换。")

	tools := []agent.Tool{
		agent.NewAddTool(),
		agent.NewSleepTool(),
		agent.NewEchoTool(),
	}

	// 事件回调保持简短，只记录与格式化展示，绝不在回调中调用阻塞式 Wait 或 Close。
	onEvent := func(evt agent.Event) {
		toolName := evt.ToolName
		if toolName == "" {
			toolName = "-"
		}
		callID := evt.ToolCallID
		if callID == "" {
			callID = "-"
		}
		runID := evt.RunID
		if runID == "" {
			runID = "-"
		}
		stepStr := "-"
		if evt.StepIndex > 0 {
			stepStr = strconv.Itoa(evt.StepIndex)
		}

		if evt.Kind == agent.EventLLMEvent {
			if evt.LLMEvent != nil {
				switch evt.LLMEvent.Type {
				case llm.EventThinkingDelta:
					fmt.Print(evt.LLMEvent.Delta)
				case llm.EventTextDelta:
					fmt.Print(evt.LLMEvent.Delta)
				}
			}
			return
		}

		detail := evt.Detail
		if evt.Kind == agent.EventRunEnd && evt.Result != nil {
			detail = fmt.Sprintf("终态=%s, StopReason=%s", evt.Result.State, evt.Result.StopReason)
			if evt.Result.ErrorMessage != "" {
				detail += fmt.Sprintf(", 错误: %s", evt.Result.ErrorMessage)
			}
		}

		fmt.Printf("\n[Agent事件] 类型=%-24s | RunID=%-8s | Step=%-3s | 工具=%-8s | 调用ID=%-10s | %s\n",
			evt.Kind, runID, stepStr, toolName, callID, detail)
	}

	ag, err := newDemoAgent(agent.Config{
		Model:            entry.Model,
		Options:          entry.Options,
		SystemPrompt:     "你是一个全能编程与计算助手，能合理调用工具帮助用户解决问题。",
		Stream:           agent.StreamFunc(stream),
		Tools:            tools,
		OnEvent:          onEvent,
		MaxParallelTools: 4,
	})
	if err != nil {
		fmt.Printf("创建 Agent 失败: %v\n", err)
		return
	}
	defer ag.Close()

	// 任务句柄与提交信息维护
	var handlesMu sync.Mutex
	var handleList []*agent.RunHandle
	handleMap := make(map[string]*agent.RunHandle)
	promptMap := make(map[string]string)

	for {
		fmt.Println("\n--------------------------------------------------")
		fmt.Println("请选择 Agent 操作:")
		fmt.Println("  [1] 提交新任务 (Submit)")
		fmt.Println("  [2] 向活动任务提交纠正 (Steer)")
		fmt.Println("  [3] 列出已提交任务并选择取消 (Cancel)")
		fmt.Println("  [4] 查看 Agent 快照 (Snapshot)")
		fmt.Println("  [5] 选择模型并调用 SetModel")
		fmt.Println("  [6] 查看指定任务结果 (有界等待 Wait)")
		fmt.Println("  [0] 关闭 Agent 并返回上级菜单 (Close)")
		fmt.Print("请输入选项编号: ")

		choice, err := readLine(reader)
		if err != nil {
			return
		}
		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			fmt.Print("请输入任务提示词 (回车使用默认: \"请使用 add 工具计算 12 加 34，再计算 56 加 78，最后汇报两个结果\"): ")
			prompt, err := readLine(reader)
			if err != nil {
				return
			}
			prompt = strings.TrimSpace(prompt)
			if prompt == "" {
				prompt = "请使用 add 工具计算 12 加 34，再计算 56 加 78，最后汇报两个结果"
			}

			userMsg := &llm.UserMessage{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					{Type: llm.TypeText, Text: prompt},
				},
				Timestamp: time.Now(),
			}

			handle, err := ag.Submit(context.Background(), userMsg)
			if err != nil {
				fmt.Printf("提交任务失败: %v\n", err)
				continue
			}

			handlesMu.Lock()
			handleList = append(handleList, handle)
			handleMap[handle.ID()] = handle
			promptMap[handle.ID()] = prompt
			handlesMu.Unlock()

			fmt.Printf(">>> 任务已成功提交 (RunID: %s)！<<<\n", handle.ID())
			fmt.Println("已保存任务句柄并在后台异步处理，立即返回操作菜单 (可在菜单 [6] 中有界等待或查看结果)。")

		case "2":
			fmt.Print("请输入对当前运行任务的纠正指令: ")
			steerText, err := readLine(reader)
			if err != nil {
				return
			}
			steerText = strings.TrimSpace(steerText)
			if steerText == "" {
				fmt.Println("纠正内容为空，已忽略。")
				continue
			}

			if err := ag.Steer(steerText); err != nil {
				fmt.Printf("提交纠正失败: %v\n", err)
			} else {
				fmt.Printf("纠正已成功发送给当前活动任务: %q\n", steerText)
			}

		case "3":
			handlesMu.Lock()
			tasks := make([]*agent.RunHandle, len(handleList))
			copy(tasks, handleList)
			handlesMu.Unlock()

			if len(tasks) == 0 {
				fmt.Println("当前尚未提交任何任务。")
				continue
			}

			fmt.Println("\n已提交的任务列表:")
			for i, h := range tasks {
				fmt.Printf("  [%d] RunID: %-8s (提示词: %q)\n", i+1, h.ID(), promptMap[h.ID()])
			}
			fmt.Print("请输入要取消的任务编号 (输入 c 取消操作): ")
			sel, err := readLine(reader)
			if err != nil {
				return
			}
			sel = strings.TrimSpace(sel)
			if sel == "c" || sel == "" {
				fmt.Println("已取消选择。")
				continue
			}
			idx, err := strconv.Atoi(sel)
			if err != nil || idx < 1 || idx > len(tasks) {
				fmt.Printf("输入无效编号: %q\n", sel)
				continue
			}
			targetHandle := tasks[idx-1]
			fmt.Printf("正在向任务 %s 发送取消请求 (Handle.Cancel())...\n", targetHandle.ID())
			targetHandle.Cancel()
			fmt.Printf("取消请求已发送给任务 %s。活动任务将收到中止信号，排队任务将直接出队。\n", targetHandle.ID())

		case "4":
			snap := ag.Snapshot()
			fmt.Println("\n=== Agent 状态快照 ===")
			fmt.Printf("  生命周期状态: %s\n", snap.Status)
			fmt.Printf("  当前活动 Run: %s\n", snap.CurrentRunID)
			fmt.Printf("  当前生效模型: %s (%s, Provider: %s)\n", snap.CurrentModel.ID, snap.CurrentModel.API, snap.CurrentModel.Provider)
			if snap.PendingModel != nil {
				fmt.Printf("  待生效新模型: %s (%s, Provider: %s)\n", snap.PendingModel.ID, snap.PendingModel.API, snap.PendingModel.Provider)
			}
			fmt.Printf("  排队任务数:   %d (IDs: %v)\n", snap.QueueLength, snap.QueuedRunIDs)
			fmt.Printf("  历史消息总数: %d 条\n", snap.HistoryCount)
			if snap.HistoryCount > 0 {
				fmt.Println("  历史消息概览:")
				for i, m := range snap.History {
					role := "unknown"
					preview := "-"
					switch msg := m.(type) {
					case *llm.UserMessage:
						role = string(msg.Role)
						if len(msg.Content) > 0 {
							preview = msg.Content[0].Text
						}
					case *llm.AssistantMessage:
						role = string(msg.Role)
						preview = fmt.Sprintf("State=%s, StopReason=%s, Blocks=%d", msg.State, msg.StopReason, len(msg.Content))
					case *llm.ToolResultMessage:
						role = string(msg.Role)
						preview = fmt.Sprintf("Tool=%s, CallID=%s, is_error=%v", msg.ToolName, msg.ToolCallID, msg.IsError)
					}
					if len(preview) > 50 {
						preview = preview[:50] + "..."
					}
					fmt.Printf("    [%d] %-10s %s\n", i+1, role, preview)
				}
			}
			fmt.Println("========================")

		case "5":
			fmt.Println("\n可选切换模型列表:")
			for i, e := range allEntries {
				fmt.Printf("  [%d] %s (真实 ID: %s, 协议: %s)\n", i+1, e.Ref, e.Model.ID, e.Model.API)
			}
			fmt.Print("请输入目标模型编号 (输入 c 取消): ")
			mIdxStr, err := readLine(reader)
			if err != nil {
				return
			}
			mIdxStr = strings.TrimSpace(mIdxStr)
			if mIdxStr == "c" || mIdxStr == "" {
				fmt.Println("已取消切换。")
				continue
			}
			mIdx, err := strconv.Atoi(mIdxStr)
			if err != nil || mIdx < 1 || mIdx > len(allEntries) {
				fmt.Println("模型编号无效。")
				continue
			}
			targetEntry := allEntries[mIdx-1]
			if err := ag.SetModel(targetEntry.Model, targetEntry.Options); err != nil {
				fmt.Printf("请求切换模型失败: %v\n", err)
			} else {
				fmt.Printf("已请求切换模型至: %s (%s)。\n说明: 当前若有请求/工具正在执行，将在下一次请求边界生效；若为空闲已立即生效。\n",
					targetEntry.Ref, targetEntry.Model.ID)
			}

		case "6":
			handlesMu.Lock()
			tasks := make([]*agent.RunHandle, len(handleList))
			copy(tasks, handleList)
			handlesMu.Unlock()

			if len(tasks) == 0 {
				fmt.Println("当前尚未提交任何任务。")
				continue
			}

			fmt.Println("\n已提交的任务列表:")
			for i, h := range tasks {
				fmt.Printf("  [%d] RunID: %-8s (提示词: %q)\n", i+1, h.ID(), promptMap[h.ID()])
			}
			fmt.Print("请输入要查看的任务编号 (默认最新任务，输入 c 取消): ")
			sel, err := readLine(reader)
			if err != nil {
				return
			}
			sel = strings.TrimSpace(sel)
			if sel == "c" {
				continue
			}
			targetHandle := tasks[len(tasks)-1]
			if sel != "" {
				idx, err := strconv.Atoi(sel)
				if err != nil || idx < 1 || idx > len(tasks) {
					fmt.Printf("输入无效编号: %q\n", sel)
					continue
				}
				targetHandle = tasks[idx-1]
			}

			fmt.Print("请输入等待超时时间(秒，默认 5 秒): ")
			tStr, err := readLine(reader)
			if err != nil {
				return
			}
			tStr = strings.TrimSpace(tStr)
			timeoutSec := 5
			if tStr != "" {
				if parsed, err := strconv.Atoi(tStr); err == nil && parsed > 0 {
					timeoutSec = parsed
				}
			}

			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
			fmt.Printf("正在对任务 %s 进行有界等待 (最长 %d 秒)...\n", targetHandle.ID(), timeoutSec)
			res, err := targetHandle.Wait(waitCtx)
			waitCancel()

			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					fmt.Printf("\n>> 等待超时 (%d 秒)，已结束等待返回操作菜单。\n", timeoutSec)
					fmt.Println(">> 提示：有界等待超时仅结束本次等待，后台任务仍在正常运行，之后可再次选择查看。")
				} else {
					fmt.Printf("\n>> 等待发生错误: %v\n", err)
				}
			} else {
				printRunResult(res)
			}

		case "0", "q", "quit", "exit":
			fmt.Println("正在关闭当前 Agent 实例 (清理排队并等待工作循环退出)...")
			if err := ag.Close(); err != nil {
				fmt.Printf("关闭 Agent 发生错误: %v\n", err)
			} else {
				fmt.Println("Agent 已成功关闭并释放资源，返回上级菜单。")
			}
			return

		default:
			fmt.Printf("输入无效: %q\n", choice)
		}
	}
}

// printRunResult 格式化展示单次 Run 的完整结果。
func printRunResult(res agent.RunResult) {
	fmt.Println("\n==================================================")
	fmt.Printf("[Run 最终执行结果] RunID: %s\n", res.RunID)
	fmt.Println("--------------------------------------------------")
	fmt.Printf("  执行状态 State:        %s\n", res.State)
	fmt.Printf("  结束原因 StopReason:   %s (原始服务端原因: %s)\n", res.StopReason, res.RawStopReason)
	if res.ErrorMessage != "" {
		fmt.Printf("  错误说明 ErrorMessage: %s\n", res.ErrorMessage)
	}
	fmt.Printf("  模型调用步数 Steps:    %d\n", res.Steps)
	fmt.Printf("  产生消息数量 Messages: %d 条\n", len(res.Messages))
	fmt.Printf("  总计 Token 用量:       输入=%d, 输出=%d, 思考=%d, 总计=%d\n",
		res.TotalUsage.Input, res.TotalUsage.Output, res.TotalUsage.Reasoning, res.TotalUsage.Total)
	if res.TotalUsage.Cost.Total > 0 {
		fmt.Printf("  总计预估费用:          $%.6f\n", res.TotalUsage.Cost.Total)
	}
	if len(res.Usage) > 0 {
		fmt.Println("  各模型用量明细:")
		for mKey, u := range res.Usage {
			fmt.Printf("    * %s: 输入=%d, 输出=%d, 思考=%d, 总计=%d\n", mKey, u.Input, u.Output, u.Reasoning, u.Total)
		}
	}
	if res.Error != nil {
		fmt.Printf("  底层 Error 对象:       %v\n", res.Error)
	}
	fmt.Println("==================================================")
}

// newDemoAgent 用显式驱动确认纯内存节点；没有 Session 或保存策略。
func newDemoAgent(cfg agent.Config) (*agent.Agent, error) {
	a, err := agent.New(cfg)
	if err != nil {
		return nil, err
	}
	driver, err := a.Driver()
	if err != nil {
		a.Close()
		return nil, err
	}
	go func() {
		for {
			checkpoint, err := driver.Next(context.Background())
			if err == io.EOF {
				return
			}
			if err != nil {
				a.StopExecution(err)
				continue
			}
			if err := driver.Resolve(context.Background(), checkpoint.ID, agent.Decision{Kind: agent.Continue}); err != nil {
				a.StopExecution(err)
			}
		}
	}()
	return a, nil
}
