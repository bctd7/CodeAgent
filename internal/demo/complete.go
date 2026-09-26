package demo

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"CodeAgent/internal/config"
	"CodeAgent/internal/llm"
)

func runCompleteExperiment(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc) {
	for {
		fmt.Println("\n--- 实验模式 3: Complete 同步调用与取消验证 (llm.Complete) ---")
		fmt.Printf("当前模型: %s (协议: %s, 真实 ID: %s)\n", entry.Ref, entry.Model.API, entry.Model.ID)
		fmt.Println("说明: 直接调用 llm.Complete()，检验一次性同步返回完整文本、思考、状态和用量，并检验取消时的 error 与 State 语义。")
		fmt.Println("请选择子模式:")
		fmt.Println("  [1] 正常 Complete 同步调用 (验证完整文本、思考、状态、用量)")
		fmt.Println("  [2] 取消 Complete 试验 (验证 Context 取消时 error 为 nil 但 State=aborted)")
		fmt.Println("  [3] 连续执行 1 和 2")
		fmt.Println("  [0] 返回上级菜单")
		fmt.Print("请输入子选项编号: ")

		choice, err := readLine(reader)
		if err != nil {
			return
		}
		choice = strings.TrimSpace(choice)
		switch choice {
		case "1":
			runSingleComplete(reader, entry, stream, false)
		case "2":
			runSingleComplete(reader, entry, stream, true)
		case "3":
			runSingleComplete(reader, entry, stream, false)
			runSingleComplete(reader, entry, stream, true)
		case "0", "q", "quit", "exit":
			return
		default:
			fmt.Printf("输入无效: %q\n", choice)
		}
	}
}

