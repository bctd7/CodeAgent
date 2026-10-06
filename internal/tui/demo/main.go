// 演示只使用内存假事件，不连接模型、数据库、审批服务或真实工具。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"CodeAgent/internal/tui"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan tui.Event, 32)
	stream := make(chan tui.Action, 1)
	done := make(chan struct{})
	// 假流的 goroutine 有明确所有者、context 和退出等待；它只发送事件。
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case action := <-stream:
				for _, piece := range []string{"这是**假数据流**，", "中文和宽字符示例：你好世界 🌍。\n\n", "```go\nfmt.Println(\"demo\")\n```\n", "没有调用真实模型服务。"} {
					timer := time.NewTimer(250 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					select {
					case events <- tui.DeltaEvent{Scope: tui.Scope{SessionID: action.SessionID}, ItemID: "stream-demo", Text: piece}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	status := tui.Status{SessionID: "demo-session", SessionTitle: "假数据演示", State: "演示", Model: "（无真实模型）"}
	cfg := tui.Config{Initial: tui.Snapshot{Status: status, Items: []tui.Item{
		{ID: "intro", Kind: tui.NoticeItem, Text: "本程序仅验证 UI。输入 /stream 演示增量；/approval 演示审批投影。F2/F3 打开假选项，F4 展开假工具。"},
		{ID: "tool-demo", Kind: tui.ToolItem, Tool: &tui.Tool{Name: "示例 read", State: "假结果", Summary: "展示一段已有结果", Parameters: `{"path":"demo.go"}`, Output: "示例内容：你好", Diff: "--- demo.go\n+++ demo.go\n-旧文字\n+新文字（仅示例文本）"}},
	}}, Events: events, Commands: []tui.Command{{Name: "/stream", Description: "假数据增量"}, {Name: "/approval", Description: "假审批投影"}}}
	cfg.Handle = func(ctx context.Context, a tui.Action) ([]tui.Event, error) {
		switch a.Kind {
		case tui.Quit:
			return []tui.Event{tui.ExitEvent{}}, nil
		case tui.CancelRun:
			return []tui.Event{tui.NoticeEvent{Text: "演示没有实际 Run 可取消"}}, nil
		case tui.Send:
			return []tui.Event{tui.ItemEvent{Item: tui.Item{ID: fmt.Sprintf("user-%d", time.Now().UnixNano()), Kind: tui.UserItem, Text: a.Text}}, tui.NoticeEvent{Text: "演示已接收消息，未启动实际 Run"}}, nil
		case tui.Steer:
			return []tui.Event{tui.NoticeEvent{Text: "仅收到 Steer 意图"}}, nil
		case tui.ExecuteCommand:
			switch strings.TrimSpace(a.Text) {
			case "/stream":
				select {
				case stream <- a:
					return []tui.Event{tui.ItemEvent{Item: tui.Item{ID: "stream-demo", Kind: tui.AssistantItem}}}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			case "/approval":
				return []tui.Event{tui.ApprovalEvent{Approval: tui.Approval{ID: "demo-approval", Source: "假数据注入；非实际审批", Tool: "示例 write", Parameters: `{"path":"demo.txt","text":"你好"}`}}}, nil
			default:
				return nil, fmt.Errorf("演示只提供 /stream 和 /approval")
			}
		case tui.ListSessions:
			return []tui.Event{tui.ChoicesEvent{Kind: a.Kind, Choices: []tui.Choice{{ID: "demo-session", Label: "假会话", Detail: "不读取数据库"}}}}, nil
		case tui.ListModels:
			return []tui.Event{tui.ChoicesEvent{Kind: a.Kind, Choices: []tui.Choice{{ID: "demo-model", Label: "假模型选项", Detail: "不连接服务"}}}}, nil
		case tui.SelectSession, tui.SelectModel:
			return []tui.Event{tui.NoticeEvent{Text: "收到选择：" + a.ChoiceID + "（仅演示）"}}, nil
		case tui.AnswerApproval:
			return []tui.Event{tui.NoticeEvent{Text: "收到假审批选择：" + string(a.Decision) + "；未改变权限或执行工具"}}, nil
		}
		return nil, nil
	}
	err := tui.Run(ctx, cfg, tui.IO{})
	cancel()
	<-done
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
