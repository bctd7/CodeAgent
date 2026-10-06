package filechange

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	journalName = "run.json"
	lockName    = "lock"
	pending     = "pending"
	prepared    = "prepared"
	done        = "done"
	tempPrefix  = ".filechange-"
)

type journal struct {
	Version int     `json:"version"`
	Run     Run     `json:"run"`
	RootID  string  `json:"root_id"`
	Started bool    `json:"started"`
	Files   []entry `json:"files"`
}

type entry struct {
	Path      string      `json:"path"`
	Before    Snapshot    `json:"before"`
	BeforeSum fingerprint `json:"before_sum"`
	After     fingerprint `json:"after"`
	AfterID   string      `json:"after_id"`
	Phase     string      `json:"phase"`
	Temp      string      `json:"temp,omitempty"`
	TempID    string      `json:"temp_id,omitempty"`
}

func checkJournalNames(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != journalName && entry.Name() != lockName && !strings.HasPrefix(entry.Name(), ".state-") {
			return fmt.Errorf("%w: journal directory contains unrelated files", ErrInvalid)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !plainFile(info) {
			return fmt.Errorf("%w: journal contains a non-plain file", ErrInvalid)
		}
	}
	return nil
}

func (s *Store) load(ctx context.Context) (*journal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.checkRoots(); err != nil {
		return nil, err
	}
	if err := checkLeaf(s.journal, journalName, true); err != nil {
		return nil, err
	}
	f, err := s.journal.OpenFile(journalName, readFlags(), 0)
	if os.IsNotExist(err) {
		return &journal{Version: 1, Run: s.run, RootID: s.rootID}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := verifyOpened(s.journal, journalName, f); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(contextReader{ctx, f})
	decoder.DisallowUnknownFields()
	var doc journal
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: decode journal: %w", ErrInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing journal data", ErrInvalid)
	}
	if err := verifyOpened(s.journal, journalName, f); err != nil {
		return nil, err
	}
	if err := s.validate(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func (s *Store) validate(doc *journal) error {
	if doc.Version != 1 || doc.Run != s.run || doc.RootID != s.rootID {
		return fmt.Errorf("%w: journal scope, root identity or version mismatch", ErrInvalid)
	}
	paths, identities := make(map[string]bool), make(map[string]bool)
	for _, f := range doc.Files {
		path, err := relativePath(s.run.ProjectRoot, f.Path)
		if err != nil || path != f.Path || paths[pathKey(path)] || f.AfterID == "" || identities[f.AfterID] {
			return fmt.Errorf("%w: invalid or duplicate journal path/identity", ErrInvalid)
		}
		paths[pathKey(path)], identities[f.AfterID] = true, true
		before, err := normalizeSnapshot(f.Before)
		if err != nil || before.Mode != f.Before.Mode || summarize(before) != f.BeforeSum || !validFingerprint(f.After) || !f.After.Exists {
			return fmt.Errorf("%w: invalid file snapshot", ErrInvalid)
		}
		if f.Phase != pending && f.Phase != prepared && f.Phase != done || !doc.Started && f.Phase != pending {
			return fmt.Errorf("%w: invalid undo phase", ErrInvalid)
		}
		if f.Phase == prepared && f.Before.Exists {
			if !strings.HasPrefix(f.Temp, tempPrefix) || filepath.Base(f.Temp) != f.Temp || f.TempID == "" || f.Temp == filepath.Base(f.Path) {
				return fmt.Errorf("%w: invalid prepared snapshot", ErrInvalid)
			}
		} else if f.Temp != "" || f.TempID != "" {
			return fmt.Errorf("%w: unexpected prepared snapshot", ErrInvalid)
		}
	}
	return nil
}

func validFingerprint(f fingerprint) bool {
	if !f.Exists {
		return f == (fingerprint{})
	}
	hash, err := hex.DecodeString(f.SHA256)
	return err == nil && len(hash) == 32 && f.Size >= 0 && f.Mode & ^os.ModePerm == 0 && platformMode(f.Mode) == f.Mode
}

func createTemp(root *os.Root, prefix string) (*os.File, string, error) {
	for range 4 {
		name := prefix + rand.Text()
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if os.IsExist(err) {
			continue
		}
		return f, name, err
	}
	return nil, "", fmt.Errorf("%w: temporary name collision", ErrInvalid)
}

// save 先写完整同目录临时日志并 Sync，再替换正式日志；落盘失败一律返回错误。
// 替换后 Sync 失败时新日志可能已可见；下一次操作必须重新 load，不能沿用旧内存。
func (s *Store) save(ctx context.Context, doc *journal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkRoots(); err != nil {
		return err
	}
	if err := s.validate(doc); err != nil {
		return err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	f, name, err := createTemp(s.journal, ".state-")
	if err != nil {
		return err
	}
	id, _, idErr := fileIdentity(f)
	if idErr != nil {
		f.Close()
		return idErr
	}
	defer func() { _ = removeFile(s.journal, name, id) }()
	if err = privateFile(f); err == nil {
		_, err = io.Copy(f, contextReader{ctx, bytes.NewReader(data)})
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = checkLeaf(s.journal, journalName, true); err != nil {
		return err
	}
	if err = s.journal.Rename(name, journalName); err != nil {
		return err
	}
	return syncDirectory(s.journal)
}
