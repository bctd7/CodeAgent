package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// ErrorKind 区分输入、取消、远端协议错误和连接失败。
type ErrorKind string

const (
	ErrorInput      ErrorKind = "input"
	ErrorCanceled   ErrorKind = "canceled"
	ErrorProtocol   ErrorKind = "protocol"
	ErrorConnection ErrorKind = "connection"
)

// Error 附带实例和操作；Unwrap 保留 context 和 SDK 原始错误供 errors.Is/As 检查。
// ErrorProtocol 包含可识别的 JSON-RPC 错误；SDK 无公开分类的错误归连接类，
// 因此不能用 Kind 推断调用是否已经产生副作用。
type Error struct {
	ServerID string
	Op       string
	Kind     ErrorKind
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("mcp %q %s (%s): %v", e.ServerID, e.Op, e.Kind, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

func wrapError(id, op string, kind ErrorKind, err error) error {
	return &Error{ServerID: id, Op: op, Kind: kind, Err: err}
}

func operationError(id, op string, err error) error {
	kind := ErrorConnection
	var rpcErr *jsonrpc.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = ErrorCanceled
	case errors.As(err, &rpcErr):
		kind = ErrorProtocol
	}
	return wrapError(id, op, kind, err)
}

var (
	ErrManagerClosed     = errors.New("MCP manager closed")
	ErrServerUnavailable = errors.New("MCP server unavailable")
	ErrStaleDefinition   = errors.New("MCP tool binding is stale; rebuild definitions before calling")
	ErrUnsupportedTool   = errors.New("MCP x-mcp-header extension is unsupported in this connection generation")
)

// ServerError 提供安全打印摘要，并通过 Unwrap 保留底层错误。
// 调用方须先脱敏凭据，才能打印解包后的原始错误。
type ServerError struct {
	ServerID, Op string
	Err          error
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("mcp %q %s: %s", e.ServerID, e.Op, safeError(e.Err))
}

func (e *ServerError) Unwrap() error { return e.Err }

func safeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrManagerClosed):
		return "manager closed"
	case errors.Is(err, ErrStaleDefinition):
		return "stale definition"
	case errors.Is(err, ErrUnsupportedTool):
		return "unsupported tool extension"
	case errors.Is(err, ErrServerUnavailable):
		return "server unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var e *Error
	if errors.As(err, &e) {
		return string(e.Kind) + " failure"
	}
	return "operation failed"
}
