package filechange

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Plan 只读取日志和文件；已知任一冲突时返回全部可检查的冲突及 ErrConflict。
// 返回值仅供预览，Undo 会重新检查整批文件，不接受外部传回的旧计划。
func (s *Store) Plan(ctx context.Context) (Plan, error) {
	if err := s.enter(ctx); err != nil {
		return Plan{}, err
	}
	defer func() { <-s.gate }()
	doc, err := s.load(ctx)
	if err != nil {
		return Plan{}, err
	}
	return s.plan(ctx, doc)
}

func (s *Store) plan(ctx context.Context, doc *journal) (Plan, error) {
	var plan Plan
	for _, f := range doc.Files {
		if f.Phase == done {
			plan.Completed = append(plan.Completed, f.Path)
			continue
		}
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		action, err := s.inspect(ctx, f)
		if err != nil {
			plan.Conflicts = append(plan.Conflicts, Failure{Path: f.Path, Stage: "preflight", Err: err})
		} else {
			plan.Actions = append(plan.Actions, action)
		}
	}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if len(plan.Conflicts) != 0 {
		return plan, ErrConflict
	}
	plan.AlreadyUndone = doc.Started && len(doc.Files) != 0 && len(plan.Completed) == len(doc.Files)
	return plan, nil
}

func recovery(f entry, state fingerprint, id string) bool {
	if f.Phase != prepared || state != f.BeforeSum {
		return false
	}
	// 删除后的不存在只能结合持久化 intent 判断，不能据此证明删除者身份。
	return !f.Before.Exists || id == f.TempID
}

func (s *Store) inspect(ctx context.Context, f entry) (Action, error) {
	action := Action{Path: f.Path, Kind: "remove"}
	if f.Before.Exists {
		action.Kind = "restore"
	}
	p, err := openParent(s.project, f.Path)
	if err != nil {
		return action, err
	}
	defer p.Close()
	state, id, err := observeLeaf(ctx, p, filepath.Base(f.Path))
	if err != nil {
		return action, err
	}
	if recovery(f, state, id) {
		action.Recovery = true
		return action, nil
	}
	if state != f.After || id != f.AfterID {
		return action, fmt.Errorf("%w: %s no longer matches recorded after", ErrConflict, f.Path)
	}
	if f.Temp != "" {
		state, id, err = observeLeaf(ctx, p, f.Temp)
		if err != nil {
			return action, err
		}
		if state.Exists && (state != f.BeforeSum || id != f.TempID) {
			return action, fmt.Errorf("%w: prepared file changed for %s", ErrConflict, f.Path)
		}
	}
	return action, nil
}

// Undo 先整批只读预检，再按记录顺序逐文件撤销。遇到第一个执行错误即停止。
// 取消在读写块及文件边界检查；已发生的文件效果会尽量补写完成日志，不自动回滚。
func (s *Store) Undo(ctx context.Context) (UndoResult, error) {
	var result UndoResult
	if err := s.enter(ctx); err != nil {
		return result, err
	}
	defer func() { <-s.gate }()
	doc, err := s.load(ctx)
	if err != nil {
		return result, err
	}
	if len(doc.Files) == 0 {
		return result, ErrNoChanges
	}
	for _, f := range doc.Files {
		if f.Phase == done {
			result.Completed = append(result.Completed, f.Path)
		} else {
			result.Pending = append(result.Pending, f.Path)
		}
	}
	plan, err := s.plan(ctx, doc)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			result.Failed = plan.Conflicts
			for _, failed := range result.Failed {
				result.Pending = without(result.Pending, failed.Path)
			}
		}
		return result, err
	}
	if plan.AlreadyUndone {
		result.AlreadyUndone = true
		return result, nil
	}
	if !doc.Started {
		doc.Started = true
		if err := s.save(ctx, doc); err != nil {
			return result, fmt.Errorf("seal journal: %w", err)
		}
	}
	for i := range doc.Files {
		f := &doc.Files[i]
		if f.Phase == done {
			continue
		}
		applied, stage, err := s.undoFile(ctx, doc, i)
		result.Pending = without(result.Pending, f.Path)
		if err != nil {
			result.Failed = append(result.Failed, Failure{Path: f.Path, Stage: stage, Applied: applied, Err: err})
			return result, fmt.Errorf("undo %s (%s): %w", f.Path, stage, err)
		}
		result.Completed = append(result.Completed, f.Path)
	}
	return result, nil
}

