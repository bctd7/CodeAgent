package longterm

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

const (
	indexName    = "MEMORY.md"
	stateName    = ".memory-state.json"
	pendingName  = ".memory-pending.json"
	stateLimit   = 8 * 1024 * 1024
	pendingLimit = 32 * 1024 * 1024
)

type batchReceipt struct {
	Digest string `json:"digest"`
	Cursor string `json:"cursor"`
}
type diskState struct {
	Version  int                     `json:"version"`
	Cursor   string                  `json:"cursor"`
	Batches  map[string]string       `json:"batches,omitempty"` // 保留旧摘要标记，不能当作新批重新整理。
	Receipts map[string]batchReceipt `json:"receipts"`
}

// 仅接受根目录普通 Markdown 文件，索引只允许服务生成。
func validPath(name string) error {
	if !utf8.ValidString(name) || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || len(name) > 160 {
		return fmt.Errorf("longterm: invalid Markdown path %q", name)
	}
	stem := strings.TrimSuffix(name, ".md")
	if stem == "" {
		return fmt.Errorf("longterm: empty filename")
	}
	if strings.EqualFold(name, indexName) && name != indexName {
		return fmt.Errorf("longterm: reserved index name")
	}
	if name != indexName && stem != strings.ToLower(stem) {
		return fmt.Errorf("longterm: topic filename must be lowercase")
	}
	upper := strings.ToUpper(stem)
	device := upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" || len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '0' && upper[3] <= '9'
	// Windows 也识别上标 1/2/3 的 COM/LPT 设备名。
	if device || upper == "COM¹" || upper == "COM²" || upper == "COM³" || upper == "LPT¹" || upper == "LPT²" || upper == "LPT³" {
		return fmt.Errorf("longterm: reserved device name")
	}
	for _, r := range stem {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return fmt.Errorf("longterm: invalid filename %q", name)
		}
	}
	return nil
}

func (s *Service) read(name string, limit int) (string, error) {
	info, err := s.root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("longterm: non-regular file %q", name)
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
		return "", fmt.Errorf("longterm: non-regular file %q", name)
	}
	bound := int64(limit)
	if bound < int64(^uint64(0)>>1) {
		bound++
	}
	b, err := io.ReadAll(io.LimitReader(f, bound))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", fmt.Errorf("%w: file %q", ErrBudget, name)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("longterm: invalid UTF-8 in %q", name)
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
	for _, entry := range entries {
		if entry.Name() == indexName || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if err := validPath(entry.Name()); err != nil {
			return nil, err
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("longterm: non-regular topic %q", entry.Name())
		}
		names = append(names, entry.Name())
		if len(names) > s.options.Limits.Files {
			return nil, fmt.Errorf("%w: topic count", ErrBudget)
		}
	}
	sort.Strings(names)
	return names, nil
}

// 临时文件 Sync 后 Rename；发布前检查取消，不沿链接覆盖目标。
func (s *Service) atomicWrite(ctx context.Context, name, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := s.root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("longterm: non-regular destination %q", name)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.root.Rename(tmp, name)
}

func (s *Service) removeRegular(name string) error {
	info, err := s.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("longterm: non-regular deletion %q", name)
	}
	return s.root.Remove(name)
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

func validID(id string, max int) bool {
	return utf8.ValidString(id) && strings.TrimSpace(id) != "" && len(id) <= max
}
func validDigest(digest string) bool {
	b, err := hex.DecodeString(digest)
	return err == nil && len(b) == 32 && digest == strings.ToLower(digest)
}

func validateState(state diskState) error {
	if state.Version != 2 || !utf8.ValidString(state.Cursor) || len(state.Cursor) > 4096 || state.Receipts == nil {
		return fmt.Errorf("longterm: invalid state version or cursor")
	}
	for id, digest := range state.Batches {
		if !validID(id, 256) || !validDigest(digest) {
			return fmt.Errorf("longterm: invalid legacy batch")
		}
	}
	for id, receipt := range state.Receipts {
		if !validID(id, 256) || !validDigest(receipt.Digest) || !utf8.ValidString(receipt.Cursor) || len(receipt.Cursor) > 4096 {
			return fmt.Errorf("longterm: invalid batch receipt")
		}
		if _, exists := state.Batches[id]; exists {
			return fmt.Errorf("longterm: ambiguous batch receipt")
		}
	}
	return nil
}

func (s *Service) readState() (diskState, error) {
	text, err := s.read(stateName, stateLimit)
	if os.IsNotExist(err) {
		return diskState{Version: 2, Receipts: map[string]batchReceipt{}}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	var state diskState
	if err := decodeJSON(text, &state); err != nil {
		return state, fmt.Errorf("longterm: invalid batch metadata: %w", err)
	}
	if state.Version == 0 {
		if state.Receipts != nil || state.Batches == nil {
			return state, fmt.Errorf("longterm: unversioned receipts")
		}
		state.Version = 2
		state.Receipts = map[string]batchReceipt{}
	}
	if err := validateState(state); err != nil {
		return state, err
	}
	return state, nil
}
