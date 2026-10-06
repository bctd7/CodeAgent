package filechange

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func pathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(pathKey(parent), pathKey(child))
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// 再按祖先目录身份比较，防止大小写不敏感卷、Windows 短名等真实别名绕过字符串检查。
func separateDirectories(project, journal string) error {
	projectInfo, err := os.Lstat(project)
	if err != nil {
		return err
	}
	journalInfo, err := os.Lstat(journal)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, pair := range []struct {
		path  string
		other fs.FileInfo
	}{{journal, projectInfo}, {project, journalInfo}} {
		if pair.other == nil {
			continue
		}
		for path := pair.path; ; path = filepath.Dir(path) {
			info, err := os.Lstat(path)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if err == nil && os.SameFile(info, pair.other) {
				return fmt.Errorf("%w: journal and project identities overlap", ErrInvalid)
			}
			if filepath.Dir(path) == path {
				break
			}
		}
	}
	return nil
}

func relativePath(root, path string) (string, error) {
	if path == "" {
		return "", ErrInvalid
	}
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(root, path)
		if err != nil {
			return "", fmt.Errorf("%w: path outside project", ErrInvalid)
		}
	}
	if !filepath.IsLocal(path) || path == "." {
		return "", fmt.Errorf("%w: path outside project", ErrInvalid)
	}
	// 不接受路径别名中的回退、尾分隔符或 Windows 的流/尾点语法。
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("%w: non-canonical path", ErrInvalid)
		}
		if runtime.GOOS == "windows" && (strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, ":*?\"<>|")) {
			return "", fmt.Errorf("%w: unsupported Windows path", ErrInvalid)
		}
	}
	return filepath.Clean(path), nil
}

func plainDirectory(info fs.FileInfo) bool {
	return info.IsDir() && info.Mode()&fs.ModeType == fs.ModeDir && !reparse(info)
}

func plainFile(info fs.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode() & ^fs.ModePerm == 0 && !reparse(info)
}

// 根目录及其祖先不接受链接，避免以链接路径重新打开时绑定到另一个项目。
func checkDirectoryPath(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !plainDirectory(info) {
			return fmt.Errorf("%w: non-plain directory %s", ErrInvalid, current)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func (s *Store) checkRoots() error {
	for _, pair := range []struct {
		root *os.Root
		info fs.FileInfo
	}{{s.project, s.projectInfo}, {s.journal, s.journalInfo}} {
		if err := checkDirectoryPath(pair.root.Name()); err != nil {
			return err
		}
		info, err := os.Lstat(pair.root.Name())
		if err != nil {
			return err
		}
		if !os.SameFile(info, pair.info) {
			return fmt.Errorf("%w: root directory identity changed", ErrConflict)
		}
	}
	return separateDirectories(s.project.Name(), s.journal.Name())
}

// 检查每一层后再取得父目录句柄；实际读写都相对于句柄进行。
func openParent(root *os.Root, path string) (*os.Root, error) {
	parent := filepath.Dir(path)
	if parent != "." {
		prefix := ""
		for _, part := range strings.Split(parent, string(filepath.Separator)) {
			prefix = filepath.Join(prefix, part)
			info, err := root.Lstat(prefix)
			if err != nil {
				return nil, err
			}
			if !plainDirectory(info) {
				return nil, fmt.Errorf("%w: linked or non-directory parent %s", ErrConflict, prefix)
			}
		}
	}
	info, err := root.Lstat(parent)
	if err != nil {
		return nil, err
	}
	p, err := root.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	opened, err := p.Stat(".")
	if err != nil || !plainDirectory(info) || !os.SameFile(info, opened) {
		p.Close()
		return nil, fmt.Errorf("%w: parent changed while opening", ErrConflict)
	}
	return p, nil
}

func checkLeaf(root *os.Root, name string, missingOK bool) error {
	info, err := root.Lstat(name)
	if missingOK && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !plainFile(info) {
		return fmt.Errorf("%w: %s is not a plain file", ErrConflict, name)
	}
	return nil
}

func verifyOpened(root *os.Root, name string, f *os.File) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !plainFile(info) || !plainFile(opened) || !os.SameFile(info, opened) {
		return fmt.Errorf("%w: %s identity or type changed", ErrConflict, name)
	}
	_, links, err := fileIdentity(f)
	if err != nil {
		return err
	}
	if links != 1 {
		return fmt.Errorf("%w: hard links are unsupported: %s", ErrConflict, name)
	}
	return nil
}

func identityAt(root *os.Root, path string) (string, error) {
	p, err := openParent(root, path)
	if err != nil {
		return "", err
	}
	defer p.Close()
	name := filepath.Base(path)
	if err := checkLeaf(p, name, false); err != nil {
		return "", err
	}
	f, err := p.OpenFile(name, readFlags(), 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := verifyOpened(p, name, f); err != nil {
		return "", err
	}
	id, _, err := fileIdentity(f)
	return id, err
}
