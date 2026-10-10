// Package filechange 保存可信调用方显式记录的一轮 Run 的 edit/write 文件变化。
// 本包不发现工具调用、不修改会话历史，也不撤销 shell 或 MCP 的外部效果。
// 调用方必须串行协调记录与实际写入，并在 Run 停止写文件后调用 Undo。
// 哈希只表示内容条件，不证明修改者身份；路径检查不能消除所有 TOCTOU。
package filechange

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrConflict  = errors.New("filechange: conflict")
	ErrChain     = errors.New("filechange: discontinuous change")
	ErrSealed    = errors.New("filechange: undo already started")
	ErrBusy      = errors.New("filechange: journal is in use")
	ErrClosed    = errors.New("filechange: store is closed")
	ErrInvalid   = errors.New("filechange: invalid input or journal")
	ErrNoChanges = errors.New("filechange: no recorded changes")
)

// Run 由可信调用方提供；一个日志目录只绑定一个 Run 和一个项目目录身份。
type Run struct {
	SessionID   string `json:"session_id"`
	RunID       string `json:"run_id"`
	ProjectRoot string `json:"project_root"`
}

// Snapshot 区分不存在与空文件。Mode 只接收权限位；不保留 ACL、属主或时间戳。
// Windows 将权限归一为可写 0666 或只读 0444，与 os.FileInfo 一致。
// Unix 必须保留属主读权限，确保进程重启后仍能验证恢复文件。
type Snapshot struct {
	Exists bool        `json:"exists"`
	Data   []byte      `json:"data,omitempty"`
	Mode   fs.FileMode `json:"mode"`
}

// Change 只描述已成功完成的普通文件写入，After 必须存在；Path 可为根内绝对路径或相对路径。
type Change struct {
	Path   string
	Before Snapshot
	After  Snapshot
}

type Action struct {
	Path     string
	Kind     string // restore 或 remove。
	Recovery bool   // 文件效果已确认，只需补写完成日志。
}

type Failure struct {
	Path    string
	Stage   string
	Applied bool // 文件效果已发生，但其后落盘或验证失败（也包括重开后的恢复）。
	Err     error
}

type Plan struct {
	Actions       []Action
	Completed     []string
	Conflicts     []Failure
	AlreadyUndone bool
}

// UndoResult 不表示跨文件原子性。失败后检查三个列表，再对同一日志重试。
// 完成日志失败的文件放在 Failed 中，Applied=true；不虚报为持久化完成。
type UndoResult struct {
	Completed     []string
	Failed        []Failure
	Pending       []string
	AlreadyUndone bool
}

// Store 持有日志的系统锁。不同 Run 可使用不同目录；同一目录只允许一个打开者。
// 每次操作重新读取日志，避免一次提交结果不确定后以内存旧状态覆盖磁盘。
type Store struct {
	gate        chan struct{}
	run         Run
	rootID      string
	project     *os.Root
	journal     *os.Root
	lock        *os.File
	projectInfo os.FileInfo
	journalInfo os.FileInfo
	closed      bool
}

