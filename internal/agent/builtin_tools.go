package agent

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"CodeAgent/internal/llm"
)

// NewAddTool 创建加法演示工具（并发安全）。
func NewAddTool() Tool {
	return Tool{
		Tool: llm.Tool{
			Name:        "add",
			Description: "计算两个整数的和，用于演示并行工具调度",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"a": {"type": "integer", "description": "第一个整数"},
					"b": {"type": "integer", "description": "第二个整数"}
				},
				"required": ["a", "b"]
			}`),
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			a, err := parseInteger(args["a"])
			if err != nil {
				return nil, fmt.Errorf("参数 a 非法: %w", err)
			}
			b, err := parseInteger(args["b"])
			if err != nil {
				return nil, fmt.Errorf("参数 b 非法: %w", err)
			}

			sum := a + b
			return []llm.ContentBlock{
				{
					Type: llm.TypeText,
					Text: fmt.Sprintf("%d", sum),
				},
			}, nil
		},
	}
}

// NewSleepTool 创建可等待、可报告进度的演示工具（并发安全）。
func NewSleepTool() Tool {
	return Tool{
		Tool: llm.Tool{
			Name:        "sleep",
			Description: "等待指定的毫秒数并返回完成通知，用于演示异步耗时操作与并发调度",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"ms": {"type": "integer", "description": "等待的毫秒数 (默认 100)"}
				}
			}`),
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			durationMS := 100
			if v, ok := args["ms"]; ok {
				if parsed, err := parseInteger(v); err == nil {
					durationMS = parsed
				}
			}
			if durationMS <= 0 {
				durationMS = 100
			}

			if onProgress != nil {
				onProgress(llm.ContentBlock{
					Type: llm.TypeText,
					Text: fmt.Sprintf("开始等待 %dms...", durationMS),
				})
			}

			timer := time.NewTimer(time.Duration(durationMS) * time.Millisecond)
			defer timer.Stop()

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
				return []llm.ContentBlock{
					{
						Type: llm.TypeText,
						Text: fmt.Sprintf("等待 %dms 成功完成", durationMS),
					},
				}, nil
			}
		},
	}
}

// NewEchoTool 创建串行回显演示工具（ParallelSafe=false）。
func NewEchoTool() Tool {
	return Tool{
		Tool: llm.Tool{
			Name:        "echo",
			Description: "返回输入的文本，用于演示串行工具边界调度",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "description": "需要回显的内容"}
				},
				"required": ["message"]
			}`),
		},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			msg, _ := args["message"].(string)
			return []llm.ContentBlock{
				{
					Type: llm.TypeText,
					Text: fmt.Sprintf("echo: %s", msg),
				},
			}, nil
		},
	}
}

func parseInteger(v any) (int, error) {
	switch val := v.(type) {
	case int:
		return val, nil
	case int64:
		return int(val), nil
	case float64:
		if math.Trunc(val) != val {
			return 0, fmt.Errorf("%v 不是整数", val)
		}
		return int(val), nil
	case string:
		return strconv.Atoi(val)
	default:
		return 0, fmt.Errorf("无法将 %v 转换为整数", v)
	}
}
