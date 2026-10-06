// Package config 读取 TOML 配置，并协调各类配置的校验与装配。
package config

import (
	"fmt"
	"strings"

	"CodeAgent/internal/instructions"
	"github.com/BurntSushi/toml"
)

// Config 保存 TOML 中的模型、MCP 连接和指令加载配置；读取不建立连接。
type Config struct {
	Instructions InstructionsConfig         `toml:"instructions"`
	Providers    map[string]ProviderConfig  `toml:"providers"`
	MCP          map[string]MCPServerConfig `toml:"mcp"`
}

// InstructionsConfig 指定各级目录中查找的单个指令文件名。
type InstructionsConfig struct {
	FileName string `toml:"file_name"` // 空值使用 instructions.DefaultFileName。
}

// Load 从指定路径加载 TOML 配置文件并执行校验。
// 若路径为空、格式非法或校验未通过，直接返回具体错误，不静默回退。
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("配置文件路径不能为空")
	}

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("解析 TOML 配置文件失败 %s: %w", path, err)
	}

	if cfg.Instructions.FileName == "" {
		cfg.Instructions.FileName = instructions.DefaultFileName
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}

	return &cfg, nil
}

// Validate 检查指令文件名、模型必要字段和 MCP 配置，不发起连接或修改配置。
func (c *Config) Validate() error {
	if err := instructions.ValidateFileName(c.Instructions.FileName); err != nil {
		return fmt.Errorf("instructions 配置无效: %w", err)
	}

	if err := c.validateModels(); err != nil {
		return err
	}
	if _, err := c.MCPServers(); err != nil {
		return err
	}
	return nil
}
