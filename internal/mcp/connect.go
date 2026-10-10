// 连接构造与 MCP 初始化。
package mcp

import (
	"context"
	"errors"
	"os/exec"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConnectStdio 启动自有直接子进程并完成初始化。ctx 仅控制建立连接，
// 建立成功后进程生命周期由 Close 控制，不随连接 ctx 到期而终止。
func ConnectStdio(ctx context.Context, cfg StdioConfig) (*Client, error) {
	if cfg.Command == "" {
		return nil, inputError(cfg.ID, "connect", "stdio 命令不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, operationError(cfg.ID, "connect", err)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env
	cmd.Stderr = cfg.Stderr
	// 限制后代继承 stderr 管道造成的退出等待；无法解除阻塞的自定义 writer。
	cmd.WaitDelay = 5 * time.Second
	return connect(ctx, cfg.ID, cfg.Identity, &sdk.CommandTransport{Command: cmd, TerminateDuration: cfg.TerminateDuration})
}

// ConnectHTTP 完成协议初始化。禁止自动跟随重定向，以免重放 tools/call。
// 对 HTTPClient 做浅复制，不修改或关闭调用方共享的 transport。
func ConnectHTTP(ctx context.Context, cfg HTTPConfig) (*Client, error) {
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return nil, operationError(cfg.ID, "connect", err)
	}
	if err := validateHeaders(cfg.Headers); err != nil {
		return nil, operationError(cfg.ID, "connect", err)
	}
	// SDK 的初始化 GET 和 subscriptions/listen 使用独立 context。
	// 初始化期间关联调用方取消，成功后停止关联；显式关闭仍取消所有 HTTP 请求。
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	c, err := connect(ctx, cfg.ID, cfg.Identity, &sdk.StreamableClientTransport{
		Endpoint: cfg.Endpoint, HTTPClient: newHTTPClient(cfg, lifetime),
		// 关闭 SDK 的 SSE 重新连接；工具调用不会重新发送。
		MaxRetries: -1,
	})
	stop()
	if err == nil && ctx.Err() != nil {
		_ = c.Close()
		err = operationError(cfg.ID, "connect", ctx.Err())
	}
	if err != nil {
		cancel()
		return nil, err
	}
	previousCancel := c.cancel
	c.cancel = func() { previousCancel(); cancel() }
	return c, nil
}

func connect(ctx context.Context, id string, identity Identity, transport sdk.Transport) (*Client, error) {
	if id == "" {
		return nil, inputError(id, "connect", "服务器实例 ID 不能为空")
	}
	if identity.Name == "" {
		identity.Name = "CodeAgent"
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &Client{id: id, changes: make(chan struct{}, 1), ctx: lifetime, cancel: cancel}
	client := sdk.NewClient(&sdk.Implementation{Name: identity.Name, Version: identity.Version}, &sdk.ClientOptions{
		// 不声明本包范围外的 roots、sampling 或 elicitation 能力。
		Capabilities: &sdk.ClientCapabilities{},
		// SDK 默认会为 input_required 结果自动重放 tools/call；本包保留结果，禁止重放。
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
			select {
			case c.changes <- struct{}{}:
			default:
			}
		},
	})
	// 新协议允许 SDK 缓存工具列表，但 SDK 没有公开的强制失效接口。
	// 列表 TTL 设为零，让 ListTools 每次都请求服务器，不改变工具定义。
	client.AddSendingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			result, err := next(ctx, method, req)
			if list, ok := result.(*sdk.ListToolsResult); ok && list != nil {
				list.TTLMs = 0
			}
			return result, err
		}
	})
	owned := &ownedTransport{Transport: transport}
	session, err := client.Connect(ctx, owned, nil)
	if owned.stop != nil {
		owned.stop()
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		cancel()
		// SDK 的 discover 成功但订阅失败分支未关闭会话，兜底回收底层连接。
		// 官方两种连接的 Close 都幂等，可与 SDK 已完成的清理兼容。
		if owned.connection != nil {
			err = errors.Join(err, owned.connection.Close())
		}
		if session != nil {
			err = errors.Join(err, session.Close())
		}
		return nil, operationError(id, "connect", err, ErrorConnection)
	}
	c.session = session
	c.connection = owned.connection
	return c, nil
}
