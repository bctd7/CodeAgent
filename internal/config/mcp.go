package config

import (
	"fmt"
	"sort"
	"time"

	"CodeAgent/internal/mcp"
)

// MCPServerConfig 仅为 TOML 数据结构；加载/校验不连接服务或执行命令。
// enabled 默认 false；省略 env 表示继承，env=[] 表示以空环境替换。
type MCPServerConfig struct {
	//是否启用
	Enabled bool `toml:"enabled"`
	//stdio 还是 http
	Transport string `toml:"transport"`
	//本地进程参数 stdio 使用
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Dir     string   `toml:"dir"`
	Env     []string `toml:"env"`
	//http 连接参数
	Endpoint string            `toml:"endpoint"`
	Headers  map[string]string `toml:"headers"`
	// 连接 请求 退出等待
	ConnectTimeoutMS    int64 `toml:"connect_timeout_ms"`
	RequestTimeoutMS    int64 `toml:"request_timeout_ms"`
	TerminateDurationMS int64 `toml:"terminate_duration_ms"`
}

// MCPServers 返回顺序确定、经过校验的连接参数。
// Args、Env、Headers 与配置共享只读引用，调用方和持有配置者均不得修改。
// Manager 构造时另行持有快照；这里的共享只读约定不提供并发安全冻结。
func (c *Config) MCPServers() ([]mcp.ServerConfig, error) {
	ids := make([]string, 0, len(c.MCP))
	for id := range c.MCP {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	specs := make([]mcp.ServerConfig, 0, len(ids))
	for _, id := range ids {
		wire := c.MCP[id]
		for _, milliseconds := range []int64{wire.ConnectTimeoutMS, wire.RequestTimeoutMS, wire.TerminateDurationMS} {
			if milliseconds < 0 || milliseconds > int64(24*time.Hour/time.Millisecond) {
				return nil, fmt.Errorf("MCP server %q: durations must be 0..86400000 ms", id)
			}
		}
		spec := mcp.ServerConfig{
			ID:             id,
			Enabled:        wire.Enabled,
			Transport:      wire.Transport,
			ConnectTimeout: time.Duration(wire.ConnectTimeoutMS) * time.Millisecond,
			RequestTimeout: time.Duration(wire.RequestTimeoutMS) * time.Millisecond,
		}
		switch wire.Transport {
		case "stdio":
			if wire.Endpoint != "" || wire.Headers != nil {
				return nil, fmt.Errorf("MCP server %q: stdio excludes endpoint and headers", id)
			}
			spec.Stdio = &mcp.StdioConfig{
				ID:                id,
				Command:           wire.Command,
				Args:              wire.Args,
				Dir:               wire.Dir,
				Env:               wire.Env,
				TerminateDuration: time.Duration(wire.TerminateDurationMS) * time.Millisecond,
			}
		case "http":
			if wire.Command != "" || wire.Args != nil || wire.Dir != "" || wire.Env != nil || wire.TerminateDurationMS != 0 {
				return nil, fmt.Errorf("MCP server %q: HTTP excludes stdio fields", id)
			}
			spec.HTTP = &mcp.HTTPConfig{
				ID:       id,
				Endpoint: wire.Endpoint,
				Headers:  wire.Headers,
			}
		}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
