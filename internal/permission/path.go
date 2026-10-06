package permission

import (
	"path/filepath"
	"runtime"
	"strings"
)

// allTargetsWithin 使用本机路径语义和目录分量边界，不使用字符串前缀授权。
func allTargetsWithin(policy Policy, targets []string, directory string, markdown bool) bool {
	root, ok := cleanAbsPath(directory)
	if !ok || len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		path, ok := resolveTargetPath(policy.WorkingDir, target)
		if !ok {
			return false
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			return false
		}
		if markdown && !strings.EqualFold(filepath.Ext(path), ".md") {
			return false
		}
	}
	return true
}

// resolveTargetPath 按工作目录词法解析并规范化目标，不访问磁盘或解析链接。
func resolveTargetPath(workingDir, target string) (string, bool) {
	if target == "" {
		return "", false
	}
	if filepath.IsAbs(target) {
		return cleanAbsPath(target)
	}
	// Windows 的 C:child 和 \child 依赖进程当前盘符，不能按工作目录猜测。
	if filepath.VolumeName(target) != "" || strings.HasPrefix(target, "/") ||
		(runtime.GOOS == "windows" && strings.HasPrefix(target, `\`)) {
		return "", false
	}
	root, ok := cleanAbsPath(workingDir)
	if !ok {
		return "", false
	}
	return cleanAbsPath(filepath.Join(root, target))
}

// cleanAbsPath 检查并词法规范化绝对路径，不访问磁盘或解析链接。
func cleanAbsPath(path string) (string, bool) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", false
	}
	if runtime.GOOS == "windows" {
		// 不把设备命名空间、备用数据流或 Windows 名称别名视为普通文件。
		native := strings.ReplaceAll(path, "/", `\`)
		if strings.HasPrefix(native, `\\?\`) || strings.HasPrefix(native, `\\.\`) {
			return "", false
		}
		tail := strings.TrimPrefix(native, filepath.VolumeName(native))
		for _, component := range strings.Split(tail, `\`) {
			if component == "" || component == "." || component == ".." {
				continue
			}
			// 先检查设备名主干，避免 Go 在带扩展名的保留名称上查询 Windows 版本行为。
			base, _, _ := strings.Cut(component, ".")
			base = strings.TrimRight(base, " ")
			if (base != "" && !filepath.IsLocal(base)) || strings.ContainsAny(component, `<>:"|?*`) ||
				strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
				return "", false
			}
		}
	}
	return filepath.Clean(path), true
}
