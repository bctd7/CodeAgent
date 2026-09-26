package demo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

func runChatExperiment(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc) {
	fmt.Println("\n--- 实验模式 1: 文本与思考流式对话 ---")
	fmt.Println("说明: 发起流式请求，实时显示文本与思考内容。支持在生成过程中按 Ctrl+C 中断本次请求。")
	fmt.Print("请输入提示词 (回车使用默认: \"你好，请做个简短自我介绍\"): ")

	prompt, err := readLine(reader)
	if err != nil {
		return
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = "你好，请做个简短自我介绍"
	}

	req := llm.Request{
		Model:   entry.Model,
		Options: entry.Options,
		Messages: []llm.Message{
			&llm.UserMessage{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					{Type: llm.TypeText, Text: prompt},
				},
				Timestamp: time.Now(),
			},
		},
	}

	// 捕获 SIGINT，支持 Ctrl+C 取消本次请求
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Println("\n[开始发起流式请求，按 Ctrl+C 可中断]...")
	streamReader, err := stream(ctx, req)
	if err != nil {
		fmt.Printf("\n[建流失败]: %v\n", err)
		return
	}
	defer streamReader.Close()

	audit := newAuditRecord(1)
	inThinking := false

	for {
		event, ok := streamReader.Next(context.Background())
		if !ok {
			break
		}
		audit.recordEvent(event)

		switch event.Type {
		case llm.EventThinkingBlockStart:
			inThinking = true
			fmt.Print("\n[思考开始]\n")
		case llm.EventThinkingDelta:
			fmt.Print(event.Delta)
		case llm.EventThinkingBlockEnd:
			inThinking = false
			fmt.Print("\n[思考结束]\n")

		case llm.EventTextBlockStart:
			fmt.Print("\n[回复内容]: ")
		case llm.EventTextDelta:
			fmt.Print(event.Delta)
		case llm.EventTextBlockEnd:
			fmt.Println()

		case llm.EventToolCallBlockStart:
			fmt.Print("\n[工具调用开始]\n")
		case llm.EventToolCallDelta:
			// 工具参数增量，不直接打断文本流
		case llm.EventToolCallBlockEnd:
			fmt.Print("\n[工具调用结束]\n")

		case llm.EventMessageComplete:
			fmt.Print("\n[流完成 (State: success)]\n")
		case llm.EventMessageError:
			if inThinking {
				fmt.Println("\n[思考中断]")
			}
			state := "error"
			if event.FinalMessage != nil {
				state = string(event.FinalMessage.State)
			}
			fmt.Printf("\n[流终止 (State: %s)]\n", state)
		}
	}

	result := streamReader.Result()
	audit.result = result

	if result.Message.State == llm.StateAborted {
		fmt.Printf("\n[请求已被取消]: %s\n", result.Message.ErrorMessage)
		if len(result.Message.Content) > 0 {
			fmt.Println("--- 已保留的中断前生成内容 ---")
			for _, b := range result.Message.Content {
				if b.Type == llm.TypeText && b.Text != "" {
					fmt.Printf("[已接收文本]: %s\n", b.Text)
				}
				if b.Type == llm.TypeThinking && b.Thinking != "" {
					fmt.Printf("[已接收思考]: %s\n", b.Thinking)
				}
			}
			fmt.Println("-------------------------------")
		}
	} else if result.Message.State == llm.StateError {
		fmt.Printf("\n[调用失败]: %s\n", result.Message.ErrorMessage)
		if result.Message.StopReason == llm.StopSafety {
			fmt.Println("[安全拦截]: 内容被模型安全过滤器拦截 (StopSafety)")
		}
	}

	report := audit.verify(nil, nil)
	printAuditReport(fmt.Sprintf("流式对话 - %s", entry.Ref), report)
}
