package basic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

const (
	grepToolName     = "grep"
	defaultGrepLimit = 100
	maxGrepLineBuf   = 1024 * 1024 // 单行 1 MiB 上限
)

var grepSchema = []byte(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Search pattern (regular expression by default)"
    },
    "path": {
      "type": "string",
      "description": "Directory or file to search in (default: current directory)"
    },
    "glob": {
      "type": "string",
      "description": "File glob pattern to filter results (e.g., '*.ts')"
    },
    "ignoreCase": {
      "type": "boolean",
      "description": "Case-insensitive search (default: false)"
    },
    "literal": {
      "type": "boolean",
      "description": "Treat pattern as literal string instead of regex (default: false)"
    },
    "context": {
      "type": "integer",
      "description": "Number of context lines before and after match (default: 0)"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of matches to return (default: 100)"
    }
  },
  "required": ["pattern"]
}`)

type rgSubtext struct {
	Text  *string `json:"text"`
	Bytes *string `json:"bytes"`
}

type rgMatchEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgSubtext `json:"path"`
		Lines      rgSubtext `json:"lines"`
		LineNumber int       `json:"line_number"`
	} `json:"data"`
}

// NewGrepTool 构造 grep 工具实例。
func NewGrepTool(workingDir string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name: grepToolName,
			Description: "Search file contents for patterns using ripgrep (rg). " +
				"Supports regular expressions and literal search, glob filtering, context lines, and case-insensitive matching.",
			Schema: grepSchema,
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return executeGrep(ctx, workingDir, args)
		},
	}
}

func decodeRgSubtext(st rgSubtext) (string, error) {
	if st.Text != nil {
		return *st.Text, nil
	}
	if st.Bytes != nil {
		decoded, err := base64.StdEncoding.DecodeString(*st.Bytes)
		if err != nil {
			return "", fmt.Errorf("解码 ripgrep base64 数据失败: %w", err)
		}
		return string(decoded), nil
	}
	return "", nil
}

func parseGrepArgs(args map[string]any) (pattern, pathStr, globPattern string, ignoreCase, literal bool, contextVal, limitVal int, err error) {
	patternVal, exists := args["pattern"]
	if !exists {
		return "", "", "", false, false, 0, 0, errors.New("缺少必填参数 'pattern'")
	}
	pattern, ok := patternVal.(string)
	if !ok || pattern == "" {
		return "", "", "", false, false, 0, 0, errors.New("参数 'pattern' 必须为非空字符串")
	}

	pathStr = "."
	if val, exists := args["path"]; exists && val != nil {
		s, ok := val.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return "", "", "", false, false, 0, 0, errors.New("参数 'path' 必须为非空字符串")
		}
		pathStr = s
	}

	if val, exists := args["glob"]; exists && val != nil {
		s, ok := val.(string)
		if !ok {
			return "", "", "", false, false, 0, 0, errors.New("参数 'glob' 必须为字符串")
		}
		globPattern = strings.TrimSpace(s)
	}

	if val, exists := args["ignoreCase"]; exists && val != nil {
		b, ok := val.(bool)
		if !ok {
			return "", "", "", false, false, 0, 0, errors.New("参数 'ignoreCase' 必须为布尔值")
		}
		ignoreCase = b
	}

	if val, exists := args["literal"]; exists && val != nil {
		b, ok := val.(bool)
		if !ok {
			return "", "", "", false, false, 0, 0, errors.New("参数 'literal' 必须为布尔值")
		}
		literal = b
	}

	contextVal = 0
	if val, exists := args["context"]; exists && val != nil {
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v < 0 || v > math.MaxInt32 {
				return "", "", "", false, false, 0, 0, errors.New("参数 'context' 必须为 0 至 2147483647 的整数")
			}
			contextVal = int(v)
		case int:
			if v < 0 || v > math.MaxInt32 {
				return "", "", "", false, false, 0, 0, errors.New("参数 'context' 必须为 0 至 2147483647 的整数")
			}
			contextVal = v
		case int64:
			if v < 0 || v > math.MaxInt32 {
				return "", "", "", false, false, 0, 0, errors.New("参数 'context' 必须为 0 至 2147483647 的整数")
			}
			contextVal = int(v)
		default:
			return "", "", "", false, false, 0, 0, errors.New("参数 'context' 必须为 0 至 2147483647 的整数")
		}
	}

	limitVal = defaultGrepLimit
	if val, exists := args["limit"]; exists && val != nil {
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v <= 0 || v > math.MaxInt32 {
				return "", "", "", false, false, 0, 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = int(v)
		case int:
			if v <= 0 || v > math.MaxInt32 {
				return "", "", "", false, false, 0, 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = v
		case int64:
			if v <= 0 || v > math.MaxInt32 {
				return "", "", "", false, false, 0, 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = int(v)
		default:
			return "", "", "", false, false, 0, 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
		}
	}

	return pattern, pathStr, globPattern, ignoreCase, literal, contextVal, limitVal, nil
}

func executeGrep(ctx context.Context, workingDir string, args map[string]any) ([]llm.ContentBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pattern, pathStr, globPattern, ignoreCase, literal, contextVal, effectiveLimit, err := parseGrepArgs(args)
	if err != nil {
		return nil, err
	}

	rgPath, err := exec.LookPath("rg")
	if err != nil {
		return nil, fmt.Errorf("ripgrep (rg) 未在 PATH 中找到，请安装 ripgrep: %w", err)
	}

	searchPath, err := resolvePath(workingDir, pathStr)
	if err != nil {
		return nil, err
	}

	fi, err := os.Stat(searchPath)
	if err != nil {
		return nil, fmt.Errorf("Path not found: %s", searchPath)
	}
	isDirectory := fi.IsDir()

	cmdArgs := []string{"--json", "--line-number", "--color=never", "--hidden"}
	if ignoreCase {
		cmdArgs = append(cmdArgs, "--ignore-case")
	}
	if literal {
		cmdArgs = append(cmdArgs, "--fixed-strings")
	}
	if globPattern != "" {
		cmdArgs = append(cmdArgs, "--glob", globPattern)
	}
	if contextVal > 0 {
		cmdArgs = append(cmdArgs, "--context", strconv.Itoa(contextVal))
	}
	cmdArgs = append(cmdArgs, "--", pattern, searchPath)

	cmd := exec.CommandContext(ctx, rgPath, cmdArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}

	stderrBuf := &limitedBuffer{max: 16 * 1024}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ripgrep 失败: %w", err)
	}

	var (
		outputLines       []string
		totalBytesCount   int
		bytesLimitHit     bool
		linesTruncated    bool
		matchCount        int
		matchLimitReached bool
		lastMatchFile     string
		lastMatchLine     int
		maxTrailingLine   int
		lastEmittedFile   string
		lastEmittedLine   int
		scanErr           error
		parseErr          error
	)

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxGrepLineBuf)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var ev rgMatchEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			parseErr = fmt.Errorf("解析 ripgrep JSON 输出失败: %w", err)
			break
		}

		if ev.Type == "end" && matchLimitReached {
			pathText, pErr := decodeRgSubtext(ev.Data.Path)
			if pErr != nil {
				parseErr = pErr
				break
			}
			if pathText == lastMatchFile {
				break
			}
		}
		if ev.Type != "match" && ev.Type != "context" {
			continue
		}

		pathText, pErr := decodeRgSubtext(ev.Data.Path)
		if pErr != nil {
			parseErr = pErr
			break
		}
		lineText, lErr := decodeRgSubtext(ev.Data.Lines)
		if lErr != nil {
			parseErr = lErr
			break
		}
		lineNum := ev.Data.LineNumber

		// 同一文件同一行避免重复展示
		if pathText == lastEmittedFile && lineNum == lastEmittedLine {
			continue
		}

		var isMatch bool
		if !matchLimitReached {
			if ev.Type == "match" {
				isMatch = true
				matchCount++
				lastMatchFile = pathText
				lastMatchLine = lineNum
				maxTrailingLine = lineNum + contextVal
				if matchCount >= effectiveLimit {
					matchLimitReached = true
				}
			} else {
				isMatch = false
			}
		} else {
			// 已达到匹配上限，仅在最后一条已接受匹配的尾随上下文范围内继续作为上下文展示
			if pathText == lastMatchFile && lineNum > lastMatchLine && lineNum <= maxTrailingLine {
				isMatch = false
			} else {
				// 超出尾随上下文范围或进入新文件，结束收集
				break
			}
		}

		// 格式化输出行与转义
		relPath := formatRelativePath(searchPath, pathText)
		if !isDirectory {
			relPath = filepath.Base(pathText)
		}
		relPath = escapeControlChars(relPath)

		sanitized := strings.TrimRight(lineText, "\r\n")
		truncatedText, wasTrunc := TruncateLine(sanitized, GrepMaxLineLength)
		if wasTrunc {
			linesTruncated = true
		}

		var lineStr string
		if isMatch {
			lineStr = fmt.Sprintf("%s:%d: %s", relPath, lineNum, truncatedText)
		} else {
			lineStr = fmt.Sprintf("%s-%d- %s", relPath, lineNum, truncatedText)
		}

		lineBytes := len(lineStr)
		if len(outputLines) > 0 {
			lineBytes++
		}

		// 严格控制 50 KiB 输出预算
		if totalBytesCount+lineBytes > DefaultMaxBytes {
			bytesLimitHit = true
			break
		}

		outputLines = append(outputLines, lineStr)
		totalBytesCount += lineBytes
		lastEmittedFile = pathText
		lastEmittedLine = lineNum

		// 尾随上下文收集完毕即结束
		if matchLimitReached && lineNum >= maxTrailingLine {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		scanErr = err
	}

	// 提前结束或出错时主动杀死子进程并关闭读取端，避免管道写满死锁
	if matchLimitReached || bytesLimitHit || parseErr != nil || scanErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = stdout.Close()
	}

	waitErr := cmd.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if scanErr != nil {
		return nil, fmt.Errorf("读取 ripgrep 输出失败: %w", scanErr)
	}

	if !matchLimitReached && !bytesLimitHit && waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			if code > 1 {
				errText := strings.TrimSpace(stderrBuf.String())
				if errText == "" {
					errText = fmt.Sprintf("ripgrep exited with code %d", code)
				}
				return nil, errors.New(errText)
			}
		} else {
			return nil, waitErr
		}
	}

	if len(outputLines) == 0 {
		return []llm.ContentBlock{
			{
				Type: llm.TypeText,
				Text: "No matches found",
			},
		}, nil
	}

	resultOutput := strings.Join(outputLines, "\n")

	var notices []string
	if matchLimitReached {
		notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", effectiveLimit, effectiveLimit*2))
	}
	if bytesLimitHit {
		notices = append(notices, fmt.Sprintf("%s limit reached", FormatSize(DefaultMaxBytes)))
	}
	if linesTruncated {
		notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", GrepMaxLineLength))
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
