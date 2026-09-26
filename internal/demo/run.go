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
	if len(entries) == 0 {
		fmt.Println("尚未配置模型")
		return nil
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Println("\n==================================================")
		fmt.Println("CodeAgent 手动验证演示入口")
		fmt.Println("==================================================")
		fmt.Println("可选模型列表:")
		for i, entry := range entries {
			keyStatus := "未配置"
			if entry.Options.APIKey != "" {
				keyStatus = "已配置"
			}
			fmt.Printf("  [%d] %s (真实 ID: %s, 协议: %s, Provider: %s, APIKey: %s)\n",
				i+1, entry.Ref, entry.Model.ID, entry.Model.API, entry.Model.Provider, keyStatus)
		}
		fmt.Println("--------------------------------------------------")
		fmt.Print("请输入模型编号选择模型 (输入 q 退出): ")

		input, err := readLine(reader)
		if err != nil {
			return nil
		}
		input = strings.TrimSpace(input)
		if input == "q" || input == "quit" || input == "exit" {
			fmt.Println("退出演示程序。")
			return nil
		}

		idx, err := strconv.Atoi(input)
		if err != nil || idx < 1 || idx > len(entries) {
			fmt.Printf("输入无效: %q，请输入 1 到 %d 之间的模型编号。\n", input, len(entries))
			continue
		}

		selectedEntry := entries[idx-1]
		runModelSession(reader, selectedEntry, stream)
	}
}

func runModelSession(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc) {
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
		fmt.Println("  [1] 文本与思考流式对话 (支持 Ctrl+C 取消)")
		fmt.Println("  [2] 加法工具调用往返 (整数相加 add，最多 3 轮)")
		fmt.Println("  [3] Complete 同步调用验证 (检查文本/思考/状态/用量/取消)")
		fmt.Println("  [4] 重新选择模型")
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
		case "4", "r", "reselect":
			return // 返回模型选择循环
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