// Open 创建或重新打开调用方指定的独立日志目录。父目录须存在；项目与日志不得互相包含。
// 目录只能包含本包的日志文件，权限被收紧为当前用户私有（Windows 另允许 SYSTEM）。
func Open(ctx context.Context, directory string, run Run) (_ *Store, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(run.SessionID) == "" || strings.TrimSpace(run.RunID) == "" || run.ProjectRoot == "" || directory == "" {
		return nil, ErrInvalid
	}
	run.ProjectRoot, err = filepath.Abs(run.ProjectRoot)
	if err != nil {
		return nil, err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if containsPath(run.ProjectRoot, directory) || containsPath(directory, run.ProjectRoot) {
		return nil, fmt.Errorf("%w: journal and project overlap", ErrInvalid)
	}
	if err = checkDirectoryPath(run.ProjectRoot); err != nil {
		return nil, err
	}
	if err = checkDirectoryPath(filepath.Dir(directory)); err != nil {
		return nil, err
	}
	if err = separateDirectories(run.ProjectRoot, directory); err != nil {
		return nil, err
	}
	if info, e := os.Lstat(directory); e == nil {
		if !plainDirectory(info) {
			return nil, fmt.Errorf("%w: journal is not a plain directory", ErrInvalid)
		}
		if err = checkJournalNames(directory); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if err = privateDirectory(directory); err != nil {
		return nil, err
	}
	s := &Store{gate: make(chan struct{}, 1), run: run}
	defer func() {
		if err != nil {
			s.release()
		}
	}()
	if s.project, err = os.OpenRoot(run.ProjectRoot); err != nil {
		return nil, err
	}
	if s.journal, err = os.OpenRoot(directory); err != nil {
		return nil, err
	}
	if s.projectInfo, err = s.project.Stat("."); err != nil {
		return nil, err
	}
	if s.journalInfo, err = s.journal.Stat("."); err != nil {
		return nil, err
	}
	if err = s.checkRoots(); err != nil {
		return nil, err
	}
	dirFile, err := s.project.Open(".")
	if err != nil {
		return nil, err
	}
	s.rootID, _, err = fileIdentity(dirFile)
	err = errors.Join(err, dirFile.Close())
	if err != nil {
		return nil, err
	}
	if err = checkLeaf(s.journal, lockName, true); err != nil {
		return nil, err
	}
	if s.lock, err = s.journal.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0600); err != nil {
		return nil, err
	}
	if err = verifyOpened(s.journal, lockName, s.lock); err != nil {
		return nil, err
	}
	if err = lockJournal(s.lock); err != nil {
		return nil, err
	}
	if err = privateFile(s.lock); err != nil {
		return nil, err
	}
	if _, err = s.load(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Close 释放系统锁；不会删除锁文件或日志。等待正在进行的操作结束。
func (s *Store) Close() error {
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.release()
}

func (s *Store) release() error {
	var err error
	if s.project != nil {
		err = errors.Join(err, s.project.Close())
	}
	if s.journal != nil {
		err = errors.Join(err, s.journal.Close())
	}
	if s.lock != nil {
		err = errors.Join(err, s.lock.Close())
	}
	return err
}

func (s *Store) enter(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-s.gate
		return err
	}
	if s.closed {
		<-s.gate
		return ErrClosed
	}
	return nil
}

// Record 仅在日志成功保存后返回成功。连续记录须满足上次 after == 本次 before。
// 调用方须在写入前取得 before，在成功写入后立即记录；本包不补记未记录的修改。
func (s *Store) Record(ctx context.Context, change Change) error {
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	doc, err := s.load(ctx)
	if err != nil {
		return err
	}
	if doc.Started {
		return ErrSealed
	}
	path, err := relativePath(s.run.ProjectRoot, change.Path)
	if err != nil {
		return err
	}
	before, err := normalizeSnapshot(change.Before)
	if err != nil {
		return err
	}
	after, err := normalizeSnapshot(change.After)
	if err != nil || !after.Exists {
		return fmt.Errorf("%w: after must be a regular file snapshot", ErrInvalid)
	}
	actual, id, err := observe(ctx, s.project, path)
	if err != nil {
		return err
	}
	if actual != summarize(after) {
		return fmt.Errorf("%w: %s no longer matches after", ErrConflict, path)
	}
	matched := -1
	for i := range doc.Files {
		f := &doc.Files[i]
		if pathKey(f.Path) == pathKey(path) {
			if f.Path != path {
				previousID, err := identityAt(s.project, f.Path)
				if err != nil || previousID != id {
					return fmt.Errorf("%w: ambiguous case variant %s", ErrInvalid, path)
				}
			}
			if f.After != summarize(before) {
				return fmt.Errorf("%w: %s", ErrChain, path)
			}
			matched = i
			continue
		}
		// 替换写可能已改变 inode；旧路径当前身份也必须参与别名判断。
		previousID, err := identityAt(s.project, f.Path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: inspect prior path %s: %w", ErrConflict, f.Path, err)
		}
		if f.AfterID == id || previousID == id {
			return fmt.Errorf("%w: %s aliases %s", ErrInvalid, path, f.Path)
		}
	}
	if matched >= 0 {
		doc.Files[matched].After, doc.Files[matched].AfterID = actual, id
		return s.save(ctx, doc)
	}
	doc.Files = append(doc.Files, entry{Path: path, Before: before, BeforeSum: summarize(before), After: actual, AfterID: id, Phase: pending})
	return s.save(ctx, doc)
}
