// Package mcp 将明确传入的 MCP 定义适配为 Agent 工具。
// 不启动服务、不向 Runtime 注册、不授权，也不重试调用。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	mcpcore "CodeAgent/internal/mcp"
)

type Caller interface {
	CallTool(context.Context, mcpcore.Definition, json.RawMessage) (*mcpcore.CallToolResult, error)
}

// Rejection 记录明确拒绝适配的定义；其他工具仍可使用。
type Rejection struct {
	Key    mcpcore.ToolKey
	Reason string
}
type Adapted struct {
	Tools    []agent.Tool
	Rejected []Rejection
}

// New 构造静态快照；定义变化或重连后应重建。
// reservedNames 须包含最终 Agent 注册中的所有其他工具名。
func New(caller Caller, definitions []mcpcore.Definition, reservedNames []string) (Adapted, error) {
	if caller == nil {
		return Adapted{}, fmt.Errorf("MCP adapter requires a caller")
	}
	used := map[string]bool{}
	for _, name := range reservedNames {
		used[strings.TrimSpace(name)] = true
	}
	seen := map[mcpcore.ToolKey]bool{}
	result := Adapted{Tools: make([]agent.Tool, 0, len(definitions)), Rejected: make([]Rejection, 0)}
	for _, d := range definitions {
		if seen[d.Key] {
			return Adapted{}, fmt.Errorf("duplicate MCP binding %q/%q", d.Key.ServerID, d.Key.Name)
		}
		seen[d.Key] = true
		if d.Key.ServerID == "" || d.Key.Name == "" || d.Tool == nil || d.Tool.Name != d.Key.Name || d.Generation == 0 || d.Revision == 0 {
			return Adapted{}, fmt.Errorf("invalid MCP definition binding")
		}
		schema, reason := inputSchema(d)
		if reason != "" {
			result.Rejected = append(result.Rejected, Rejection{d.Key, reason})
			continue
		}
		name := toolName(d.Key)
		if used[name] {
			return Adapted{}, fmt.Errorf("MCP model tool name conflicts with another registration: %q", name)
		}
		used[name] = true
		// 仅捕获路由/版本字段，不保留调用方持有的 schema 指针。
		binding := mcpcore.Definition{Key: d.Key, Generation: d.Generation, Revision: d.Revision}
		result.Tools = append(result.Tools, agent.Tool{
			Tool:         llm.Tool{Name: name, Description: d.Tool.Description, Schema: schema},
			ParallelSafe: false,
			Execute:      toolExecutor(caller, binding),
		})
	}
	return result, nil
}

// inputSchema 返回编码副本或拒绝原因，检查 schema 形状和扩展限制。
func inputSchema(d mcpcore.Definition) ([]byte, string) {
	schema, err := json.Marshal(d.Tool.InputSchema)
	if err != nil {
		return nil, "input schema cannot be encoded"
	}
	var shape map[string]any
	if err := json.Unmarshal(schema, &shape); err != nil || shape == nil || shape["type"] != "object" {
		return nil, "input schema must declare type object"
	}
	if d.UnsupportedReason != "" || hasHeaderExtension(shape) {
		return nil, "x-mcp-header tools are unsupported in this connection; use an ordinary tool or reconnect after removing the extension"
	}
	return schema, ""
}

// toolExecutor 绑定路由与版本，返回一次调用并转换结果的执行函数。
func toolExecutor(caller Caller, binding mcpcore.Definition) func(context.Context, string, map[string]any, func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
	return func(ctx context.Context, _ string, args map[string]any, _ func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]any{}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("MCP arguments cannot be encoded: %w", err)
		}
		response, err := caller.CallTool(ctx, binding, raw)
		if err != nil {
			return nil, err
		}
		return convertResult(response)
	}
}

// toolName 稳定且不超过 64 个 ASCII 字符，通过结构化键的摘要保留身份，
// 不解析分隔符；New 仍检查摘要/名称冲突。
func toolName(key mcpcore.ToolKey) string {
	encoded, _ := json.Marshal(key)
	hash := sha256.Sum256(encoded)
	return "mcp_" + slug(key.ServerID, 12) + "_" + slug(key.Name, 20) + "_" + hex.EncodeToString(hash[:12])
}

func slug(value string, limit int) string {
	var b strings.Builder
	for _, c := range value {
		if b.Len() >= limit {
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(byte(c))
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

func hasHeaderExtension(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "x-mcp-header" || hasHeaderExtension(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if hasHeaderExtension(item) {
				return true
			}
		}
	}
	return false
}
