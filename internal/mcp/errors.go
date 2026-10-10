package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// ErrorKind 区分输入、取消、协议、连接和本地状态错误，不能据此推断副作用。
type ErrorKind string

const (
	ErrorInput      ErrorKind = "input"
	ErrorCanceled   ErrorKind = "canceled"
	ErrorProtocol   ErrorKind = "protocol"
	ErrorConnection ErrorKind = "connection"
	ErrorState      ErrorKind = "state"
)

var (
	ErrManagerClosed     = errors.New("MCP manager closed")
	ErrManagerStarted    = errors.New("MCP manager already started; use explicit Reconnect")
	ErrServerUnavailable = errors.New("MCP server unavailable")
	ErrStaleDefinition   = errors.New("MCP tool binding is stale; rebuild definitions before calling")
	ErrUnsupportedTool   = errors.New("MCP x-mcp-header extension is unsupported in this connection generation")
	ErrToolNotFound      = errors.New("MCP 目录中不存在指定工具")
)

// Error 默认只打印安全摘要；Unwrap 保留原原因供 errors.Is/As 和脱敏诊断。
type Error struct {
	ServerID string
	Op       string
	Kind     ErrorKind
	Err      error
}

func (e *Error) Error() string {
	id := e.ServerID
	if id != "" && !validServerID(id) {
		id = "<invalid>"
	}
	op := e.Op
	switch op {
	case "validate", "connect", "close previous connection", "close", "wait", "discover", "refresh", "tools/list", "tools/call", "catalog/update", "catalog/get", "catalog/search":
	default:
		op = "operation"
	}
	return fmt.Sprintf("mcp %q %s: %s", id, op, safeError(e))
}

func (e *Error) Unwrap() error { return e.Err }

// inputError 将动态校验原因留在错误链中，默认展示不包含这些细节。
func inputError(id, op, format string, args ...any) error {
	return &Error{ServerID: id, Op: op, Kind: ErrorInput, Err: fmt.Errorf(format, args...)}
}

// protocolError 用于 SDK 未返回类型错误时，本包检测到的非法协议结果。
func protocolError(id, op, format string, args ...any) error {
	return &Error{ServerID: id, Op: op, Kind: ErrorProtocol, Err: fmt.Errorf(format, args...)}
}

// operationError 补操作上下文，已有同一上下文不再包装；未知本地错误归 state。
// SDK 调用边界显式提供 connection 回退，类型和固定原因优先于回退分类。
func operationError(id, op string, err error, fallback ...ErrorKind) error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok && e.ServerID == id && e.Op == op {
		return err
	}
	kind := ErrorState
	if len(fallback) > 0 {
		kind = fallback[0]
	}
	var rpcErr *jsonrpc.Error
	var previous *Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = ErrorCanceled
	case errors.As(err, &rpcErr):
		kind = ErrorProtocol
	case errors.Is(err, ErrManagerClosed), errors.Is(err, ErrManagerStarted), errors.Is(err, ErrServerUnavailable), errors.Is(err, ErrStaleDefinition), errors.Is(err, ErrUnsupportedTool), errors.Is(err, ErrToolNotFound):
		kind = ErrorState
	case errors.As(err, &previous):
		kind = previous.Kind
	}
	return &Error{ServerID: id, Op: op, Kind: kind, Err: err}
}

func safeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrManagerClosed):
		return "manager closed"
	case errors.Is(err, ErrManagerStarted):
		return "manager already started"
	case errors.Is(err, ErrStaleDefinition):
		return "stale definition"
	case errors.Is(err, ErrUnsupportedTool):
		return "unsupported tool extension"
	case errors.Is(err, ErrServerUnavailable):
		return "server unavailable"
	case errors.Is(err, ErrToolNotFound):
		return "tool not found"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var e *Error
	if errors.As(err, &e) {
		switch e.Kind {
		case ErrorInput:
			return "input failure"
		case ErrorProtocol:
			return "protocol failure"
		case ErrorConnection:
			return "connection failure"
		case ErrorState:
			return "state failure"
		case ErrorCanceled:
			return "canceled"
		}
	}
	return "operation failed"
}
