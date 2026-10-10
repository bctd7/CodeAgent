package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

const IndexName = "MEMORY.md"

// 本文件负责主题文档的校验、格式与索引生成。

// formatTopic 生成带来源元数据的主题正文。
func formatTopic(summary, kind, body string, ids []string) string {
	description, _ := json.Marshal(summary)
	sources, _ := json.Marshal(ids)
	return "---\ndescription: " + string(description) + "\ntype: " + kind + "\nsources: " + string(sources) + "\n---\n\n" + strings.TrimSpace(body) + "\n"
}

// splitFrontmatter 读取本包生成的 LF 元数据块。
func splitFrontmatter(text string) (header, body string, ok bool) {
	rest, found := strings.CutPrefix(text, "---\n")
	if !found {
		return "", "", false
	}
	return strings.Cut(rest, "\n---\n")
}

// validSummary 检查单行索引摘要。
func validSummary(text string) bool {
	return utf8.ValidString(text) && strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= 200 && !strings.ContainsAny(text, "\r\n") && !strings.Contains(text, "<!--")
}

// 索引由服务根据最终正文生成，模型不能写入任意链接或未保存的正文引用。
func buildIndex(files map[string]string) (string, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		topic, err := ParseTopic(files[name])
		if err != nil {
			return "", fmt.Errorf("longterm: topic %q: %w", name, err)
		}
		fmt.Fprintf(&b, "- [%s](%s) — %s\n", strings.TrimSuffix(name, ".md"), name, topic.Summary)
	}
	return b.String(), nil
}

// prepareEntry 校验条目并生成主题正文。
func (s *Store) prepareEntry(e Entry) (string, error) {
	if err := ValidPath(e.Path); err != nil {
		return "", err
	}
	if e.Path == IndexName {
		return "", os.ErrPermission
	}
	_, _, hasFrontmatter := splitFrontmatter(strings.TrimSpace(e.Content))
	if !validSummary(e.Summary) || !utf8.ValidString(e.Content) || strings.TrimSpace(e.Content) == "" || hasFrontmatter {
		return "", fmt.Errorf("longterm: invalid summary or body")
	}
	if err := validateSourceIDs(e.Sources); err != nil {
		return "", err
	}
	content := formatTopic(e.Summary, e.Type, e.Content, e.Sources)
	if _, err := ParseTopic(content); err != nil {
		return "", err
	}
	if len(content) > s.limits.FileBytes {
		return "", fmt.Errorf("%w: topic size", ErrBudget)
	}
	return content, nil
}

// ParseTopic 解析本包的主题格式；description、type、sources 与正文必须完整。
func ParseTopic(content string) (Entry, error) {
	if !utf8.ValidString(content) {
		return Entry{}, fmt.Errorf("longterm: invalid topic text")
	}
	header, body, ok := splitFrontmatter(content)
	if !ok || strings.TrimSpace(body) == "" {
		return Entry{}, fmt.Errorf("longterm: topic metadata and body required")
	}
	fields := map[string]string{}
	for line := range strings.SplitSeq(header, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found || (key != "description" && key != "type" && key != "sources") {
			return Entry{}, fmt.Errorf("longterm: invalid topic frontmatter field")
		}
		if _, duplicate := fields[key]; duplicate {
			return Entry{}, fmt.Errorf("longterm: duplicate topic frontmatter field")
		}
		fields[key] = value
	}
	var topic Entry
	if json.Unmarshal([]byte(fields["description"]), &topic.Summary) != nil || !validSummary(topic.Summary) {
		return Entry{}, fmt.Errorf("longterm: invalid topic description")
	}
	topic.Type = fields["type"]
	switch topic.Type {
	case "user", "feedback", "project", "reference":
	default:
		return Entry{}, fmt.Errorf("longterm: invalid memory type")
	}
	if err := json.Unmarshal([]byte(fields["sources"]), &topic.Sources); err != nil {
		return Entry{}, fmt.Errorf("longterm: invalid sources: %w", err)
	}
	if err := validateSourceIDs(topic.Sources); err != nil {
		return Entry{}, err
	}
	topic.Content = body
	return topic, nil
}

func validateSourceIDs(ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("longterm: sources required")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !ValidID(id, 256) || seen[id] {
			return fmt.Errorf("longterm: invalid source ID")
		}
		seen[id] = true
	}
	return nil
}

func (s *Store) indexDocument(text string) Document {
	text = strings.TrimSpace(text)
	doc := Document{Path: IndexName, Content: text}
	lines := strings.Split(text, "\n")
	if len(lines) > s.limits.IndexLines {
		doc.Content = strings.Join(lines[:s.limits.IndexLines], "\n")
		doc.Truncated = true
	}
	runes := []rune(doc.Content)
	if len(runes) > s.limits.IndexRunes {
		prefix := string(runes[:s.limits.IndexRunes])
		if newline := strings.LastIndexByte(prefix, '\n'); newline >= 0 {
			doc.Content = prefix[:newline]
		} else {
			doc.Content = ""
		}
		doc.Truncated = true
	}
	return doc
}
