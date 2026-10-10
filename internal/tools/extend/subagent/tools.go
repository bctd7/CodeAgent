package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

// New 将调用者作用域内的业务能力适配为现有 Agent 工具，不自动注册或启动任务。
func New(controller Controller) ([]agent.Tool, error) {
	if controller == nil {
		return nil, fmt.Errorf("subagent: Controller 不能为空")
	}
	return []agent.Tool{
		newTool("spawn_agent", "Start a child task and return its ID immediately. Use explore for investigation or worker for implementation. Omit model_ref to inherit the current model. Children share the working directory; coordinate concurrent edits.", `{
			"type":"object", "additionalProperties":false,
			"properties":{
				"task":{"type":"string","minLength":1,"description":"Task instructions"},
				"role":{"type":"string","enum":["explore","worker"]},
				"model_ref":{"type":"string","minLength":1,"description":"Optional configured model reference; omission inherits the current model"},
				"file_scope":{"type":"array","items":{"type":"string","minLength":1},"maxItems":256,"uniqueItems":true,"description":"Optional worker file paths, held for the entire child run; omission acquires paths on first modification"}
			}, "required":["task","role"]
		}`, controller.Spawn),
		newTool("send_input", "Send instructions to your child task. Running tasks consume them at safe points; a finished task can continue with its existing context.", `{
			"type":"object", "additionalProperties":false,
			"properties":{"id":{"type":"string","minLength":1},"input":{"type":"string","minLength":1}},
			"required":["id","input"]
		}`, controller.Send),
		newTool("wait", "Wait for any or all specified child tasks to finish, fail or be cancelled. A timeout ends only this wait and leaves tasks running. Returns a snapshot for every requested ID.", fmt.Sprintf(`{
			"type":"object", "additionalProperties":false,
			"properties":{
				"ids":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":64,"uniqueItems":true},
				"mode":{"type":"string","enum":["any","all"]},
				"timeout_ms":{"type":"integer","minimum":1,"maximum":%d}
			}, "required":["ids","mode","timeout_ms"]
		}`, MaxWaitTimeoutMS), controller.Wait),
		newTool("cancel_agent", "Request cancellation and cleanup of the current child task. Does not close its runtime, delete its session or roll back files. The returned snapshot may still be running during cleanup.", `{
			"type":"object", "additionalProperties":false,
			"properties":{"id":{"type":"string","minLength":1}}, "required":["id"]
		}`, controller.Cancel),
	}, nil
}

// newTool 复用参数解码、校验与 JSON 文本结果转换，不实现业务等待或取消循环。
func newTool[Input interface{ validate() error }, Output any](name, description, schema string, invoke func(context.Context, Input) (Output, error)) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{Name: name, Description: description, Schema: []byte(schema)},
		// Wait 可与 Send/Cancel 并行；Controller 必须支持并发调用。
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for key, value := range args {
				if value == nil {
					return nil, fmt.Errorf("参数 %s 不能为 null", key)
				}
			}
			data, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("subagent: 参数编码失败: %w", err)
			}
			var input Input
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil {
				return nil, fmt.Errorf("subagent: 参数格式错误: %w", err)
			}
			if err := input.validate(); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			output, err := invoke(ctx, input)
			if err != nil {
				return nil, err
			}
			data, err = json.Marshal(output)
			if err != nil {
				return nil, fmt.Errorf("subagent: 结果编码失败: %w", err)
			}
			return []llm.ContentBlock{{Type: llm.TypeText, Text: string(data)}}, nil
		},
	}
}