func runSingleComplete(reader *bufio.Reader, entry config.ModelEntry, stream StreamFunc, cancelMode bool) {
	var prompt string
	if cancelMode {
		fmt.Println("\n>>> [子实验: 取消 Complete 同步调用] <<<")
		fmt.Println("说明: 发起请求后在网络传输/生成中途自动触发 context 取消，核查返回的 error 与消息 State。")
		prompt = "请写一篇深入解析 Go 语言 GMP 调度器底层实现的长文，包含思考过程与详细推演。"
	} else {
		fmt.Println("\n>>> [子实验: 正常 Complete 同步调用] <<<")
		fmt.Print("请输入提示词 (回车使用默认: \"用三句话解释 Go 的 interface，请在回答前进行思考\"): ")
		p, err := readLine(reader)
		if err != nil {
			return
		}
		p = strings.TrimSpace(p)
		if p == "" {
			prompt = "用三句话解释 Go 的 interface，请在回答前进行思考"
		} else {
			prompt = p
		}
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

	var ctx context.Context
	var cancel context.CancelFunc

	if cancelMode {
		ctx, cancel = context.WithCancel(context.Background())
		// 延迟 150ms 触发取消：此时 HTTP 请求已发出，流正在建连或接收首批数据
		time.AfterFunc(150*time.Millisecond, func() {
			fmt.Println("\n[系统触发 Context 取消...]")
			cancel()
		})
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	}
	defer cancel()

	fmt.Printf("\n[开始执行 llm.Complete() 调用] 模型=%s 协议=%s...\n", entry.Model.ID, entry.Model.API)
	startTime := time.Now()
	msg, err := llm.Complete(ctx, req, stream)
	costTime := time.Since(startTime)

	fmt.Printf("[调用返回] 耗时: %v\n", costTime)
	fmt.Printf("  - 返回 Go error:       %v\n", err)
	fmt.Printf("  - 消息 State:          %s\n", msg.State)
	fmt.Printf("  - 结束原因 StopReason: %s (Raw: %s)\n", msg.StopReason, msg.RawStopReason)
	fmt.Printf("  - 错误信息 ErrorMsg:   %q\n", msg.ErrorMessage)

	// 提取思考与文本块
	var thinkingBlocks []string
	var textBlocks []string
	for _, b := range msg.Content {
		switch b.Type {
		case llm.TypeThinking:
			thinkingBlocks = append(thinkingBlocks, b.Thinking)
		case llm.TypeText:
			textBlocks = append(textBlocks, b.Text)
		}
	}

	thinkingStr := strings.Join(thinkingBlocks, "\n")
	textStr := strings.Join(textBlocks, "\n")

	if len(thinkingStr) > 0 {
		fmt.Printf("\n--- 提取到的思考内容 (共 %d 字节, %d 字符) ---\n",
			len(thinkingStr), utf8.RuneCountInString(thinkingStr))
		preview := thinkingStr
		if len(preview) > 300 {
			preview = preview[:300] + "...(后略)"
		}
		fmt.Println(preview)
		fmt.Println("--------------------------------------------------")
	} else {
		fmt.Println("\n[思考内容]: (空)")
	}

	if len(textStr) > 0 {
		fmt.Printf("\n--- 提取到的完整文本内容 (共 %d 字节, %d 字符) ---\n",
			len(textStr), utf8.RuneCountInString(textStr))
		fmt.Println(textStr)
		fmt.Println("--------------------------------------------------")
	} else {
		fmt.Println("\n[回复文本]: (空)")
	}

	// 构造审计报告
	var items []auditItem

	// 1. Go error 核对
	if cancelMode {
		if err == nil {
			items = append(items, auditItem{
				name:   "返回 error 核对",
				status: "[通过]",
				detail: "error 为 nil (符合设计契约：取消操作由消息 State 承载，非 InternalErr)",
			})
		} else {
			items = append(items, auditItem{
				name:   "返回 error 核对",
				status: "[失败]",
				detail: fmt.Sprintf("期望 error 为 nil，实际为 %v", err),
			})
		}
	} else {
		if err == nil {
			items = append(items, auditItem{
				name:   "返回 error 核对",
				status: "[通过]",
				detail: "error 为 nil，建流及内部流转无状态契约违规",
			})
		} else {
			items = append(items, auditItem{
				name:   "返回 error 核对",
				status: "[失败]",
				detail: fmt.Sprintf("调用发生内部错误: %v", err),
			})
		}
	}

	// 2. 消息状态 State 核对
	if cancelMode {
		if msg.State == llm.StateAborted {
			items = append(items, auditItem{
				name:   "消息 State 核对",
				status: "[通过]",
				detail: fmt.Sprintf("State 为 %q (成功识别为取消终态)", msg.State),
			})
		} else {
			items = append(items, auditItem{
				name:   "消息 State 核对",
				status: "[失败]",
				detail: fmt.Sprintf("期望 State=%q，实际为 %q", llm.StateAborted, msg.State),
			})
		}
	} else {
		if msg.State == llm.StateSuccess {
			items = append(items, auditItem{
				name:   "消息 State 核对",
				status: "[通过]",
				detail: fmt.Sprintf("State 为 %q (模型成功生成并正常结束)", msg.State),
			})
		} else {
			items = append(items, auditItem{
				name:   "消息 State 核对",
				status: "[调用未成功]",
				detail: fmt.Sprintf("State 为 %q，原因: %s", msg.State, msg.ErrorMessage),
			})
		}
	}

	// 3. 结束原因与错误消息
	if cancelMode {
		if msg.StopReason == "" && strings.Contains(msg.ErrorMessage, "取消") {
			items = append(items, auditItem{
				name:   "取消终态属性",
				status: "[通过]",
				detail: fmt.Sprintf("StopReason 已清空, ErrorMessage=%q", msg.ErrorMessage),
			})
		} else {
			items = append(items, auditItem{
				name:   "取消终态属性",
				status: "[失败]",
				detail: fmt.Sprintf("StopReason=%q, ErrorMessage=%q", msg.StopReason, msg.ErrorMessage),
			})
		}
	} else {
		if msg.State == llm.StateSuccess && msg.StopReason != "" {
			items = append(items, auditItem{
				name:   "成功结束原因",
				status: "[通过]",
				detail: fmt.Sprintf("StopReason=%s, RawStopReason=%s", msg.StopReason, msg.RawStopReason),
			})
		}
	}

	// 4. 思考返回核对
	if cancelMode {
		items = append(items, auditItem{
			name:   "思考内容捕获",
			status: "[核对]",
			detail: fmt.Sprintf("中断前累积思考块数量: %d, 字节数: %d", len(thinkingBlocks), len(thinkingStr)),
		})
	} else {
		if len(thinkingStr) > 0 {
			items = append(items, auditItem{
				name:   "思考内容捕获",
				status: "[通过]",
				detail: fmt.Sprintf("正常返回思考块，共 %d 字节 (%d 字符)", len(thinkingStr), utf8.RuneCountInString(thinkingStr)),
			})
		} else {
			items = append(items, auditItem{
				name:   "思考内容捕获",
				status: "[提示]",
				detail: "未返回思考内容 (模型未输出思考或未配置思考)",
			})
		}
	}

	// 5. 文本内容核对
	if cancelMode {
		items = append(items, auditItem{
			name:   "文本内容捕获",
			status: "[核对]",
			detail: fmt.Sprintf("中断前累积文本块数量: %d, 字节数: %d", len(textBlocks), len(textStr)),
		})
	} else {
		if len(textStr) > 0 {
			items = append(items, auditItem{
				name:   "文本内容捕获",
				status: "[通过]",
				detail: fmt.Sprintf("正常返回完整文本，共 %d 字节 (%d 字符)", len(textStr), utf8.RuneCountInString(textStr)),
			})
		} else {
			items = append(items, auditItem{
				name:   "文本内容捕获",
				status: "[失败]",
				detail: "回复文本为空",
			})
		}
	}

	// 6. 用量统计核对
	if msg.Usage.Total > 0 {
		items = append(items, auditItem{
			name:   "Token 用量统计",
			status: "[通过]",
			detail: fmt.Sprintf("Input: %d, Output: %d, Reasoning: %d, Total: %d",
				msg.Usage.Input, msg.Usage.Output, msg.Usage.Reasoning, msg.Usage.Total),
		})
	} else {
		status := "[跳过]"
		if !cancelMode && msg.State == llm.StateSuccess {
			status = "[警告]"
		}
		items = append(items, auditItem{
			name:   "Token 用量统计",
			status: status,
			detail: "服务端未返回用量数据或因中断未到达用量 chunk",
		})
	}

	// 7. 核心契约特别核验项: error 与 State 联合判定
	if cancelMode {
		if err == nil && msg.State != llm.StateSuccess {
			items = append(items, auditItem{
				name:   "★ error与State联合核对",
				status: "[通过 - 契约成立]",
				detail: "验证证明：error 为 nil 但 State=aborted！调用方仅检查 error==nil 会误判成功，必须核验 State！",
			})
		} else {
			items = append(items, auditItem{
				name:   "★ error与State联合核对",
				status: "[异常]",
				detail: fmt.Sprintf("err=%v, State=%s", err, msg.State),
			})
		}
	} else {
		if err == nil && msg.State == llm.StateSuccess {
			items = append(items, auditItem{
				name:   "★ error与State联合核对",
				status: "[通过 - 调用成功]",
				detail: "error 为 nil 且 State 为 success，双重确认模型调用完整成功",
			})
		} else {
			items = append(items, auditItem{
				name:   "★ error与State联合核对",
				status: "[注意]",
				detail: fmt.Sprintf("err=%v, State=%s，调用未处于成功状态", err, msg.State),
			})
		}
	}

	title := fmt.Sprintf("Complete 调用 - %s (%s)", entry.Ref, entry.Model.API)
	if cancelMode {
		title = fmt.Sprintf("Complete 取消试验 - %s (%s)", entry.Ref, entry.Model.API)
	}
	printAuditReport(title, items)
}
