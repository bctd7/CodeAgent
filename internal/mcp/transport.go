package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ownedTransport 记录 SDK 连接供失败时回收，并保护 stdio 初始化期间的堵塞 I/O。
type ownedTransport struct {
	sdk.Transport
	connection sdk.Connection
	stop       func() bool
}

func (t *ownedTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if _, stdio := t.Transport.(*sdk.CommandTransport); conn != nil && stdio {
		conn = &cancelWriteConnection{Connection: conn}
		// HTTP 由 lifetimeTransport 取消初始化请求；stdio 必须关闭管道解除堵塞。
		t.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	}
	t.connection = conn
	return conn, err
}

// stdio 写入管道的系统调用不接受 context。写入期间取消只能关闭整条连接，
// 防止调用永远阻塞；不能重用可能只发送了半条消息的协议流。
type cancelWriteConnection struct{ sdk.Connection }

func (c *cancelWriteConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Connection.Close() })
	defer stop()
	return c.Connection.Write(ctx, msg)
}

// ConnectStdio 启动自有直接子进程并完成初始化。ctx 仅控制建立连接，
// 建立成功后进程生命周期由 Close 控制，不随连接 ctx 到期而终止。
func ConnectStdio(ctx context.Context, cfg StdioConfig) (*Client, error) {
	if cfg.Command == "" {
		return nil, wrapError(cfg.ID, "connect", ErrorInput, errors.New("stdio 命令不能为空"))
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
		return nil, wrapError(cfg.ID, "connect", ErrorInput, err)
	}
	if err := validateHeaders(cfg.Headers); err != nil {
		return nil, wrapError(cfg.ID, "connect", ErrorInput, err)
	}
	client := http.DefaultClient
	if cfg.HTTPClient != nil {
		client = cfg.HTTPClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// SDK 的初始化 GET 和 subscriptions/listen 使用独立 context。
	// 初始化期间关联调用方取消，成功后停止关联；显式关闭仍取消所有 HTTP 请求。
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	base := copyClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if len(cfg.Headers) != 0 {
		base = &headerTransport{base: base, headers: cloneHeaders(cfg.Headers)}
	}
	copyClient.Transport = &lifetimeTransport{base: base, ctx: lifetime}
	c, err := connect(ctx, cfg.ID, cfg.Identity, &sdk.StreamableClientTransport{
		Endpoint: cfg.Endpoint, HTTPClient: &copyClient,
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

// lifetimeTransport 为 SDK 脱离调用方 context 的 HTTP 请求补上连接生命周期。
type lifetimeTransport struct {
	base http.RoundTripper
	ctx  context.Context
}

func (t *lifetimeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// SDK 自己为终止会话的 DELETE 设置短超时，不能被提前取消。
	if req.Method == http.MethodDelete {
		return t.base.RoundTrip(req)
	}
	ctx, cleanup := requestContext(req.Context(), t.ctx)
	response, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		cleanup()
		return response, err
	}
	response.Body = &lifetimeBody{ReadCloser: response.Body, cleanup: cleanup}
	return response, nil
}

// 响应体可能是长连接 SSE，只能在关闭响应体后解除 context 关联。
type lifetimeBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *lifetimeBody) Close() error {
	defer b.cleanup()
	return b.ReadCloser.Close()
}

// 静态请求头按请求复制；此包装不重试请求。
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copyReq := req.Clone(req.Context())
	for k, v := range t.headers {
		copyReq.Header.Set(k, v)
	}
	return t.base.RoundTrip(copyReq)
}
