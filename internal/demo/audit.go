package demo

import (
	"fmt"
	"strings"

	"CodeAgent/internal/llm"
)

// auditRecord 暂存单次流式调用的观察数据与核对结果。
type auditRecord struct {
	turnIndex          int
	eventsCount        int
	terminalEventCount int
	lastTerminalEvent  *llm.Event
	textDeltas         map[int]*strings.Builder
	thinkingDeltas     map[int]*strings.Builder
	toolDeltas         map[int]*strings.Builder
	result             llm.Result
}

func newAuditRecord(turnIndex int) *auditRecord {
	return &auditRecord{
		turnIndex:      turnIndex,
		textDeltas:     make(map[int]*strings.Builder),
		thinkingDeltas: make(map[int]*strings.Builder),
		toolDeltas:     make(map[int]*strings.Builder),
	}
}

func (a *auditRecord) recordEvent(event llm.Event) {
	a.eventsCount++
	switch event.Type {
	case llm.EventTextDelta:
		sb, ok := a.textDeltas[event.BlockIndex]
		if !ok {
			sb = &strings.Builder{}
			a.textDeltas[event.BlockIndex] = sb
		}
		sb.WriteString(event.Delta)

	case llm.EventThinkingDelta:
		sb, ok := a.thinkingDeltas[event.BlockIndex]
		if !ok {
			sb = &strings.Builder{}
			a.thinkingDeltas[event.BlockIndex] = sb
		}
		sb.WriteString(event.Delta)

	case llm.EventToolCallDelta:
		sb, ok := a.toolDeltas[event.BlockIndex]
		if !ok {
			sb = &strings.Builder{}
			a.toolDeltas[event.BlockIndex] = sb
		}
		sb.WriteString(event.Delta)

	case llm.EventMessageComplete, llm.EventMessageError:
		a.terminalEventCount++
		a.lastTerminalEvent = &event
	}
}

type auditItem struct {
	name   string
	status string // "[通过]", "[失败]", "[跳过]", "[未完成]"
	detail string
}

