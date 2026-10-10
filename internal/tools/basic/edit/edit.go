package edit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/tools/internal/pathutil"

	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/text/unicode/norm"
)

const (
	editToolName        = "edit"
	maxDiffOutputLength = 5000
)

var editSchema = []byte(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to edit (relative or absolute)"
    },
    "edits": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "oldText": {
            "type": "string",
            "description": "Exact text to replace (must match a unique, non-overlapping occurrence)"
          },
          "newText": {
            "type": "string",
            "description": "Replacement text"
          }
        },
        "required": ["oldText", "newText"]
      },
      "description": "List of text replacements to apply"
    }
  },
  "required": ["path", "edits"]
}`)

type editItem struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

type textReplacement struct {
	matchIndex  int
	matchLength int
	newText     string
}

type lineSpan struct {
	start int
	end   int
}

type replacementGroup struct {
	startLine    int
	endLine      int
	replacements []textReplacement
}

type fuzzyMatchResult struct {
	found                 bool
	index                 int
	matchLength           int
	usedFuzzyMatch        bool
	contentForReplacement string
}

// New 构造 edit 工具实例。
func New(workingDir string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name: editToolName,
			Description: "Edit a single file using exact or normalized text replacement. " +
				"Every edits[].oldText must match a unique, non-overlapping region of the original file. " +
				"If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits.",
			Schema: editSchema,
		},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return executeEdit(ctx, workingDir, args)
		},
	}
}

// executeEdit 执行编辑主流程。
func executeEdit(ctx context.Context, workingDir string, args map[string]any) ([]llm.ContentBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pathStr, edits, err := parseEditArgs(args)
	if err != nil {
		return nil, err
	}

	resolvedPath, err := pathutil.ResolvePath(workingDir, pathStr)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fi, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("无法访问文件 %s: %w", pathStr, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("目标路径为目录而非文件: %s", pathStr)
	}

	rawBytes, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败 %s: %w", pathStr, err)
	}

	if !utf8.Valid(rawBytes) {
		return nil, fmt.Errorf("文件不是合法 UTF-8 文本: %s", pathStr)
	}

	var hasBOM bool
	contentBytes := rawBytes
	if bytes.HasPrefix(rawBytes, []byte("\xef\xbb\xbf")) {
		hasBOM = true
		contentBytes = rawBytes[3:]
	}

	fileContent := string(contentBytes)
	ending := detectLineEnding(fileContent)
	normalizedContent := normalizeToLF(fileContent)

	baseContent, newContent, err := applyEditsToNormalizedContent(normalizedContent, edits, pathStr)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	finalContent := restoreLineEndings(newContent, ending)
	var finalBytes []byte
	if hasBOM {
		finalBytes = append([]byte("\xef\xbb\xbf"), []byte(finalContent)...)
	} else {
		finalBytes = []byte(finalContent)
	}

	if err := os.WriteFile(resolvedPath, finalBytes, fi.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("写入文件失败 %s: %w", pathStr, err)
	}

	diffStr := generateDiff(pathStr, baseContent, newContent)
	resultText := fmt.Sprintf("成功编辑文件: %s（共完成 %d 处替换）\n\n--- 修改差异 ---\n%s",
		pathStr, len(edits), diffStr)

	return []llm.ContentBlock{
		{
			Type: llm.TypeText,
			Text: resultText,
		},
	}, nil
}

func parseEditArgs(args map[string]any) (string, []editItem, error) {
	pathStr, ok := args["path"].(string)
	if !ok || strings.TrimSpace(pathStr) == "" {
		return "", nil, errors.New("参数 'path' 必须为非空字符串")
	}

	rawEdits, exists := args["edits"]
	if !exists || rawEdits == nil {
		return "", nil, errors.New("参数 'edits' 必须为非空数组")
	}

	var items []any
	switch v := rawEdits.(type) {
	case []any:
		items = v
	case []map[string]any:
		items = make([]any, len(v))
		for i, m := range v {
			items[i] = m
		}
	default:
		return "", nil, errors.New("参数 'edits' 必须为非空数组")
	}

	if len(items) == 0 {
		return "", nil, errors.New("参数 'edits' 必须为非空数组")
	}

	edits := make([]editItem, len(items))
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("edits[%d] 必须为对象", i)
		}
		oldText, ok := m["oldText"].(string)
		if !ok || oldText == "" {
			return "", nil, fmt.Errorf("edits[%d].oldText 必须为非空字符串", i)
		}
		newText, ok := m["newText"].(string)
		if !ok {
			return "", nil, fmt.Errorf("edits[%d].newText 必须为字符串", i)
		}
		edits[i] = editItem{OldText: oldText, NewText: newText}
	}

	return pathStr, edits, nil
}

func applyEditsToNormalizedContent(normalizedContent string, edits []editItem, path string) (string, string, error) {
	normalizedEdits := make([]editItem, len(edits))
	for i, edit := range edits {
		if len(edit.OldText) == 0 {
			if len(edits) == 1 {
				return "", "", fmt.Errorf("%s 中的 oldText 不能为空", path)
			}
			return "", "", fmt.Errorf("%s 中的 edits[%d].oldText 不能为空", path, i)
		}
		normalizedEdits[i] = editItem{
			OldText: normalizeToLF(edit.OldText),
			NewText: normalizeToLF(edit.NewText),
		}
	}

	var usedFuzzyMatch bool
	for _, edit := range normalizedEdits {
		m := fuzzyFindText(normalizedContent, edit.OldText)
		if m.usedFuzzyMatch {
			usedFuzzyMatch = true
			break
		}
	}

	var replacementBaseContent string
	if usedFuzzyMatch {
		replacementBaseContent = normalizeForFuzzyMatch(normalizedContent)
	} else {
		replacementBaseContent = normalizedContent
	}

	matchedEdits := make([]matchedEdit, 0, len(normalizedEdits))
	for i, edit := range normalizedEdits {
		matchResult := fuzzyFindText(replacementBaseContent, edit.OldText)
		if !matchResult.found {
			if len(normalizedEdits) == 1 {
				return "", "", fmt.Errorf("未在 %s 中找到待替换的旧文本。oldText 必须与原文件内容完全精确匹配（包括所有空白字符和换行）", path)
			}
			return "", "", fmt.Errorf("未在 %s 中找到 edits[%d]。oldText 必须与原文件内容完全精确匹配（包括所有空白字符和换行）", path, i)
		}

		occurrences := countOccurrences(replacementBaseContent, edit.OldText, usedFuzzyMatch)
		if occurrences > 1 {
			if len(normalizedEdits) == 1 {
				return "", "", fmt.Errorf("在 %s 中发现待替换文本存在 %d 处匹配。待替换文本必须在文件中唯一，请提供更多上下文以消除歧义", path, occurrences)
			}
			return "", "", fmt.Errorf("在 %s 中发现 edits[%d] 存在 %d 处匹配。每个 oldText 必须唯一，请提供更多上下文以消除歧义", path, i, occurrences)
		}

		matchedEdits = append(matchedEdits, matchedEdit{
			editIndex:   i,
			matchIndex:  matchResult.index,
			matchLength: matchResult.matchLength,
			newText:     edit.NewText,
		})
	}

	sort.Slice(matchedEdits, func(i, j int) bool {
		return matchedEdits[i].matchIndex < matchedEdits[j].matchIndex
	})

	for i := 1; i < len(matchedEdits); i++ {
		prev := matchedEdits[i-1]
		curr := matchedEdits[i]
		if prev.matchIndex+prev.matchLength > curr.matchIndex {
			return "", "", fmt.Errorf("edits[%d] 与 edits[%d] 在 %s 中发生重叠。请合并为一个 edit 或分别指定不重叠的区域",
				prev.editIndex, curr.editIndex, path)
		}
	}

	textReps := make([]textReplacement, len(matchedEdits))
	for i, m := range matchedEdits {
		textReps[i] = textReplacement{
			matchIndex:  m.matchIndex,
			matchLength: m.matchLength,
			newText:     m.newText,
		}
	}

	baseContent := normalizedContent
	var newContent string
	if usedFuzzyMatch {
		var err error
		newContent, err = applyReplacementsPreservingUnchangedLines(normalizedContent, replacementBaseContent, textReps)
		if err != nil {
			return "", "", err
		}
	} else {
		newContent = applyReplacements(replacementBaseContent, textReps, 0)
	}

	if baseContent == newContent {
		if len(normalizedEdits) == 1 {
			return "", "", fmt.Errorf("替换后内容没有变化：%s 的替换结果与原文件完全相同", path)
		}
		return "", "", fmt.Errorf("替换后内容没有变化：%s 的全部替换结果与原文件完全相同", path)
	}

	return baseContent, newContent, nil
}

func detectLineEnding(content string) string {
	crlf := strings.Count(content, "\r\n")
	lf := strings.Count(content, "\n") - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

func normalizeToLF(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}

func restoreLineEndings(text string, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

func normalizeForFuzzyMatch(text string) string {
	nfkc := norm.NFKC.String(text)
	lines := strings.Split(nfkc, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	s := strings.Join(lines, "\n")

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\u2018', '\u2019', '\u201A', '\u201B':
			b.WriteRune('\'')
		case '\u201C', '\u201D', '\u201E', '\u201F':
			b.WriteRune('"')
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			b.WriteRune('-')
		case '\u00A0', '\u202F', '\u205F', '\u3000':
			b.WriteRune(' ')
		default:
			if r >= '\u2002' && r <= '\u200A' {
				b.WriteRune(' ')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func fuzzyFindText(content string, oldText string) fuzzyMatchResult {
	exactIndex := strings.Index(content, oldText)
	if exactIndex != -1 {
		return fuzzyMatchResult{
			found:                 true,
			index:                 exactIndex,
			matchLength:           len(oldText),
			usedFuzzyMatch:        false,
			contentForReplacement: content,
		}
	}

	fuzzyContent := normalizeForFuzzyMatch(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	if fuzzyOldText == "" {
		return fuzzyMatchResult{index: -1}
	}
	fuzzyIndex := strings.Index(fuzzyContent, fuzzyOldText)

	if fuzzyIndex == -1 {
		return fuzzyMatchResult{
			found:                 false,
			index:                 -1,
			matchLength:           0,
			usedFuzzyMatch:        false,
			contentForReplacement: content,
		}
	}

	return fuzzyMatchResult{
		found:                 true,
		index:                 fuzzyIndex,
		matchLength:           len(fuzzyOldText),
		usedFuzzyMatch:        true,
		contentForReplacement: fuzzyContent,
	}
}

// countOccurrences 统计 oldText 在指定基准空间中的出现次数（覆盖重叠位置）。
func countOccurrences(content, oldText string, isFuzzy bool) int {
	var targetContent, targetPattern string
	if isFuzzy {
		targetContent = normalizeForFuzzyMatch(content)
		targetPattern = normalizeForFuzzyMatch(oldText)
	} else {
		targetContent = content
		targetPattern = oldText
	}

	patLen := len(targetPattern)
	if patLen == 0 {
		return 0
	}

	count := 0
	for i := 0; i+patLen <= len(targetContent); i++ {
		if targetContent[i:i+patLen] == targetPattern {
			count++
		}
	}
	return count
}

func splitLinesWithEndings(content string) []string {
	if len(content) == 0 {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

func getLineSpans(content string) []lineSpan {
	lines := splitLinesWithEndings(content)
	spans := make([]lineSpan, len(lines))
	offset := 0
	for i, line := range lines {
		spans[i] = lineSpan{start: offset, end: offset + len(line)}
		offset += len(line)
	}
	return spans
}

func getReplacementLineRange(lines []lineSpan, rep textReplacement) (int, int, error) {
	repStart := rep.matchIndex
	repEnd := rep.matchIndex + rep.matchLength

	startLine := -1
	for i, line := range lines {
		if repStart >= line.start && repStart < line.end {
			startLine = i
			break
		}
	}
	if startLine == -1 {
		return 0, 0, errors.New("替换起始位置超出基准内容范围")
	}

	endLine := startLine
	for i := startLine; i < len(lines); i++ {
		line := lines[i]
		if repEnd <= line.end {
			endLine = i + 1
			break
		}
	}
	if repEnd > lines[len(lines)-1].end {
		return 0, 0, errors.New("替换结束位置超出基准内容范围")
	}

	return startLine, endLine, nil
}

func applyReplacements(content string, reps []textReplacement, baseOffset int) string {
	sort.Slice(reps, func(i, j int) bool {
		return reps[i].matchIndex > reps[j].matchIndex
	})

	result := content
	for _, rep := range reps {
		start := rep.matchIndex - baseOffset
		end := start + rep.matchLength
		result = result[:start] + rep.newText + result[end:]
	}
	return result
}

func applyReplacementsPreservingUnchangedLines(originalContent, baseContent string, reps []textReplacement) (string, error) {
	originalLines := splitLinesWithEndings(originalContent)
	baseLines := getLineSpans(baseContent)

	if len(originalLines) != len(baseLines) {
		return "", errors.New("原文件行数与规范化基准行数不一致，无法安全回填未修改行")
	}

	sort.Slice(reps, func(i, j int) bool {
		return reps[i].matchIndex < reps[j].matchIndex
	})

	var groups []replacementGroup
	for _, rep := range reps {
		startLine, endLine, err := getReplacementLineRange(baseLines, rep)
		if err != nil {
			return "", err
		}

		if len(groups) > 0 {
			last := &groups[len(groups)-1]
			if startLine < last.endLine {
				if endLine > last.endLine {
					last.endLine = endLine
				}
				last.replacements = append(last.replacements, rep)
				continue
			}
		}
		groups = append(groups, replacementGroup{
			startLine:    startLine,
			endLine:      endLine,
			replacements: []textReplacement{rep},
		})
	}

	var sb strings.Builder
	originalLineIndex := 0
	for _, group := range groups {
		for i := originalLineIndex; i < group.startLine; i++ {
			sb.WriteString(originalLines[i])
		}

		groupStartOffset := baseLines[group.startLine].start
		groupEndOffset := baseLines[group.endLine-1].end
		groupBaseSlice := baseContent[groupStartOffset:groupEndOffset]

		replaced := applyReplacements(groupBaseSlice, group.replacements, groupStartOffset)
		sb.WriteString(replaced)

		originalLineIndex = group.endLine
	}

	for i := originalLineIndex; i < len(originalLines); i++ {
		sb.WriteString(originalLines[i])
	}

	return sb.String(), nil
}

func generateDiff(path string, oldContent, newContent string) string {
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldContent),
		B:        difflib.SplitLines(newContent),
		FromFile: path,
		ToFile:   path,
		Context:  3,
	}
	diffText, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return ""
	}
	if len(diffText) > maxDiffOutputLength {
		cutoff := maxDiffOutputLength
		for cutoff > 0 && !utf8.RuneStart(diffText[cutoff]) {
			cutoff--
		}
		return diffText[:cutoff] + "\n... [diff truncated]"
	}
	return diffText
}
