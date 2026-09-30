package basic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

//已检查

var writeSchema = []byte(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to write (relative or absolute)"
    },
    "content": {
      "type": "string",
      "description": "Content to write to the file"
    }
  },
  "required": ["path", "content"]
}`)

// NewWriteTool 构造 write 工具实例。
func NewWriteTool(workingDir string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name: "write",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. " +
				"Automatically creates parent directories. Content is written verbatim.",
			Schema: writeSchema,
		},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return executeWrite(ctx, workingDir, args)
		},
	}
}

// executeWrite 执行写文件操作：校验参数 → 路径解析 → 权限保持 → 自动创建父目录 → 直接写入。
func executeWrite(ctx context.Context, workingDir string, args map[string]any) ([]llm.ContentBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pathStr, ok := args["path"].(string)
	if !ok || strings.TrimSpace(pathStr) == "" {
		return nil, fmt.Errorf("参数 'path' 必须为非空字符串")
	}

	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("参数 'content' 必须为字符串")
	}

	target, err := resolvePath(workingDir, pathStr)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	perm := os.FileMode(0666)
	info, err := os.Stat(target)
	switch {
	case err == nil:
		if info.IsDir() {
			return nil, fmt.Errorf("目标路径为目录而非文件: %s", target)
		}
		perm = info.Mode().Perm()
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("检查文件状态失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return nil, fmt.Errorf("创建父目录失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := os.WriteFile(target, []byte(content), perm); err != nil {
		return nil, fmt.Errorf("写入文件失败 %s: %w", target, err)
	}

	return []llm.ContentBlock{
		{
			Type: llm.TypeText,
			Text: fmt.Sprintf("已写入 %s (%d 字节)", escapeControlChars(target), len(content)),
		},
	}, nil
}
