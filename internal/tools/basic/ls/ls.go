package ls

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/tools/internal/output"
	"CodeAgent/internal/tools/internal/pathutil"
)

const (
	lsToolName     = "ls"
	defaultLsLimit = 500
)

var lsSchema = []byte(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory to list (default: current directory)"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of entries to return (default: 500)"
    }
  }
}`)

// New 构造 ls 工具实例。
func New(workingDir string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name: lsToolName,
			Description: "List directory contents. Returns entries sorted alphabetically (case-insensitive), " +
				"with '/' suffix for directories. Includes dotfiles/hidden items.",
			Schema: lsSchema,
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return executeLs(ctx, workingDir, args)
		},
	}
}

func parseLsArgs(args map[string]any) (string, int, error) {
	pathStr := "."
	if val, exists := args["path"]; exists && val != nil {
		s, ok := val.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return "", 0, errors.New("参数 'path' 必须为非空字符串")
		}
		pathStr = s
	}

	limitVal := defaultLsLimit
	if val, exists := args["limit"]; exists && val != nil {
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v <= 0 || v > math.MaxInt32 {
				return "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = int(v)
		case int:
			if v <= 0 || v > math.MaxInt32 {
				return "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = v
		case int64:
			if v <= 0 || v > math.MaxInt32 {
				return "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
			}
			limitVal = int(v)
		default:
			return "", 0, errors.New("参数 'limit' 必须为 1 至 2147483647 的整数")
		}
	}

	return pathStr, limitVal, nil
}

func executeLs(ctx context.Context, workingDir string, args map[string]any) ([]llm.ContentBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pathStr, effectiveLimit, err := parseLsArgs(args)
	if err != nil {
		return nil, err
	}

	searchPath, err := pathutil.ResolvePath(workingDir, pathStr)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fi, err := os.Stat(searchPath)
	if err != nil {
		return nil, fmt.Errorf("Path not found: %s", searchPath)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("Not a directory: %s", searchPath)
	}

	entries, err := os.ReadDir(searchPath)
	if err != nil {
		return nil, fmt.Errorf("Cannot read directory: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(entries, func(i, j int) bool {
		lowerI := strings.ToLower(entries[i].Name())
		lowerJ := strings.ToLower(entries[j].Name())
		if lowerI != lowerJ {
			return lowerI < lowerJ
		}
		return entries[i].Name() < entries[j].Name()
	})

	var (
		results         []string
		entryLimitHit   bool
		totalBytesCount int
		bytesLimitHit   bool
	)

	for _, entry := range entries {
		if len(results) >= effectiveLimit {
			entryLimitHit = true
			break
		}

		fullPath := filepath.Join(searchPath, entry.Name())
		targetStat, err := os.Stat(fullPath)
		if err != nil {
			continue
		}

		suffix := ""
		if targetStat.IsDir() {
			suffix = "/"
		}
		line := output.EscapeControlChars(entry.Name()) + suffix

		lineBytes := len(line)
		if len(results) > 0 {
			lineBytes++
		}

		if totalBytesCount+lineBytes > output.DefaultMaxBytes {
			bytesLimitHit = true
			break
		}

		results = append(results, line)
		totalBytesCount += lineBytes
	}

	if len(results) == 0 {
		return []llm.ContentBlock{
			{
				Type: llm.TypeText,
				Text: "(empty directory)",
			},
		}, nil
	}

	resultOutput := strings.Join(results, "\n")

	var notices []string
	if entryLimitHit {
		notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", effectiveLimit, effectiveLimit*2))
	}
	if bytesLimitHit {
		notices = append(notices, fmt.Sprintf("%s limit reached", output.FormatSize(output.DefaultMaxBytes)))
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