func (a *auditRecord) verify(expectedToolIDs []string, matchedToolIDs []string) []auditItem {
	var items []auditItem
	msg := a.result.Message

	// 1. 终态唯一性核对
	if a.terminalEventCount == 1 {
		items = append(items, auditItem{
			name:   "终态唯一性",
			status: "[通过]",
			detail: fmt.Sprintf("收到且仅收到 1 个终态事件 (State: %s, StopReason: %q)", msg.State, msg.StopReason),
		})
	} else {
		items = append(items, auditItem{
			name:   "终态唯一性",
			status: "[失败]",
			detail: fmt.Sprintf("终态事件数量异常: %d (必须严格为 1)", a.terminalEventCount),
		})
	}

	// 2. 内部不变量核对 (InternalErr 只能记录不变量错误，普通失败为 nil)
	if a.result.InternalErr != nil {
		items = append(items, auditItem{
			name:   "内部不变量",
			status: "[失败]",
			detail: fmt.Sprintf("存在内部状态契约违背: %v", a.result.InternalErr),
		})
	} else {
		items = append(items, auditItem{
			name:   "内部不变量",
			status: "[通过]",
			detail: "InternalErr 为 nil，未发生契约违背",
		})
	}

	// 3. 终态合法性
	if !msg.State.IsTerminal() {
		items = append(items, auditItem{
			name:   "终态合法性",
			status: "[失败]",
			detail: fmt.Sprintf("最终消息 State 未处于已定义终态: %q", msg.State),
		})
	} else {
		items = append(items, auditItem{
			name:   "终态合法性",
			status: "[通过]",
			detail: fmt.Sprintf("最终状态为合法终态 %q", msg.State),
		})
	}

	// 4. 取消状态与保留内容核对
	if msg.State == llm.StateAborted {
		hasContent := len(msg.Content) > 0
		var totalChars int
		for _, b := range msg.Content {
			totalChars += len(b.Text) + len(b.Thinking)
		}
		if hasContent {
			items = append(items, auditItem{
				name:   "中断保留内容",
				status: "[通过]",
				detail: fmt.Sprintf("成功保留已生成的 %d 个内容块 (共 %d 字符)", len(msg.Content), totalChars),
			})
		} else {
			items = append(items, auditItem{
				name:   "中断保留内容",
				status: "[跳过]",
				detail: "首个内容块返回前已被中断，未产生待保留内容",
			})
		}
		items = append(items, auditItem{
			name:   "正常结束检查",
			status: "[未完成]",
			detail: "本次调用已被主动取消 (StateAborted)",
		})
	}

	// 5. 文本增量一致性核对
	var hasText bool
	var textMismatch bool
	var textTotalLen int
	for idx, block := range msg.Content {
		if block.Type == llm.TypeText {
			hasText = true
			textTotalLen += len(block.Text)
			sb := a.textDeltas[idx]
			var deltaStr string
			if sb != nil {
				deltaStr = sb.String()
			}
			if deltaStr != block.Text {
				textMismatch = true
			}
		}
	}
	if hasText {
		if !textMismatch {
			items = append(items, auditItem{
				name:   "文本增量一致性",
				status: "[通过]",
				detail: fmt.Sprintf("流式 Delta 累积内容与最终消息完全一致 (共 %d 字符)", textTotalLen),
			})
		} else {
			items = append(items, auditItem{
				name:   "文本增量一致性",
				status: "[失败]",
				detail: "流式 Delta 累积内容与最终消息快照不一致",
			})
		}
	} else {
		items = append(items, auditItem{
			name:   "文本增量一致性",
			status: "[跳过]",
			detail: "未收到文本内容块",
		})
	}

	// 6. 思考增量与签名一致性核对
	var hasThinking bool
	var thinkingMismatch bool
	var thinkingTotalLen int
	var hasThinkingSig bool
	for idx, block := range msg.Content {
		if block.Type == llm.TypeThinking {
			hasThinking = true
			thinkingTotalLen += len(block.Thinking)
			if block.ThinkingSignature != "" {
				hasThinkingSig = true
			}
			sb := a.thinkingDeltas[idx]
			var deltaStr string
			if sb != nil {
				deltaStr = sb.String()
			}
			if deltaStr != block.Thinking {
				thinkingMismatch = true
			}
		}
	}
	if hasThinking {
		if !thinkingMismatch {
			sigInfo := "无签名"
			if hasThinkingSig {
				sigInfo = "包含签名"
			}
			items = append(items, auditItem{
				name:   "思考增量一致性",
				status: "[通过]",
				detail: fmt.Sprintf("思考流式增量与最终消息完全一致 (共 %d 字符, %s)", thinkingTotalLen, sigInfo),
			})
		} else {
			items = append(items, auditItem{
				name:   "思考增量一致性",
				status: "[失败]",
				detail: "思考流式增量与最终消息快照不一致",
			})
		}
	} else {
		items = append(items, auditItem{
			name:   "思考增量一致性",
			status: "[跳过]",
			detail: "未输出思考内容块",
		})
	}

	// 7. 工具调用块与参数解析核对
	var hasToolCall bool
	var toolCallFailures []string
	for _, block := range msg.Content {
		if block.Type == llm.TypeToolCall {
			hasToolCall = true
			if block.ID == "" {
				toolCallFailures = append(toolCallFailures, "缺少 tool_call id")
			}
			if block.Name == "" {
				toolCallFailures = append(toolCallFailures, "缺少 tool_call name")
			}
			if block.Arguments == nil {
				toolCallFailures = append(toolCallFailures, fmt.Sprintf("工具 %s 参数为 nil", block.Name))
			}
		}
	}
	if hasToolCall {
		if len(toolCallFailures) == 0 {
			items = append(items, auditItem{
				name:   "工具调用结构核对",
				status: "[通过]",
				detail: "工具调用的 ID、名称及 Arguments JSON 对象均解析完整",
			})
		} else {
			items = append(items, auditItem{
				name:   "工具调用结构核对",
				status: "[失败]",
				detail: strings.Join(toolCallFailures, "; "),
			})
		}
	} else {
		items = append(items, auditItem{
			name:   "工具调用结构核对",
			status: "[跳过]",
			detail: "未产生工具调用",
		})
	}

	// 8. 工具调用 ID 配对核对 (在多轮工具往返中)
	if len(expectedToolIDs) > 0 || len(matchedToolIDs) > 0 {
		var mismatch bool
		if len(expectedToolIDs) != len(matchedToolIDs) {
			mismatch = true
		} else {
			for i := range expectedToolIDs {
				if expectedToolIDs[i] != matchedToolIDs[i] {
					mismatch = true
					break
				}
			}
		}
		if !mismatch {
			items = append(items, auditItem{
				name:   "工具调用 ID 配对",
				status: "[通过]",
				detail: fmt.Sprintf("工具结果 ToolCallID 与模型发出 ID 严格匹配 (%d 次调用)", len(expectedToolIDs)),
			})
		} else {
			items = append(items, auditItem{
				name:   "工具调用 ID 配对",
				status: "[失败]",
				detail: fmt.Sprintf("期望 ID %v 与回传 ID %v 不匹配", expectedToolIDs, matchedToolIDs),
			})
		}
	}

	// 9. 用量与费用核对
	if msg.Usage.Total > 0 {
		costStr := ""
		if msg.Usage.Cost.Total > 0 {
			costStr = fmt.Sprintf(", 预估费用: $%.6f", msg.Usage.Cost.Total)
		}
		items = append(items, auditItem{
			name:   "Token 用量统计",
			status: "[通过]",
			detail: fmt.Sprintf("输入: %d, 输出: %d, 思考: %d, 总计: %d%s",
				msg.Usage.Input, msg.Usage.Output, msg.Usage.Reasoning, msg.Usage.Total, costStr),
		})
	} else {
		items = append(items, auditItem{
			name:   "Token 用量统计",
			status: "[跳过]",
			detail: "服务端未返回用量数据 (或调用提前终止)",
		})
	}

	return items
}

func printAuditReport(title string, items []auditItem) {
	fmt.Println("\n==================================================")
	fmt.Printf("[验收核对报告] %s\n", title)
	fmt.Println("--------------------------------------------------")
	for _, item := range items {
		fmt.Printf("  %-18s %s %s\n", item.name+":", item.status, item.detail)
	}
	fmt.Println("==================================================")
}
