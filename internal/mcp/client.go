// Package mcp 提供独立的 MCP 协议客户端，不依赖 Agent、工具适配或 Runtime。
// 本文件保存客户端结构、连接后的操作与请求生命周期。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type InitializeResult = sdk.InitializeResult

// Client 独占一个服务器会话，可并发调用。构造成功后必须显式 Close。
// 请勿复制 Client；每台服务器分别连接，即使工具原名相同也不会混用。
type Client struct {
	id         string
	session    *sdk.ClientSession
	connection sdk.Connection
	changes    chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	closeOnce  sync.Once
	closeErr   error
}

// Wait 观察 SDK 连接终止，包括远端断开。
// Done 仍只表示本地关闭；Wait 本身不释放资源。
func (c *Client) Wait() error {
	return operationError(c.id, "wait", c.session.Wait(), ErrorConnection)
}

// ID 返回调用方指定的服务器实例标识，不是 HTTP 协议的会话 ID。
func (c *Client) ID() string { return c.id }

// InitializeResult 返回协商结果的原生只读视图；调用方不得修改。
func (c *Client) InitializeResult() *InitializeResult { return c.session.InitializeResult() }

// ToolsChanged 合并尚未消费的列表变化通知。收到后再次调用 ListTools。
// 通知依赖服务器能力和传输支持；频道不关闭，退出时应同时监听 Done。
func (c *Client) ToolsChanged() <-chan struct{} { return c.changes }

// Done 在本客户端主动关闭时关闭，不代表服务器异步断连。
func (c *Client) Done() <-chan struct{} { return c.ctx.Done() }

// requestContext 关联单次请求与连接生命周期；完成后必须调用返回的清理函数。
func requestContext(ctx, lifetime context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

// MaxToolPages 和 MaxDiscoveredTools 限制完整发现规模；超过任一上限均不返回部分快照。
const MaxToolPages = 256
const MaxDiscoveredTools = 10000

// ListTools 每次从首页重新请求完整工具列表，保留原名，并拒绝重复的分页游标。
// 分页失败时不返回部分列表，避免上层误当成完整快照。
func (c *Client) ListTools(ctx context.Context) ([]*Tool, error) {
	ctx, cancel := requestContext(ctx, c.ctx)
	defer cancel()
	tools := make([]*Tool, 0)
	cursor := ""
	seen := map[string]bool{"": true}
	for pages := 0; ; pages++ {
		if pages >= MaxToolPages {
			return nil, protocolError(c.id, "tools/list", "MCP discovery page budget exceeded")
		}
		page, err := c.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			if ctx.Err() != nil {
				err = errors.Join(ctx.Err(), err)
			}
			return nil, operationError(c.id, "tools/list", err, ErrorConnection)
		}
		if page == nil {
			return nil, protocolError(c.id, "tools/list", "服务器返回空列表结果")
		}
		if len(page.Tools) > MaxDiscoveredTools-len(tools) {
			return nil, protocolError(c.id, "tools/list", "MCP discovery tool budget exceeded")
		}
		tools = append(tools, page.Tools...)
		cursor = page.NextCursor
		if cursor == "" {
			return tools, nil
		}
		if seen[cursor] {
			return nil, protocolError(c.id, "tools/list", "服务器重复返回分页游标")
		}
		seen[cursor] = true
	}
}

// CallTool 按服务器原名发送一次调用。参数必须是 JSON 对象；空参数视为 {}。
// 不重试、不重连、不重放；取消不保证服务器已经停止副作用。
// 工具执行失败通过结果的 IsError 返回，连接或协议失败才返回 Go error。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*CallToolResult, error) {
	if name == "" {
		return nil, inputError(c.id, "tools/call", "工具原名不能为空")
	}
	arguments = bytes.TrimSpace(arguments)
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if !json.Valid(arguments) || arguments[0] != '{' {
		return nil, inputError(c.id, "tools/call", "工具参数必须是合法 JSON 对象")
	}
	// 独立副本保留大整数的 JSON 表示，不转换成 float64。
	arguments = append(json.RawMessage(nil), arguments...)
	ctx, cancel := requestContext(ctx, c.ctx)
	defer cancel()
	result, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return nil, operationError(c.id, "tools/call", err, ErrorConnection)
	}
	if result == nil {
		return nil, protocolError(c.id, "tools/call", "服务器返回空调用结果")
	}
	return result, nil
}

// Close 幂等地取消本包进行中的请求，再关闭 SDK 会话和自有子进程。
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		// SDK 会话优雅关闭等待在途写入；stdio 管道写满时只有先关闭底层
		// 连接才能解除写入，再等待会话退出，避免相互等待。
		if err := errors.Join(c.connection.Close(), c.session.Close()); err != nil {
			c.closeErr = operationError(c.id, "close", err, ErrorConnection)
		}
	})
	return c.closeErr
}
