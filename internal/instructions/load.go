package instructions

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Load 每次读取全局及项目祖先链的指令文档，不扫描旁支或组装模型提示词。
// 配置、目录、取消错误立即返回；文件错误通过 Diagnostics 和汇总 error 同时报告。
// 返回 error 时可能已有部分文档，调用方应先处理 error 再决定是否使用。
func Load(ctx context.Context, options Options) (Result, error) {
	result := Result{Documents: []Document{}, Diagnostics: []Diagnostic{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if options.MaxBytes < 0 {
		return result, fmt.Errorf("instructions: negative MaxBytes")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.FileName == "" {
		options.FileName = DefaultFileName
	}
	if err := ValidateFileName(options.FileName); err != nil {
		return result, err
	}
	rootDir, candidates, err := discover(options)
	if err != nil {
		return result, err
	}
	var root *os.Root
	if rootDir != "" {
		root, err = os.OpenRoot(rootDir)
		if err != nil {
			return result, fmt.Errorf("instructions: open project root: %w", err)
		}
		defer root.Close()
	}

	seen := []os.FileInfo{}
	errorsFound := []error{}
	for _, doc := range candidates {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(errorsFound, err)...)
		}
		read, err := readDocument(doc, root, rootDir, options.MaxBytes-result.BytesRead, seen)
		result.BytesRead += read.bytesRead
		if err != nil {
			wrapped := fmt.Errorf("instructions: %s: %w", doc.Path, err)
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: doc.Path, Scope: doc.Scope, Err: wrapped})
			errorsFound = append(errorsFound, wrapped)
		}
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(errorsFound, err)...)
		}
		if err != nil || read.missing || read.duplicate {
			continue
		}
		// 只登记读取与关闭成功的身份，失败来源不压掉后续别名。
		seen = append(seen, read.info)
		doc.Content = read.content
		result.Documents = append(result.Documents, doc)
	}
	return result, errors.Join(errorsFound...)
}
