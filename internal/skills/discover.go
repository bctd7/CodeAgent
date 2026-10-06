package skills

import (
	"os"
	"path/filepath"
	"strings"
)

const maxDepth = 64

// discover 将文件候选交给 onSkill，由 Load 校验元数据；诊断即时交给 onDiagnostic。
// 找到 SKILL.md 后停止目录递归，即使该入口无效也不深入其子目录。
func discover(root string, onSkill func(string, os.FileInfo), onDiagnostic func(string, string, string)) {
	visited := make(map[string]bool)
	var walk func(string, int)
	walk = func(path string, depth int) {
		if depth > maxDepth {
			onDiagnostic("depth", path, "技能目录超过最大扫描深度 64")
			return
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			onDiagnostic("path", path, err.Error())
			return
		}
		if !withinRoot(root, real) {
			onDiagnostic("outside-root", path, "链接目标超出来源目录")
			return
		}
		if visited[real] {
			onDiagnostic("duplicate-directory", path, "已扫描此真实目录，跳过循环或重复链接")
			return
		}
		visited[real] = true
		entries, err := os.ReadDir(real)
		if err != nil {
			onDiagnostic("read", path, err.Error())
			return
		}
		for _, entry := range entries {
			if entry.Name() != "SKILL.md" {
				continue
			}
			file := filepath.Join(real, entry.Name())
			resolved, err := filepath.EvalSymlinks(file)
			if err != nil {
				onDiagnostic("path", file, err.Error())
				return
			}
			if !withinRoot(root, resolved) {
				onDiagnostic("outside-root", file, "技能文件链接目标超出来源目录")
				return
			}
			info, err := os.Stat(resolved)
			if err != nil {
				onDiagnostic("read", file, err.Error())
				return
			}
			if !info.Mode().IsRegular() {
				onDiagnostic("format", file, "SKILL.md 必须是普通文件")
				return
			}
			onSkill(file, info)
			return
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
				continue
			}
			child := filepath.Join(real, entry.Name())
			isDir := entry.IsDir()
			if entry.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(child)
				if err != nil {
					onDiagnostic("path", child, err.Error())
					continue
				}
				isDir = info.IsDir()
			}
			if isDir {
				walk(child, depth+1)
			}
		}
	}
	walk(root, 0)
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
