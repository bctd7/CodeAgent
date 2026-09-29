package basic

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

const (
	findToolName     = "find"
	defaultFindLimit = 1000
)

var findSchema = []byte(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob pattern to match file paths. Examples: '*.ts', 'src/**/*.ts', 'test*.js'"
    },
    "path": {
      "type": "string",
      "description": "Directory to search in (default: current directory)"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of results to return (default: 1000)"
    }
  },
  "required": ["pattern"]
}`)

// NewFindTool 构造 find 工具实例。
func NewFindTool(workingDir string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name: findToolName,
			Description: "Find files and directories matching a glob pattern using fd. " +
				"Returns matching paths relative to the search directory, separated by '/'.",
			Schema: findSchema,
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return executeFind(ctx, workingDir, args)
		},
	}
}

func parseFindArgs(args map[string]any) (pattern, pathStr string, limitVal int, err error) {
	patternVal, exists := args["pattern"]
	if !exists {
		return "", "", 0, errors.New("缺少必填参数 'pattern'")
	}
	pattern, ok := patternVal.(string)
	if !ok || strings.TrimSpace(pattern) == "" {
		return "", "", 0, errors.New("参数 'pattern' 必须为非空字符串")
	}

	pathStr = "."
	if val, exists := args["path"]; exists && val != nil {
		s, ok := val.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return "", "", 0, errors.New("参数 'path' 必须为非空字符串")
		}
		pathStr = s
	}

	limitVal = defaultFindLimit
	if val, exists := args["limit"]; exists && val != nil {
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v <= 0 || v > math.MaxInt32 {
				return "", "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = int(v)
		case int:
			if v <= 0 || v > math.MaxInt32 {
				return "", "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = v
		case int64:
			if v <= 0 || v > math.MaxInt32 {
				return "", "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = int(v)
		default:
			return "", "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
		}
	}

	return pattern, pathStr, limitVal, nil
}

func executeFind(ctx context.Context, workingDir string, args map[string]any) ([]llm.ContentBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pattern, pathStr, effectiveLimit, err := parseFindArgs(args)
	if err != nil {
		return nil, err
	}

	fdPath, err := exec.LookPath("fd")
	if err != nil {
		return nil, fmt.Errorf("fd 未在 PATH 中找到，请安装 fd: %w", err)
	}

	searchPath, err := resolvePath(workingDir, pathStr)
	if err != nil {
		return nil, err
	}

	fi, err := os.Stat(searchPath)
	if err != nil {
		return nil, fmt.Errorf("Path not found: %s", searchPath)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("Not a directory: %s", searchPath)
	}

	// 检查 searchPath 是否处于 git 仓库内
	insideGitRepo := false
	for cur := searchPath; ; {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			insideGitRepo = true
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	cmdArgs := []string{"--glob", "--color=never", "--hidden"}
	if !insideGitRepo {
		cmdArgs = append(cmdArgs, "--no-require-git")
	}
	cmdArgs = append(cmdArgs, "--max-results", strconv.Itoa(effectiveLimit))

	effectivePattern := pattern
	if strings.Contains(pattern, "/") {
		cmdArgs = append(cmdArgs, "--full-path")
		if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
			effectivePattern = "**/" + pattern
		}
		if runtime.GOOS == "windows" {
			effectivePattern = strings.ReplaceAll(effectivePattern, "/", "[/\\\\]")
		}
	}
	cmdArgs = append(cmdArgs, "--", effectivePattern, searchPath)

	cmd := exec.CommandContext(ctx, fdPath, cmdArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}

	stderrBuf := &limitedBuffer{max: 16 * 1024}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 fd 失败: %w", err)
	}

	var (
		lines           []string
		scanErr         error
		totalBytesCount int
		bytesLimitHit   bool
	)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		rawLine := strings.TrimRight(scanner.Text(), "\r\n")
		if rawLine == "" {
			continue
		}

		rel := formatRelativePath(searchPath, rawLine)
		escaped := escapeControlChars(rel)

		lineBytes := len(escaped)
		if len(lines) > 0 {
			lineBytes++
		}

		if totalBytesCount+lineBytes > DefaultMaxBytes {
			bytesLimitHit = true
			// 达到字节预算后终止子进程
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = stdout.Close()
			break
		}

		lines = append(lines, escaped)
		totalBytesCount += lineBytes
	}

	if err := scanner.Err(); err != nil {
		scanErr = err
		_ = cmd.Process.Kill()
		_ = stdout.Close()
	}

	waitErr := cmd.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, fmt.Errorf("读取 fd 输出失败: %w", scanErr)
	}

	// 真实 fd 失败不能因已有部分输出而视作成功
	if waitErr != nil && !bytesLimitHit {
		errText := strings.TrimSpace(stderrBuf.String())
		if errText == "" {
			errText = waitErr.Error()
		}
		return nil, fmt.Errorf("fd 执行失败: %s", errText)
	}

	if len(lines) == 0 {
		return []llm.ContentBlock{
			{
				Type: llm.TypeText,
				Text: "No files found matching pattern",
			},
		}, nil
	}

	resultLimitReached := len(lines) >= effectiveLimit
	resultOutput := strings.Join(lines, "\n")

	var notices []string
	if resultLimitReached {
		notices = append(notices, fmt.Sprintf("%d results limit reached. Use limit=%d for more, or refine pattern", effectiveLimit, effectiveLimit*2))
	}
	if bytesLimitHit {
		notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
	}
	if len(notices) > 0 {
		resultOutput += "\n\n[" + strings.Join(notices, ". ") + "]"
	}

	return []llm.ContentBlock{
		{
			Type: llm.TypeText,
			Text: resultOutput,
		},
	}, nil
}
