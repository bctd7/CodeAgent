package demo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

// StreamFunc 是 demo 调用底层流式调度的统一抽象，使 demo 不反向依赖 provider 包。
type StreamFunc func(context.Context, llm.Request) (llm.StreamReader, error)

// Run 启动控制台手动交互与验收演示入口。
func Run(entries []config.ModelEntry, stream StreamFunc) error {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Println("\n==================================================")
		fmt.Println("CodeAgent 交互演示与验收入口")
		fmt.Println("==================================================")
		fmt.Println("快捷操作:")
		fmt.Println("  [A] 一键运行 Agent 离线自动化验收套件 (12 项场景，无须凭据)")
		fmt.Println("--------------------------------------------------")
		fmt.Println("可选配置模型列表:")
		for i, entry := range entries {
			keyStatus := "未配置"
			if entry.Options.APIKey != "" {
				keyStatus = "已配置"
			}
			fmt.Printf("  [%d] %s (真实 ID: %s, 协议: %s, Provider: %s, APIKey: %s)\n",
				i+1, entry.Ref, entry.Model.ID, entry.Model.API, entry.Model.Provider, keyStatus)
		}
		fmt.Println("--------------------------------------------------")
		fmt.Print("请输入选项 (输入 A 离线验收, 输入数字选择模型, 输入 q 退出): ")

		input, err := readLine(reader)
		if err != nil {
			return nil
		}
		input = strings.TrimSpace(input)
		if input == "q" || input == "quit" || input == "exit" {
			fmt.Println("退出演示程序。")
			return nil
		}

		if strings.EqualFold(input, "a") || strings.EqualFold(input, "offline") {
			suite := RunOfflineAcceptance(true)
			reportFile := "docs/Agent验收报告.md"
			if err := SaveAcceptanceReport(reportFile, suite, nil, nil); err != nil {
				fmt.Printf("保存验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[验收报告已保存至]: %s\n", reportFile)
			}
			continue
		}

		if len(entries) == 0 {
			fmt.Println("当前未配置任何模型，仅可使用 [A] 离线验收模式。")
			continue
		}

		idx, err := strconv.Atoi(input)
		if err != nil || idx < 1 || idx > len(entries) {
			fmt.Printf("输入无效: %q，请输入 A 或 1 到 %d 之间的模型编号。\n", input, len(entries))
			continue
		}

		selectedEntry := entries[idx-1]
		runModelSession(reader, selectedEntry, stream, entries)
	}
}

func runModelSession(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc, allEntries []config.ModelEntry) {
	for {
		fmt.Println("\n--------------------------------------------------")
		fmt.Printf("当前已选模型: %s\n", entry.Ref)
		fmt.Printf("  - 真实 ID:      %s\n", entry.Model.ID)
		fmt.Printf("  - 协议:         %s\n", entry.Model.API)
		fmt.Printf("  - Provider:     %s\n", entry.Model.Provider)
		fmt.Printf("  - BaseURL:      %s\n", entry.Model.BaseURL)
		fmt.Printf("  - MaxTokens:    %d\n", entry.Model.MaxTokens)
		fmt.Printf("  - ContextWindow:%d\n", entry.Model.ContextWindow)
		if entry.Options.OpenAI != nil {
			fmt.Printf("  - OpenAI 选项:  reasoning_effort=%q, max_tokens_field=%q\n",
				entry.Options.OpenAI.ReasoningEffort, entry.Options.OpenAI.MaxTokensField)
		}
		if entry.Options.Anthropic != nil {
			fmt.Printf("  - Anthropic 选项: thinking=%q, budget_tokens=%d, effort=%q\n",
				entry.Options.Anthropic.Thinking, entry.Options.Anthropic.BudgetTokens, entry.Options.Anthropic.Effort)
		}
		fmt.Println("请选择实验模式:")
		fmt.Println("  [1] 文本与思考流式对话 (纯 LLM 流式对话)")
		fmt.Println("  [2] 加法工具调用往返 (纯 LLM 工具往返)")
		fmt.Println("  [3] Complete 同步调用验证 (纯 LLM 同步调用验证)")
		fmt.Println("  [4] Agent 核心任务执行 (交互控制台: Submit/Steer/Cancel/Snapshot/SetModel/Wait)")
		fmt.Println("  [5] Agent 离线自动化验收套件 (14 项完整离线用例)")
		fmt.Println("  [6] Agent 在线受控验收模式 (受控真实 API 测试入口)")
		fmt.Println("  [7] Agent 针对性补充在线验收 (5 项强校验实验，<=16 次预算)")
		fmt.Println("  [8] 重新选择模型")
		fmt.Println("  [0] 退出程序")
		fmt.Print("请输入选项编号: ")

		choice, err := readLine(reader)
		if err != nil {
			return
		}
		choice = strings.TrimSpace(choice)
		switch choice {
		case "1":
			runChatExperiment(reader, entry, stream)
		case "2":
			runToolExperiment(reader, entry, stream)
		case "3":
			runCompleteExperiment(reader, entry, stream)
		case "4":
			runAgentExperiment(reader, entry, stream, allEntries)
		case "5":
			suite := RunOfflineAcceptance(true)
			reportFile := "docs/Agent验收报告.md"
			if err := SaveAcceptanceReport(reportFile, suite, nil, nil); err != nil {
				fmt.Printf("保存验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[验收报告已保存至]: %s\n", reportFile)
			}
		case "6":
			runOnlineAcceptanceMenu(reader, entry, stream, allEntries)
		case "7":
			suppSuite := RunSupplementalOnlineSuite(allEntries, stream, true)
			offlineSuite := RunOfflineAcceptance(false)
			reportFile := "docs/Agent验收报告.md"
			if err := SaveAcceptanceReport(reportFile, offlineSuite, nil, &suppSuite); err != nil {
				fmt.Printf("保存验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[补充验收报告已保存至]: %s\n", reportFile)
			}
		case "8", "r", "reselect":
			return // 返回主菜单
		case "0", "q", "quit", "exit":
			fmt.Println("退出演示程序。")
			os.Exit(0)
		default:
			fmt.Printf("输入无效: %q，请重新输入选项编号。\n", choice)
		}
	}
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
