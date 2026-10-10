// 底层传输适配与连接回收。
package mcp

import (
	"context"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ownedTransport 记录 SDK 连接供失败时回收，并保护 stdio 初始化期间的堵塞 I/O。
type ownedTransport struct {
	//建立传输的对象
	sdk.Transport
	// 传输的本身，用于初始化失败以后还能释放这个连接
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

// newHTTPClient 浅复制客户端后改写 CheckRedirect 和 Transport，底层传输仍共享。
func newHTTPClient(cfg HTTPConfig, lifetime context.Context) *http.Client {
	client := http.DefaultClient
	if cfg.HTTPClient != nil {
		client = cfg.HTTPClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := copyClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if len(cfg.Headers) != 0 {
		// 保留连接层的请求头副本。
		base = &headerTransport{base: base, headers: cloneHeaders(cfg.Headers)}
	}
	copyClient.Transport = &lifetimeTransport{base: base, ctx: lifetime}
	return &copyClient
}

func cloneHeaders(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
