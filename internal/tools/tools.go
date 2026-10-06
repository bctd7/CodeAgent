// Package tools 提供默认工具集合的显式构造入口。
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/tools/basic"
	"CodeAgent/internal/tools/basic/read"
	"CodeAgent/internal/tools/basic/shell"
)

// Config 定义基础工具集合所需的运行配置。
type Config struct {
	WorkingDir string
}

// New 创建基础工具集合。
// WorkingDir 为空时使用当前工作目录，创建时解析并固定为绝对路径。
// 工具执行时的相对路径基于该目录解析，不调用 os.Chdir()。
// 默认依次提供 read、当前平台命令工具（Windows 为 powershell，其他平台为 bash）、edit、write。
// find、grep、ls 与扩展工具由调用方按需使用各子包构造器。
func New(cfg Config) ([]agent.Tool, error) {
	wd := strings.TrimSpace(cfg.WorkingDir)
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("获取当前工作目录失败: %w", err)
		}
	} else if wd == "~" || strings.HasPrefix(wd, "~/") || strings.HasPrefix(wd, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("解析主目录失败: %w", err)
		}
		if wd == "~" {
			wd = home
		} else {
			wd = filepath.Join(home, wd[2:])
		}
	}
	absWd, err := filepath.Abs(wd)
	if err != nil {
		return nil, fmt.Errorf("解析工作目录绝对路径失败: %w", err)
	}

	return []agent.Tool{
		read.New(absWd),
		shell.New(absWd),
		basic.NewEdit(absWd),
		basic.NewWrite(absWd),
	}, nil
}
