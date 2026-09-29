package basic

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultMaxLines   = 2000
	DefaultMaxBytes   = 50 * 1024 // 50 KiB (51200 字节)
	GrepMaxLineLength = 500       // Grep 匹配单行最大 Unicode 字符数
)

// TruncateOptions 定义文本截断的行数与字节数限制。
type TruncateOptions struct {
	MaxLines int
	MaxBytes int
}

// TruncateResult 记录截断处理的结果与统计信息。
type TruncateResult struct {
	Content               string // 截断后的正文内容
	Truncated             bool   // 是否触发了截断
	TruncatedBy           string // 触发截断的限制类型："lines"、"bytes" 或 ""
	TotalLines            int    // 原文本总行数
	TotalBytes            int    // 原文本总字节数
	OutputLines           int    // 截断输出中的完整行数
	OutputBytes           int    // 截断输出的字节数
	LastLinePartial       bool   // 末行是否被部分截断（仅保留末尾且最后单行超限时为 true）
	FirstLineExceedsLimit bool   // 首行是否单独超过字节上限（仅保留开头时使用）
}

// limitedBuffer 提供具有上限的内存缓冲区，防止 stderr 输出过大耗尽内存。
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Buffer.Len() >= b.max {
		return len(p), nil
	}
	remaining := b.max - b.Buffer.Len()
	if len(p) > remaining {
		b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// escapeControlChars 对路径中的换行与控制字符进行转义，防止结果被折行破坏。
func escapeControlChars(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(fmt.Sprintf(`\x%02x`, r))
			} else if unicode.IsControl(r) {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// FormatSize 将字节大小格式化为人类可读的字符串（B, KB, MB）。
func FormatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024.0)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024.0*1024.0))
}

// splitLinesForCounting 将内容拆分为行用于计数与行级截断。
// 末尾换行不额外算出一个空行；空字符串返回空切片。
func splitLinesForCounting(content string) []string {
	if len(content) == 0 {
		return []string{}
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// truncateStringToBytesFromEnd 从末尾截取最多 maxBytes 字节，
// 且保证在合法的 UTF-8 字符边界起始，避免截断损坏多字节字符。
func truncateStringToBytesFromEnd(str string, maxBytes int) string {
	b := []byte(str)
	if len(b) <= maxBytes {
		return str
	}
	start := len(b) - maxBytes
	for start < len(b) && !utf8.RuneStart(b[start]) {
		start++
	}
	return string(b[start:])
}

// TruncateHead 保留文本开头的完整行，适用于文件读取等从头部查看的场景。
// 统一默认文本上限为 2000 行、50 KiB，先达到者生效。
// 首行单独超过字节上限时，返回空内容并置 FirstLineExceedsLimit 为 true。
func TruncateHead(content string, opts TruncateOptions) TruncateResult {
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return TruncateResult{
			Content:     content,
			Truncated:   false,
			TruncatedBy: "",
			TotalLines:  totalLines,
			TotalBytes:  totalBytes,
			OutputLines: totalLines,
			OutputBytes: totalBytes,
		}
	}

	if len(lines) > 0 && len(lines[0]) > maxBytes {
		return TruncateResult{
			Content:               "",
			Truncated:             true,
			TruncatedBy:           "bytes",
			TotalLines:            totalLines,
			TotalBytes:            totalBytes,
			OutputLines:           0,
			OutputBytes:           0,
			FirstLineExceedsLimit: true,
		}
	}

	var outputLinesArr []string
	outputBytesCount := 0
	truncatedBy := "lines"

	for i := 0; i < len(lines) && i < maxLines; i++ {
		line := lines[i]
		lineBytes := len(line)
		if i > 0 {
			lineBytes++
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}

		outputLinesArr = append(outputLinesArr, line)
		outputBytesCount += lineBytes
	}

	if len(outputLinesArr) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	outputContent := strings.Join(outputLinesArr, "\n")
	return TruncateResult{
		Content:     outputContent,
		Truncated:   true,
		TruncatedBy: truncatedBy,
		TotalLines:  totalLines,
		TotalBytes:  totalBytes,
		OutputLines: len(outputLinesArr),
		OutputBytes: len(outputContent),
	}
}

// TruncateTail 保留文本末尾的内容，适用于 Shell 命令输出等查看尾部日志的场景。
// 统一默认文本上限为 2000 行、50 KiB，先达到者生效。
// 仅保留完整行，除非末行单行即超过字节上限，此时截取末尾部分并置 LastLinePartial 为 true。
func TruncateTail(content string, opts TruncateOptions) TruncateResult {
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return TruncateResult{
			Content:     content,
			Truncated:   false,
			TruncatedBy: "",
			TotalLines:  totalLines,
			TotalBytes:  totalBytes,
			OutputLines: totalLines,
			OutputBytes: totalBytes,
		}
	}

	var collected []string
	outputBytesCount := 0
	truncatedBy := "lines"
	lastLinePartial := false

	for i := len(lines) - 1; i >= 0 && len(collected) < maxLines; i-- {
		line := lines[i]
		lineBytes := len(line)
		if len(collected) > 0 {
			lineBytes++
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			if len(collected) == 0 {
				truncatedLine := truncateStringToBytesFromEnd(line, maxBytes)
				collected = append(collected, truncatedLine)
				outputBytesCount = len(truncatedLine)
				lastLinePartial = true
			}
			break
		}

		collected = append(collected, line)
		outputBytesCount += lineBytes
	}

	if len(collected) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	// 逆序反转得到正常顺序，避免每次向切片头部插入
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}

	outputContent := strings.Join(collected, "\n")
	return TruncateResult{
		Content:         outputContent,
		Truncated:       true,
		TruncatedBy:     truncatedBy,
		TotalLines:      totalLines,
		TotalBytes:      totalBytes,
		OutputLines:     len(collected),
		OutputBytes:     len(outputContent),
		LastLinePartial: lastLinePartial,
	}
}

// TruncateLine 对单行文本进行长度缩短，超过 maxChars (Unicode 字符数) 时添加 "... [truncated]" 后缀。
func TruncateLine(line string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		maxChars = GrepMaxLineLength
	}
	runes := []rune(line)
	if len(runes) <= maxChars {
		return line, false
	}
	return string(runes[:maxChars]) + "... [truncated]", true
}
