package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	stateName    = ".memory-state.json"
	pendingName  = ".memory-pending.json"
	stateLimit   = 8 * 1024 * 1024
	pendingLimit = 32 * 1024 * 1024
)

type BatchResult struct {
	Changed, Duplicate bool
}

// Transaction 是锁内的暂存视图，只在 UpdateBatch 回调期间使用。
// 调用方串行操作，不保留给后台，也不重入同目录 Store 的公开方法。
type Transaction struct {
	store  *Store
	names  []string
	staged map[string]*string
	cache  map[string]string
}

// UpdateBatch 管理目录锁、恢复、去重、暂存和提交。回调失败时不发布本批修改。
func (s *Store) UpdateBatch(ctx context.Context, id string, apply func(*Transaction) error) (BatchResult, error) {
	if !ValidID(id, 256) {
		return BatchResult{}, fmt.Errorf("longterm: invalid batch ID")
	}
	if ctx == nil {
		return BatchResult{}, fmt.Errorf("longterm: nil context")
	}
	if err := ctx.Err(); err != nil {
		return BatchResult{}, err
	}
	// 固定顺序 writer -> data；前台读取仅使用 data。
	if err := acquire(ctx, s.locks.writer); err != nil {
		return BatchResult{}, err
	}
	defer func() { <-s.locks.writer }()
	var state diskState
	err := s.committed(ctx, func(_ *Transaction) error {
		var e error
		state, e = s.readState()
		return e
	})
	if err != nil {
		return BatchResult{}, err
	}
	if state.Receipts[id] {
		return BatchResult{Duplicate: true}, nil
	}
	tx := &Transaction{store: s, staged: map[string]*string{}, cache: map[string]string{}}
	if err := apply(tx); err != nil {
		return BatchResult{}, err
	}
	state.Receipts[id] = true
	if err := acquire(ctx, s.locks.data); err != nil {
		return BatchResult{}, err
	}
	defer func() { <-s.locks.data }()
	changed, err := s.commit(ctx, tx.staged, state)
	if err != nil {
		return BatchResult{}, err
	}
	return BatchResult{Changed: changed}, nil
}

// ReadCommittedIndex 读取已提交索引，用于准备初始任务。
// Read(IndexName) 则根据本次暂存内容生成索引。
func (v *Transaction) ReadCommittedIndex() (Document, error) {
	text, err := v.store.readFile(IndexName, stateLimit)
	if os.IsNotExist(err) {
		return Document{Path: IndexName}, nil
	}
	if err != nil {
		return Document{}, err
	}
	return v.store.indexDocument(text), nil
}

func (v *Transaction) Read(name string) (Document, error) {
	text, err := v.readContent(name)
	if err != nil {
		return Document{}, err
	}
	if name == IndexName {
		return v.store.indexDocument(text), nil
	}
	return Document{Path: name, Content: text}, nil
}

