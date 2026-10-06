// Package common 提供共享输出预算、统计、显示格式与有界缓冲。
package common

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
)

const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024 // 50 KiB (51200 字节)
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

// LimitedBuffer 提供具有上限的内存缓冲区，防止 stderr 输出过大耗尽内存。
type LimitedBuffer struct {
	bytes.Buffer
	Max int
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	if b.Buffer.Len() >= b.Max {
		return len(p), nil
	}
	remaining := b.Max - b.Buffer.Len()
	if len(p) > remaining {
		b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// EscapeControlChars 对路径中的换行与控制字符进行转义，防止结果被折行破坏。
func EscapeControlChars(s string) string {
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

// SplitLinesForCounting 将内容拆分为行用于计数与行级截断。
// 末尾换行不额外算出一个空行；空字符串返回空切片。
func SplitLinesForCounting(content string) []string {
	if len(content) == 0 {
		return []string{}
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}
