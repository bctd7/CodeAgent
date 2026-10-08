package longterm

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"
)

func (s *Service) ReadIndex(ctx context.Context) (Document, error) {
	if err := s.lock(ctx); err != nil {
		return Document{}, err
	}
	defer s.unlock()
	text, err := s.read(indexName, 8*1024*1024)
	if os.IsNotExist(err) {
		return Document{Path: indexName}, nil
	}
	if err != nil {
		return Document{}, err
	}
	return s.indexDocument(text), nil
}

func (s *Service) indexDocument(text string) Document {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	_, text, _ = splitFrontmatter(text)
	// 索引不包含代码正文，移除注释后再应用行数和 Unicode 字符预算。
	for {
		start := strings.Index(text, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(text[start+4:], "-->")
		if end < 0 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+4+end+3:]
	}
	text = strings.TrimSpace(text)
	doc := Document{Path: indexName, Content: text}
	lines := strings.Split(text, "\n")
	if len(lines) > s.options.Limits.IndexLines {
		doc.Content = strings.Join(lines[:s.options.Limits.IndexLines], "\n")
		doc.Truncated = true
	}
	runes := []rune(doc.Content)
	if len(runes) > s.options.Limits.IndexRunes {
		prefix := string(runes[:s.options.Limits.IndexRunes])
		if newline := strings.LastIndexByte(prefix, '\n'); newline >= 0 {
			doc.Content = prefix[:newline]
		} else {
			doc.Content = ""
		}
		doc.Truncated = true
	}
	return doc
}

func (s *Service) Read(ctx context.Context, path string) (Document, error) {
	if path == indexName {
		return s.ReadIndex(ctx)
	}
	if err := validPath(path); err != nil {
		return Document{}, err
	}
	if err := s.lock(ctx); err != nil {
		return Document{}, err
	}
	defer s.unlock()
	text, err := s.read(path, s.options.Limits.FileBytes)
	return Document{Path: path, Content: text}, err
}

func (s *Service) Search(ctx context.Context, query string, limit int) ([]Match, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	names, err := s.topicNames()
	if err != nil {
		return nil, err
	}
	return search(ctx, names, query, limit, func(name string) (string, error) { return s.read(name, s.options.Limits.FileBytes) })
}

func search(ctx context.Context, names []string, query string, limit int, read func(string) (string, error)) ([]Match, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query = strings.ToLower(strings.TrimSpace(query))
	matches := []Match{}
	if query == "" {
		return matches, nil
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, err := read(name)
		if err != nil {
			return nil, err
		}
		lineNumber := 0
		for line := range strings.SplitSeq(text, "\n") {
			lineNumber++
			if strings.Contains(strings.ToLower(line), query) {
				if utf8.RuneCountInString(line) > 1000 {
					line = string([]rune(line)[:1000])
				}
				matches = append(matches, Match{Path: name, Line: lineNumber, Text: line})
				if len(matches) >= limit {
					return matches, nil
				}
			}
		}
	}
	return matches, nil
}

// Put、Forget 与整理循环共用写入和索引流程，无模型也能管理记忆。
func (s *Service) Put(ctx context.Context, entry Entry) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	content, err := s.prepareEntry(entry)
	if err != nil {
		return err
	}
	state, err := s.readState()
	if err != nil {
		return err
	}
	_, err = s.commit(ctx, map[string]*string{entry.Path: &content}, state)
	return err
}

func (s *Service) Forget(ctx context.Context, path string) error {
	if err := validPath(path); err != nil {
		return err
	}
	if path == indexName {
		return os.ErrPermission
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	state, err := s.readState()
	if err != nil {
		return err
	}
	_, err = s.commit(ctx, map[string]*string{path: nil}, state)
	return err
}

func (s *Service) Cursor(ctx context.Context) (string, error) {
	if err := s.lock(ctx); err != nil {
		return "", err
	}
	defer s.unlock()
	state, err := s.readState()
	return state.Cursor, err
}