func without(paths []string, path string) []string {
	for i, item := range paths {
		if item == path {
			return append(paths[:i], paths[i+1:]...)
		}
	}
	return paths
}

func (s *Store) undoFile(ctx context.Context, doc *journal, index int) (bool, string, error) {
	f := &doc.Files[index]
	if err := ctx.Err(); err != nil {
		return false, "cancel", err
	}
	if err := s.checkRoots(); err != nil {
		return false, "check", err
	}
	p, err := openParent(s.project, f.Path)
	if err != nil {
		return false, "check", err
	}
	defer p.Close()
	name := filepath.Base(f.Path)
	state, id, err := observeLeaf(ctx, p, name)
	if err != nil {
		return false, "check", err
	}
	if recovery(*f, state, id) {
		return s.finish(ctx, doc, index, p)
	}
	if state != f.After || id != f.AfterID {
		return false, "check", fmt.Errorf("%w: file changed after preflight", ErrConflict)
	}
	needsSave := f.Phase != prepared
	if f.Before.Exists {
		var staged fingerprint
		var stagedID string
		if f.Temp != "" {
			staged, stagedID, err = observeLeaf(ctx, p, f.Temp)
			if err != nil {
				return false, "prepare", err
			}
		}
		if staged.Exists {
			if staged != f.BeforeSum || stagedID != f.TempID {
				return false, "prepare", fmt.Errorf("%w: prepared file changed", ErrConflict)
			}
		} else {
			f.Temp, f.TempID, err = stageSnapshot(ctx, p, f.Before)
			if err != nil {
				return false, "prepare", err
			}
			needsSave = true
		}
	}
	f.Phase = prepared
	if needsSave {
		if err := s.save(ctx, doc); err != nil {
			// 提交可能已可见，保留准备文件供重开后的日志确认，不能立即删除。
			return false, "prepare-journal", err
		}
	}
	if err := s.checkRoots(); err != nil {
		return false, "check", err
	}
	currentParent, err := openParent(s.project, f.Path)
	if err != nil {
		return false, "check", err
	}
	oldInfo, e1 := p.Stat(".")
	currentInfo, e2 := currentParent.Stat(".")
	currentParent.Close()
	if e1 != nil || e2 != nil || !os.SameFile(oldInfo, currentInfo) {
		return false, "check", fmt.Errorf("%w: parent changed", ErrConflict)
	}
	if f.Before.Exists {
		state, id, err = observeLeaf(ctx, p, f.Temp)
		if err != nil || state != f.BeforeSum || id != f.TempID {
			return false, "check", errors.Join(ErrConflict, err)
		}
	}
	state, id, err = observeLeaf(ctx, p, name)
	if err != nil || state != f.After || id != f.AfterID {
		return false, "check", errors.Join(ErrConflict, err)
	}
	if err := ctx.Err(); err != nil {
		return false, "cancel", err
	}
	if f.Before.Exists {
		err = replaceFile(p, f.Temp, name, f.TempID)
	} else {
		err = removeFile(p, name, f.AfterID)
	}
	if err != nil {
		return false, "apply", err
	}
	// 单文件效果已经发生。即使此时取消，也收尾其落盘状态，随后再停止下一文件。
	return s.finish(context.WithoutCancel(ctx), doc, index, p)
}

func (s *Store) finish(ctx context.Context, doc *journal, index int, parent *os.Root) (bool, string, error) {
	f := &doc.Files[index]
	if err := syncDirectory(parent); err != nil {
		return true, "sync", err
	}
	state, id, err := observeLeaf(ctx, parent, filepath.Base(f.Path))
	if err != nil || !recovery(*f, state, id) {
		return true, "verify", errors.Join(ErrConflict, err)
	}
	f.Phase, f.Temp, f.TempID = done, "", ""
	if err := s.save(ctx, doc); err != nil {
		return true, "complete-journal", err
	}
	return true, "", nil
}

func stageSnapshot(ctx context.Context, parent *os.Root, before Snapshot) (name, id string, err error) {
	f, name, err := createTemp(parent, tempPrefix)
	if err != nil {
		return "", "", err
	}
	id, _, err = fileIdentity(f)
	defer func() {
		if err != nil {
			_ = removeFile(parent, name, id)
		}
	}()
	if err == nil {
		_, err = io.Copy(f, contextReader{ctx, bytes.NewReader(before.Data)})
	}
	if err == nil {
		err = f.Chmod(before.Mode)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = syncDirectory(parent)
	}
	return name, id, err
}
