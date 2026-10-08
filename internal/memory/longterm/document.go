package longterm

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// 本文件负责主题文档的校验、格式与索引生成。

// formatTopic 生成带来源元数据的主题正文。
func formatTopic(summary, kind, body string, ids []string) string {
	description, _ := json.Marshal(summary)
	sources, _ := json.Marshal(ids)
	return "---\ndescription: " + string(description) + "\ntype: " + kind + "\nsources: " + string(sources) + "\n---\n\n" + strings.TrimSpace(body) + "\n"
}

// description 读取主题摘要，缺失时使用文件名。
func description(name, content string) string {
	if header, _, ok := splitFrontmatter(content); ok {
		for line := range strings.SplitSeq(header, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if value, ok := strings.CutPrefix(line, "description: "); ok {
				var summary string
				if json.Unmarshal([]byte(value), &summary) == nil && validSummary(summary) {
					return summary
				}
			}
		}
	}
	return strings.TrimSuffix(name, ".md")
}

// 统一识别 LF/CRLF frontmatter，仅剥离完整块，不把正文的分隔线误当元数据。
func splitFrontmatter(text string) (header, body string, ok bool) {
	first, rest, found := strings.Cut(text, "\n")
	if !found || strings.TrimSuffix(first, "\r") != "---" {
		return "", text, false
	}
	start := len(text) - len(rest)
	for offset := start; offset < len(text); {
		line, remaining, newline := strings.Cut(text[offset:], "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			return text[start:offset], remaining, true
		}
		if !newline {
			break
		}
		offset += len(line) + 1
	}
	return "", text, false
}

// validSummary 检查单行索引摘要。
func validSummary(text string) bool {
	return utf8.ValidString(text) && strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= 200 && !strings.ContainsAny(text, "\r\n") && !strings.Contains(text, "<!--")
}

// 索引由服务根据最终正文生成，模型不能写入任意链接或未保存的正文引用。
func buildIndex(files map[string]string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "- [%s](%s) — %s\n", strings.TrimSuffix(name, ".md"), name, description(name, files[name]))
	}
	return b.String()
}

// prepareEntry 校验条目并生成主题正文。
func (s *Service) prepareEntry(e Entry) (string, error) {
	if err := validPath(e.Path); err != nil {
		return "", err
	}
	if e.Path == indexName {
		return "", os.ErrPermission
	}
	_, _, hasFrontmatter := splitFrontmatter(strings.TrimSpace(e.Content))
	if !validSummary(e.Summary) || !utf8.ValidString(e.Content) || strings.TrimSpace(e.Content) == "" || hasFrontmatter {
		return "", fmt.Errorf("longterm: invalid summary or body")
	}
	switch e.Type {
	case "user", "feedback", "project", "reference":
	default:
		return "", fmt.Errorf("longterm: invalid memory type")
	}
	if len(e.Sources) == 0 {
		return "", fmt.Errorf("longterm: sources required")
	}
	seen := map[string]bool{}
	for _, id := range e.Sources {
		if !utf8.ValidString(id) || strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			return "", fmt.Errorf("longterm: invalid source ID")
		}
		seen[id] = true
	}
	content := formatTopic(e.Summary, e.Type, e.Content, e.Sources)
	if len(content) > s.options.Limits.FileBytes {
		return "", fmt.Errorf("%w: topic size", ErrBudget)
	}
	return content, nil
}

// 旧正文无 frontmatter 时仍可读；已有来源必须能安全解析。
func topicSources(content string) ([]string, error) {
	header, _, ok := splitFrontmatter(content)
	if !ok {
		return nil, nil
	}
	for line := range strings.SplitSeq(header, "\n") {
		value, found := strings.CutPrefix(strings.TrimSuffix(line, "\r"), "sources: ")
		if !found {
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(value), &ids); err != nil {
			return nil, fmt.Errorf("longterm: invalid old sources: %w", err)
		}
		for _, id := range ids {
			if !validID(id, 256) {
				return nil, fmt.Errorf("longterm: invalid old source ID")
			}
		}
		return ids, nil
	}
	return nil, nil
}
