package instructions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateFileName 检查单个文件名；空值表示使用默认文件名，不允许路径或仅空白。
func ValidateFileName(name string) error {
	if name == "" {
		return nil
	}
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:\x00") {
		return fmt.Errorf("instructions: invalid file name: %q", name)
	}
	return nil
}

// discover 只解析目录并构造候选顺序，不打开根句柄或读取正文。
func discover(options Options) (rootDir string, candidates []Document, err error) {
	var rel string
	if options.ProjectRoot != "" {
		rootDir, err = directory(options.ProjectRoot)
		if err != nil {
			return "", nil, fmt.Errorf("instructions: project root: %w", err)
		}
		target := options.TargetDir
		if target == "" {
			target = rootDir
		} else if !filepath.IsAbs(target) {
			// Windows 的 \child 和 C:child 不能直接拼到项目根。
			if filepath.VolumeName(target) != "" || os.IsPathSeparator(target[0]) {
				return "", nil, fmt.Errorf("instructions: ambiguous target path: %q", target)
			}
			target = filepath.Join(rootDir, target)
		}
		targetDir, err := directory(target)
		if err != nil {
			return "", nil, fmt.Errorf("instructions: target directory: %w", err)
		}
		rel, err = filepath.Rel(rootDir, targetDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", nil, fmt.Errorf("%w: %q", ErrOutsideProject, targetDir)
		}
	} else if options.TargetDir != "" {
		return "", nil, fmt.Errorf("instructions: TargetDir requires ProjectRoot")
	}

	if options.GlobalDir != "" {
		globalDir, err := filepath.Abs(options.GlobalDir)
		if err != nil {
			return "", nil, fmt.Errorf("instructions: global directory: %w", err)
		}
		if info, err := os.Stat(globalDir); err == nil && !info.IsDir() {
			return "", nil, fmt.Errorf("instructions: global location is not a directory: %q", globalDir)
		} else if err != nil && !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("instructions: global directory: %w", err)
		}
		candidates = append(candidates, Document{Path: filepath.Join(globalDir, options.FileName), Scope: Scope{Global: true}})
	}
	if rootDir != "" {
		chain := []string{rootDir}
		if rel != "." {
			for _, part := range strings.Split(rel, string(filepath.Separator)) {
				chain = append(chain, filepath.Join(chain[len(chain)-1], part))
			}
		}
		for _, dir := range chain {
			candidates = append(candidates, Document{Path: filepath.Join(dir, options.FileName), Scope: Scope{Directory: dir}})
		}
	}
	return rootDir, candidates, nil
}

// directory 将已有目录解析为实际绝对路径，供祖先查找和边界检查使用。
func directory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %q", abs)
	}
	return abs, nil
}
