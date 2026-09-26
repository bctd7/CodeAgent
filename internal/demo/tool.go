package demo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

var addTool = llm.Tool{
	Name:        "add",
	Description: "计算两个整数相加之和 (a + b)",
	Schema: []byte(`{
		"type": "object",
		"properties": {
			"a": {
				"type": "integer",
				"description": "第一个加数"
			},
			"b": {
				"type": "integer",
				"description": "第二个加数"
			}
		},
		"required": ["a", "b"]
	}`),
}

func runToolExperiment(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc) {
	fmt.Println("\n--- 实验模式 2: 加法工具调用往返 ---")
	fmt.Println("说明: 向模型下发加法工具 (add)，执行 声明工具 → 收到调用 → 本地计算 → 回传结果 → 获取最终回答 的完整闭环。最多进行 3 轮模型调用。")
	fmt.Print("请输入提示词 (回车使用默认: \"请计算 42 加 58 的和，必须调用 add 工具计算\"): ")

	prompt, err := readLine(reader)
	if err != nil {
		return
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = "请计算 42 加 58 的和，必须调用 add 工具计算"
	}

	messages := []llm.Message{
		&llm.UserMessage{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: llm.TypeText, Text: prompt},
			},
			Timestamp: time.Now(),
		},
	}

	const maxTurns = 3
	var expectedToolIDs []string
	var matchedToolIDs []string
	var auditRecords []*auditRecord

	for turn := 1; turn <= maxTurns; turn++ {
		fmt.Printf("\n>>> [第 %d 轮模型调用开始] <<<\n", turn)

		req := llm.Request{
			Model:    entry.Model,
			Options:  entry.Options,
			Messages: messages,
			Tools:    []llm.Tool{addTool},
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		streamReader, err := stream(ctx, req)
		if err != nil {
			stop()
			fmt.Printf("\n[建流失败]: %v\n", err)
			break
		}

		audit := newAuditRecord(turn)
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
				fmt.Print("\n[收到工具调用通知]\n")
			case llm.EventToolCallDelta:
				// 参数增量暂不打断控制台
			case llm.EventToolCallBlockEnd:
				fmt.Print("[工具参数接收完毕]\n")

			case llm.EventMessageComplete:
				fmt.Print("\n[本轮调用流完成 (State: success)]\n")
			case llm.EventMessageError:
				if inThinking {
					fmt.Println("\n[思考中断]")
				}
				state := "error"
				if event.FinalMessage != nil {
					state = string(event.FinalMessage.State)
				}
				fmt.Printf("\n[本轮调用流终止 (State: %s)]\n", state)
			}
		}

		streamReader.Close()
		stop()

		result := streamReader.Result()
		audit.result = result
		auditRecords = append(auditRecords, audit)

		if result.Message.State == llm.StateAborted {
			fmt.Printf("\n[第 %d 轮请求已被取消]: %s\n", turn, result.Message.ErrorMessage)
			break
		} else if result.Message.State == llm.StateError {
			fmt.Printf("\n[第 %d 轮请求失败]: %s\n", turn, result.Message.ErrorMessage)
			break
		}

		// 检查本轮是否产生了工具调用
		var toolCallBlocks []llm.ContentBlock
		for _, b := range result.Message.Content {
			if b.Type == llm.TypeToolCall {
				toolCallBlocks = append(toolCallBlocks, b)
			}
		}

		if len(toolCallBlocks) == 0 {
			fmt.Println("\n[模型未产生工具调用，交互正常结束]")
			break
		}

		// 保留完整 assistant 消息（包含思考内容和签名）参与后续往返
		asstCopy := result.Message
		messages = append(messages, &asstCopy)

		// 执行工具并构造 ToolResultMessage
		for _, tc := range toolCallBlocks {
			expectedToolIDs = append(expectedToolIDs, tc.ID)
			fmt.Printf("\n[处理工具调用]: ID=%s, 名称=%s, 参数=%v\n", tc.ID, tc.Name, tc.Arguments)

			if tc.Name == "add" {
				a, b, parseErr := parseAddArgs(tc.Arguments)
				if parseErr != nil {
					fmt.Printf("[工具参数错误]: %v\n", parseErr)
					messages = append(messages, &llm.ToolResultMessage{
						Role:       llm.RoleTool,
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Content: []llm.ContentBlock{
							{Type: llm.TypeText, Text: fmt.Sprintf("参数错误: %v", parseErr)},
						},
						IsError:   true,
						Timestamp: time.Now(),
					})
				} else {
					sum := a + b
					fmt.Printf("[本地执行成功] 计算 %d + %d = %d\n", a, b, sum)
					messages = append(messages, &llm.ToolResultMessage{
						Role:       llm.RoleTool,
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Content: []llm.ContentBlock{
							{Type: llm.TypeText, Text: strconv.Itoa(sum)},
						},
						IsError:   false,
						Timestamp: time.Now(),
					})
				}
			} else {
				fmt.Printf("[未知工具]: %s\n", tc.Name)
				messages = append(messages, &llm.ToolResultMessage{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					Content: []llm.ContentBlock{
						{Type: llm.TypeText, Text: fmt.Sprintf("未知工具: %s", tc.Name)},
					},
					IsError:   true,
					Timestamp: time.Now(),
				})
			}
			matchedToolIDs = append(matchedToolIDs, tc.ID)
		}

		if turn == maxTurns {
			fmt.Println("\n[已达到最大演示轮数 (3 轮)，停止循环]")
		}
	}

	// 打印每轮调用的验收核对报告
	for _, a := range auditRecords {
		report := a.verify(expectedToolIDs, matchedToolIDs)
		printAuditReport(fmt.Sprintf("加法工具演示 (第 %d 轮) - %s", a.turnIndex, entry.Ref), report)
	}
}

func parseAddArgs(args map[string]any) (int, int, error) {
	if args == nil {
		return 0, 0, fmt.Errorf("参数对象为 nil")
	}
	aRaw, okA := args["a"]
	bRaw, okB := args["b"]
	if !okA || !okB {
		return 0, 0, fmt.Errorf("缺少必需参数 a 或 b (收到: %v)", args)
	}
	a, errA := toInt(aRaw)
	if errA != nil {
		return 0, 0, fmt.Errorf("参数 a 非法: %w", errA)
	}
	b, errB := toInt(bRaw)
	if errB != nil {
		return 0, 0, fmt.Errorf("参数 b 非法: %w", errB)
	}
	return a, b, nil
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("浮点数 %v 无法无损转换为整数", n)
		}
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, err
		}
		return int(i), nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("字符串 %q 不是合法整数", n)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("不支持的数值类型: %T (%v)", v, v)
	}
}
