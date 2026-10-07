package mcp

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultRequestTimeout = 30 * time.Second
)

// ServerConfig 描述明确的连接参数，不负责加载配置。
// 禁用配置同样校验；Env 保留 StdioConfig 的 nil 继承/非 nil 替换约定。
type ServerConfig struct {
	ID             string
	Enabled        bool
	Transport      string
	Stdio          *StdioConfig
	HTTP           *HTTPConfig
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
}

// Identity 是初始化时发送的客户端身份，与服务器实例 ID 分开。
type Identity struct {
	Name    string
	Version string
}

// StdioConfig 只启动明确配置的命令，不经过 shell、不安装服务器。
type StdioConfig struct {
	ID       string
	Identity Identity
	Command  string
	Args     []string
	Dir      string
	// Env 为 nil 时继承环境；非 nil 时完整替换，调用方负责所需 PATH 等变量。
	Env []string
	// Stderr 必须支持并发写入且不阻塞；nil 时丢弃，不污染协议 stdout。
	Stderr io.Writer
	// TerminateDuration 为关闭 stdin 后的退出宽限期；零值使用 SDK 默认值。
	TerminateDuration time.Duration
}

// HTTPConfig 指定 Streamable HTTP 地址；可注入带静态认证头的 HTTPClient。
// 注入的 RoundTripper 不得自动重试 POST。此包不执行 OAuth 登录流程。
type HTTPConfig struct {
	ID         string
	Identity   Identity
	Endpoint   string
	HTTPClient *http.Client
	// Headers 是复制的静态请求头，不允许覆盖协议管理的请求头。
	Headers map[string]string
}

func (s ServerConfig) Validate() error {
	if !validServerID(s.ID) {
		return errors.New("MCP server ID must use 1..128 ASCII letters, digits, '.', '_' or '-'")
	}
	for _, d := range []time.Duration{s.ConnectTimeout, s.RequestTimeout} {
		if d < 0 || d > 24*time.Hour {
			return fmt.Errorf("MCP server %q: timeout must be 0..24h (zero uses default)", s.ID)
		}
	}
	switch s.Transport {
	case "stdio":
		return s.validateStdio()
	case "http":
		return s.validateHTTP()
	default:
		return fmt.Errorf("MCP server %q: transport must be stdio or http", s.ID)
	}
}

func (s ServerConfig) validateStdio() error {
	if s.Stdio == nil || s.HTTP != nil || strings.TrimSpace(s.Stdio.Command) == "" {
		return fmt.Errorf("MCP server %q: stdio requires command and excludes HTTP fields", s.ID)
	}
	if s.Stdio.ID != "" && s.Stdio.ID != s.ID {
		return fmt.Errorf("MCP server %q: conflicting stdio ID", s.ID)
	}
	if s.Stdio.TerminateDuration < 0 || s.Stdio.TerminateDuration > 24*time.Hour {
		return fmt.Errorf("MCP server %q: invalid terminate duration", s.ID)
	}
	if strings.ContainsRune(s.Stdio.Command, 0) || strings.ContainsRune(s.Stdio.Dir, 0) {
		return fmt.Errorf("MCP server %q: NUL in command or directory", s.ID)
	}
	for _, a := range s.Stdio.Args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("MCP server %q: NUL in argument", s.ID)
		}
	}
	seen := map[string]bool{}
	for _, value := range s.Stdio.Env {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.ContainsRune(value, 0) {
			return fmt.Errorf("MCP server %q: invalid environment entry", s.ID)
		}
		// Windows 环境变量名不区分大小写；所有平台均拒绝歧义项。
		name = strings.ToUpper(name)
		if seen[name] {
			return fmt.Errorf("MCP server %q: duplicate environment name", s.ID)
		}
		seen[name] = true
	}
	return nil
}

func (s ServerConfig) validateHTTP() error {
	if s.HTTP == nil || s.Stdio != nil {
		return fmt.Errorf("MCP server %q: HTTP requires endpoint and excludes stdio fields", s.ID)
	}
	if s.HTTP.ID != "" && s.HTTP.ID != s.ID {
		return fmt.Errorf("MCP server %q: conflicting HTTP ID", s.ID)
	}
	if err := validateEndpoint(s.HTTP.Endpoint); err != nil {
		return fmt.Errorf("MCP server %q: %w", s.ID, err)
	}
	if err := validateHeaders(s.HTTP.Headers); err != nil {
		return fmt.Errorf("MCP server %q: %w", s.ID, err)
	}
	return nil
}

func validServerID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("MCP endpoint must be HTTP(S), without user information or fragment")
	}
	return nil
}

func validateHeaders(headers map[string]string) error {
	seen := map[string]bool{}
	for name, value := range headers {
		lower := strings.ToLower(name)
		if name == "" || seen[lower] {
			return errors.New("MCP headers contain empty or duplicate name")
		}
		seen[lower] = true
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return errors.New("MCP header name is invalid")
			}
		}
		for _, c := range value {
			if c == '\r' || c == '\n' || c < 32 && c != '\t' || c == 127 {
				return errors.New("MCP header value contains control characters")
			}
		}
		if strings.HasPrefix(lower, "mcp-") || lower == "host" || lower == "content-length" || lower == "content-type" || lower == "accept" || lower == "connection" || lower == "transfer-encoding" || lower == "last-event-id" {
			return errors.New("MCP static header overrides a transport-owned header")
		}
	}
	return nil
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
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

// prepare 为管理器建立配置快照，补齐默认超时和传输实例 ID。
// 快照内部只读；Stderr、Jar 和底层 RoundTripper 仍由调用方共享。
func (s ServerConfig) prepare() ServerConfig {
	if s.ConnectTimeout == 0 {
		s.ConnectTimeout = DefaultConnectTimeout
	}
	if s.RequestTimeout == 0 {
		s.RequestTimeout = DefaultRequestTimeout
	}
	if s.Stdio != nil {
		c := *s.Stdio
		c.ID = s.ID
		c.Args = cloneStrings(c.Args)
		c.Env = cloneStrings(c.Env)
		s.Stdio = &c
	}
	if s.HTTP != nil {
		c := *s.HTTP
		c.ID = s.ID
		c.Headers = cloneHeaders(c.Headers)
		if c.HTTPClient != nil {
			// 隔离调用方字段变更；连接时另建副本安装传输包装。
			h := *c.HTTPClient
			c.HTTPClient = &h
		}
		s.HTTP = &c
	}
	return s
}
