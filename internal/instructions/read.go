package instructions

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type fileRead struct {
	content   string
	info      os.FileInfo
	bytesRead int64
	missing   bool
	duplicate bool
}

// readDocument 按剩余预算读取一个候选；失败也报告已读字节，关闭成功才算读取成功。
func readDocument(doc Document, root *os.Root, rootDir string, remaining int64, seen []os.FileInfo) (result fileRead, err error) {
	name := doc.Path
	if !doc.Scope.Global {
		name, err = filepath.Rel(rootDir, doc.Path)
		if err != nil {
			return result, err
		}
	}
	var info os.FileInfo
	if doc.Scope.Global {
		info, err = os.Stat(name)
	} else {
		info, err = root.Stat(name)
	}
	if os.IsNotExist(err) {
		result.missing = true
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("instructions: not a regular file")
	}
	var file *os.File
	if doc.Scope.Global {
		file, err = os.Open(name)
	} else {
		file, err = root.Open(name)
	}
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	result.info, err = file.Stat()
	if err != nil {
		return result, err
	}
	// 打开后的检查不能由路径检查代替，路径可能在打开前发生变化。
	if !result.info.Mode().IsRegular() {
		return result, fmt.Errorf("instructions: not a regular file")
	}
	for _, previous := range seen {
		if os.SameFile(previous, result.info) {
			result.duplicate = true
			return result, nil
		}
	}
	if result.info.Size() > remaining {
		return result, ErrBudget
	}
	data, err := io.ReadAll(io.LimitReader(file, remaining))
	result.bytesRead = int64(len(data))
	if err != nil {
		return result, err
	}
	// 并发增长导致的不完整正文不得当作成功文档。
	after, err := file.Stat()
	if err != nil {
		return result, err
	}
	if after.Size() > int64(len(data)) {
		if after.Size() > remaining {
			return result, ErrBudget
		}
		return result, io.ErrUnexpectedEOF
	}
	if !utf8.Valid(data) {
		return result, fmt.Errorf("instructions: invalid UTF-8")
	}
	result.content = strings.TrimPrefix(string(data), "\ufeff")
	return result, nil
}