func (v *Transaction) Files() ([]string, error) {
	if v.names == nil {
		names, err := v.store.topicNames()
		if err != nil {
			return nil, err
		}
		v.names = names
	}
	set := map[string]bool{}
	for _, name := range v.names {
		set[name] = true
	}
	for name, text := range v.staged {
		if text == nil {
			delete(set, name)
		} else {
			set[name] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (v *Transaction) Search(ctx context.Context, query string, limit int) ([]Match, error) {
	names, err := v.Files()
	if err != nil {
		return nil, err
	}
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
		text, err := v.readContent(name)
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

func (v *Transaction) Put(entry Entry) error {
	content, err := v.store.prepareEntry(entry)
	if err != nil {
		return err
	}
	v.staged[entry.Path] = &content
	names, err := v.Files()
	if err != nil {
		return err
	}
	if len(names) > v.store.limits.Files {
		return fmt.Errorf("%w: topic count", ErrBudget)
	}
	return nil
}

func (v *Transaction) Forget(path string) error {
	if err := ValidPath(path); err != nil {
		return err
	}
	if path == IndexName {
		return os.ErrPermission
	}
	v.staged[path] = nil
	return nil
}

// diskState 只登记成功批次，不保存来源哈希或会话进度。
type diskState struct {
	Version  int             `json:"version"`
	Receipts map[string]bool `json:"receipts"`
}

func decodeJSON(text string, out any) error {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("longterm: trailing JSON data")
	}
	return nil
}

func validateState(state diskState) error {
	if state.Version != 3 || state.Receipts == nil {
		return fmt.Errorf("longterm: invalid batch state")
	}
	for id, completed := range state.Receipts {
		if !ValidID(id, 256) || !completed {
			return fmt.Errorf("longterm: invalid batch receipt")
		}
	}
	return nil
}

func (s *Store) readState() (diskState, error) {
	text, err := s.readFile(stateName, stateLimit)
	if os.IsNotExist(err) {
		return diskState{Version: 3, Receipts: map[string]bool{}}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	var state diskState
	if err := decodeJSON(text, &state); err != nil {
		return state, fmt.Errorf("longterm: invalid batch metadata: %w", err)
	}
	if err := validateState(state); err != nil {
		return state, err
	}
	return state, nil
}

func (v *Transaction) readCachedTopic(name string) (string, error) {
	if text, ok := v.cache[name]; ok {
		return text, nil
	}
	text, err := v.store.readFile(name, v.store.limits.FileBytes)
	if err != nil {
		return "", err
	}
	if _, err := ParseTopic(text); err != nil {
		return "", fmt.Errorf("longterm: topic %q: %w", name, err)
	}
	v.cache[name] = text
	return text, nil
}

func (v *Transaction) readContent(name string) (string, error) {
	if err := ValidPath(name); err != nil {
		return "", err
	}
	if text, exists := v.staged[name]; exists {
		if text == nil {
			return "", os.ErrNotExist
		}
		return *text, nil
	}
	if name == IndexName {
		names, err := v.Files()
		if err != nil {
			return "", err
		}
		files := make(map[string]string, len(names))
		for _, topicName := range names {
			text, err := v.readContent(topicName)
			if err != nil {
				return "", err
			}
			files[topicName] = text
		}
		return buildIndex(files)
	}
	return v.readCachedTopic(name)
}

// 一个有界目标日志，重放不依赖旧文件或模型。外部直接读文件不受锁保护。
type pendingCommit struct {
	Version int               `json:"version"`
	Topics  map[string]string `json:"topics"`
	Deletes []string          `json:"deletes"`
	Index   string            `json:"index"`
	State   diskState         `json:"state"`
}

func (s *Store) validatePending(p pendingCommit) error {
	if p.Version != 1 || p.Topics == nil || p.Deletes == nil {
		return fmt.Errorf("longterm: invalid pending structure")
	}
	if len(p.Topics) > s.limits.Files {
		return fmt.Errorf("%w: topic count", ErrBudget)
	}
	for name, text := range p.Topics {
		if err := ValidPath(name); err != nil {
			return err
		}
		if name == IndexName {
			return fmt.Errorf("longterm: invalid pending topic")
		}
		if len(text) > s.limits.FileBytes {
			return fmt.Errorf("%w: topic size", ErrBudget)
		}
	}
	seen := map[string]bool{}
	for _, name := range p.Deletes {
		if err := ValidPath(name); err != nil {
			return err
		}
		_, exists := p.Topics[name]
		if name == IndexName || exists || seen[name] {
			return fmt.Errorf("longterm: invalid pending deletion")
		}
		seen[name] = true
	}
	index, err := buildIndex(p.Topics)
	if err != nil {
		return err
	}
	if p.Index != index {
		return fmt.Errorf("longterm: pending index differs from topics")
	}
	lines := 0
	if p.Index != "" {
		lines = strings.Count(strings.TrimSuffix(p.Index, "\n"), "\n") + 1
	}
	if lines > s.limits.IndexLines || utf8.RuneCountInString(p.Index) > s.limits.IndexRunes || len(p.Index) > stateLimit {
		return fmt.Errorf("%w: index", ErrBudget)
	}
	if err := validateState(p.State); err != nil {
		return err
	}
	state, err := json.Marshal(p.State)
	if err != nil {
		return err
	}
	if len(state)+1 > stateLimit {
		return fmt.Errorf("%w: batch metadata", ErrBudget)
	}
	return nil
}

func (s *Store) commit(ctx context.Context, staged map[string]*string, state diskState) (bool, error) {
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
		text, err := s.readFile(name, s.limits.FileBytes)
		if err != nil {
			return false, err
		}
		files[name] = text
	}
	p := pendingCommit{Version: 1, Topics: files, Deletes: []string{}, State: state}
	changed := false
	for name, text := range staged {
		if err := ValidPath(name); err != nil {
			return false, err
		}
		if name == IndexName {
			return false, os.ErrPermission
		}
		old, exists := files[name]
		if text == nil {
			if exists {
				delete(files, name)
				p.Deletes = append(p.Deletes, name)
				changed = true
			}
		} else {
			files[name] = *text
			if !exists || old != *text {
				changed = true
			}
		}
	}
	p.Index, err = buildIndex(files)
	if err != nil {
		return false, err
	}
	previous, err := s.readFile(IndexName, stateLimit)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if previous != p.Index {
		changed = true
	}
	if err := s.validatePending(p); err != nil {
		return false, err
	}
	sort.Strings(p.Deletes)
	encoded, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	if len(encoded)+1 > pendingLimit {
		return false, fmt.Errorf("%w: pending log", ErrBudget)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.atomicWrite(ctx, pendingName, string(encoded)+"\n"); err != nil {
		return false, err
	}
	// 从发布点起尽量完成，失败保留日志，不声称回滚或成功。
	if err := s.applyPending(p); err != nil {
		return false, fmt.Errorf("%w: %w", ErrCommitPending, err)
	}
	return changed, nil
}

func (s *Store) applyPending(p pendingCommit) error {
	names := make([]string, 0, len(p.Topics))
	for name := range p.Topics {
		names = append(names, name)
	}
	sort.Strings(names)
	ctx := context.Background()
	for _, name := range names {
		if err := s.atomicWrite(ctx, name, p.Topics[name]); err != nil {
			return err
		}
	}
	if err := s.atomicWrite(ctx, IndexName, p.Index); err != nil {
		return err
	}
	for _, name := range p.Deletes {
		if err := s.removeRegular(name); err != nil {
			return err
		}
	}
	state, err := json.Marshal(p.State)
	if err != nil {
		return err
	}
	if err := s.atomicWrite(ctx, stateName, string(state)+"\n"); err != nil {
		return err
	}
	return s.removeRegular(pendingName)
}

func (s *Store) recoverPending() error {
	text, err := s.readFile(pendingName, pendingLimit)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCommitPending, err)
	}
	var pending pendingCommit
	if err := decodeJSON(text, &pending); err != nil {
		return fmt.Errorf("%w: invalid log: %w", ErrCommitPending, err)
	}
	if err := s.validatePending(pending); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitPending, err)
	}
	if err := s.applyPending(pending); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitPending, err)
	}
	return nil
}
