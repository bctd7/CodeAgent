package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 本文件负责记忆目录、受限文件访问、批次状态和提交。

const indexName = "MEMORY.md"
const stateName = ".memory-state.json"

type diskState struct {
	Cursor  string            `json:"cursor"`
	Batches map[string]string `json:"batches"`
}

// 仅允许根目录中的普通 Markdown 文件，拒绝 Windows ADS、设备名和路径穿越。
func validPath(name string) error {
	if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || len(name) > 160 {
		return fmt.Errorf("memory: invalid Markdown path %q", name)
	}
	stem := strings.TrimSuffix(name, ".md")
	if stem == "" {
		return fmt.Errorf("memory: empty filename")
	}
	if name != indexName && stem != strings.ToLower(stem) {
		return fmt.Errorf("memory: topic filename must be lowercase")
	}
	for _, r := range stem {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return fmt.Errorf("memory: invalid filename %q", name)
		}
	}
	if strings.EqualFold(name, indexName) && name != indexName {
		return fmt.Errorf("memory: reserved index name")
	}
	return nil
}

func (s *Service) read(name string, limit int) (string, error) {
	info, err := s.root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("memory: not a regular file %q", name)
	}
	if info.Size() > int64(limit) {
		return "", fmt.Errorf("%w: file %q", ErrBudget, name)
	}
	f, err := s.root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() {
		return "", fmt.Errorf("memory: not a regular file %q", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", fmt.Errorf("%w: file %q", ErrBudget, name)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("memory: invalid UTF-8 in %q", name)
	}
	return string(b), nil
}

func (s *Service) topicNames() ([]string, error) {
	f, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if e.Name() == indexName || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if err := validPath(e.Name()); err != nil {
			return nil, err
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("memory: non-regular topic %q", e.Name())
		}
		names = append(names, e.Name())
		if len(names) > s.options.Limits.Files {
			return nil, fmt.Errorf("%w: topic count", ErrBudget)
		}
	}
	sort.Strings(names)
	return names, nil
}

// 单文件临时写入并重命名；不通过符号链接覆盖文件。
func (s *Service) atomicWrite(name, content string) error {
	if info, err := s.root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("memory: non-regular destination %q", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".memory-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	_, err = io.WriteString(f, content)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return s.root.Rename(tmp, name)
}

func (s *Service) readState() (diskState, error) {
	text, err := s.read(stateName, 8*1024*1024)
	if os.IsNotExist(err) {
		return diskState{Batches: map[string]string{}}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	var state diskState
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		return state, fmt.Errorf("memory: invalid batch metadata: %w", err)
	}
	if state.Batches == nil {
		state.Batches = map[string]string{}
	}
	return state, nil
}

func (s *Service) commit(ctx context.Context, staged map[string]*string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	names, err := s.topicNames()
	if err != nil {
		return false, err
	}
	files := make(map[string]string, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		text, err := s.read(name, s.options.Limits.FileBytes)
		if err != nil {
			return false, err
		}
		files[name] = text
	}
	writes := []string{}
	deletes := []string{}
	for name, content := range staged {
		old, exists := files[name]
		if content == nil {
			if exists {
				delete(files, name)
				deletes = append(deletes, name)
			}
			continue
		}
		files[name] = *content
		if !exists || old != *content {
			writes = append(writes, name)
		}
	}
	if len(files) > s.options.Limits.Files {
		return false, fmt.Errorf("%w: topic count", ErrBudget)
	}
	index := buildIndex(files)
	if len(strings.Split(strings.TrimSpace(index), "\n")) > s.options.Limits.IndexLines || utf8.RuneCountInString(index) > s.options.Limits.IndexRunes {
		return false, fmt.Errorf("%w: index; merge related topics", ErrBudget)
	}
	previous, err := s.read(indexName, 8*1024*1024)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	// 重试可能遇到上次已经保存的正文，但尚未保存索引；即使正文无变化也修复索引。
	indexChanged := previous != index || (os.IsNotExist(err) && len(files) > 0)
	if len(writes) == 0 && len(deletes) == 0 && !indexChanged {
		return false, nil
	}
	sort.Strings(writes)
	sort.Strings(deletes)
	// 写正文之前检查取消。一旦落盘开始便完成索引与删除顺序，防止取消留下悬空链接。
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, name := range writes {
		if err := s.atomicWrite(name, files[name]); err != nil {
			return false, err
		}
	}
	if indexChanged {
		if err := s.atomicWrite(indexName, index); err != nil {
			return false, err
		}
	}
	for _, name := range deletes {
		if err := s.root.Remove(name); err != nil {
			return false, err
		}
	}
	return true, nil
}
