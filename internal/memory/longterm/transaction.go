package longterm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// 一个有界目标日志，重放不依赖旧文件或模型。外部直接读文件不受锁保护。
type pendingCommit struct {
	Version int               `json:"version"`
	Topics  map[string]string `json:"topics"`
	Deletes []string          `json:"deletes"`
	Index   string            `json:"index"`
	State   diskState         `json:"state"`
}

func (s *Service) validatePending(p pendingCommit) error {
	if p.Version != 1 || p.Topics == nil || p.Deletes == nil {
		return fmt.Errorf("longterm: invalid pending structure")
	}
	if len(p.Topics) > s.options.Limits.Files {
		return fmt.Errorf("%w: topic count", ErrBudget)
	}
	for name, text := range p.Topics {
		if err := validPath(name); err != nil {
			return err
		}
		if name == indexName || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
			return fmt.Errorf("longterm: invalid pending topic")
		}
		if len(text) > s.options.Limits.FileBytes {
			return fmt.Errorf("%w: topic size", ErrBudget)
		}
	}
	seen := map[string]bool{}
	for _, name := range p.Deletes {
		if err := validPath(name); err != nil {
			return err
		}
		_, exists := p.Topics[name]
		if name == indexName || exists || seen[name] {
			return fmt.Errorf("longterm: invalid pending deletion")
		}
		seen[name] = true
	}
	if p.Index != buildIndex(p.Topics) {
		return fmt.Errorf("longterm: pending index differs from topics")
	}
	lines := 0
	if p.Index != "" {
		lines = strings.Count(strings.TrimSuffix(p.Index, "\n"), "\n") + 1
	}
	if lines > s.options.Limits.IndexLines || utf8.RuneCountInString(p.Index) > s.options.Limits.IndexRunes || len(p.Index) > stateLimit {
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

func (s *Service) commit(ctx context.Context, staged map[string]*string, state diskState) (bool, error) {
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
	p := pendingCommit{Version: 1, Topics: files, Deletes: []string{}, State: state}
	changed := false
	for name, text := range staged {
		if err := validPath(name); err != nil {
			return false, err
		}
		if name == indexName {
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
	p.Index = buildIndex(files)
	previous, err := s.read(indexName, stateLimit)
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

func (s *Service) applyPending(p pendingCommit) error {
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
	if err := s.atomicWrite(ctx, indexName, p.Index); err != nil {
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

func (s *Service) recoverPending() error {
	text, err := s.read(pendingName, pendingLimit)
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
