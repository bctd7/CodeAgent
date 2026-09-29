// Package basic contains the built-in tools available to the Agent.
// index.go provides the unified creation entrypoint for basic tools.
package basic

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"CodeAgent/internal/agent"
	implements "CodeAgent/internal/tools/basic/implements"
)

// Config 定义基础工具集合所需的运行配置。
type Config struct {
	WorkingDir string
}

// New 创建基础工具集合。
// WorkingDir 为空时使用当前工作目录，创建时解析并固定为绝对路径。
// 工具执行时的相对路径基于该目录解析，不调用 os.Chdir()。
// 显式注册全部七种工具：edit、find、grep、read、write、ls 及当前平台的命令工具（powershell 或 bash）。
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

	editTool := implements.NewEditTool(absWd)
	findTool := implements.NewFindTool(absWd)
	grepTool := implements.NewGrepTool(absWd)
	readTool := implements.NewReadTool(absWd)
	writeTool := implements.NewWriteTool(absWd)
	lsTool := implements.NewLsTool(absWd)
	shellTool := implements.NewShellTool(absWd)

	return []agent.Tool{editTool, findTool, grepTool, readTool, writeTool, lsTool, shellTool}, nil
}
