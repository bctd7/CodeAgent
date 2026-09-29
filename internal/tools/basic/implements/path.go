package basic

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolvePath 解析文件路径。
// 相对路径基于固定工作目录 workingDir。
// 支持绝对路径，以及 ~ 与 ~/、~\ 用户主目录展开。
// 不限制工作目录之外的路径，不调用 os.Chdir。
func resolvePath(workingDir, filePath string) (string, error) {
	if strings.TrimSpace(filePath) == "" {
		return "", errors.New("文件路径不能为空")
	}

	if filePath == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户主目录失败: %w", err)
		}
		return filepath.Clean(home), nil
	}

	if strings.HasPrefix(filePath, "~/") || strings.HasPrefix(filePath, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户主目录失败: %w", err)
		}
		return filepath.Clean(filepath.Join(home, filePath[2:])), nil
	}

	if filepath.IsAbs(filePath) {
		return filepath.Clean(filePath), nil
	}

	return filepath.Clean(filepath.Join(workingDir, filePath)), nil
}

// ResolvePath 是 resolvePath 的导出别名。
func ResolvePath(workingDir, filePath string) (string, error) {
	return resolvePath(workingDir, filePath)
}

// formatRelativePath 返回相对于 baseDir 的路径；如果无法计算相对路径或路径在 baseDir 之外，
// 则返回 targetPath 的干净路径。路径分隔符统一使用 /。
func formatRelativePath(baseDir, targetPath string) string {
	rel, err := filepath.Rel(baseDir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(filepath.Clean(targetPath))
	}
	return filepath.ToSlash(rel)
}

// FormatRelativePath 是 formatRelativePath 的导出别名。
func FormatRelativePath(baseDir, targetPath string) string {
	return formatRelativePath(baseDir, targetPath)
}
