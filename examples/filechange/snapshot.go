package filechange

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

type fingerprint struct {
	Exists bool        `json:"exists"`
	Size   int64       `json:"size"`
	SHA256 string      `json:"sha256"`
	Mode   fs.FileMode `json:"mode"`
}

func summarize(s Snapshot) fingerprint {
	if !s.Exists {
		return fingerprint{}
	}
	return fingerprint{Exists: true, Size: int64(len(s.Data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(s.Data)), Mode: s.Mode}
}

func normalizeSnapshot(s Snapshot) (Snapshot, error) {
	if !s.Exists {
		if len(s.Data) != 0 || s.Mode != 0 {
			return Snapshot{}, fmt.Errorf("%w: missing snapshot has data or mode", ErrInvalid)
		}
		return Snapshot{}, nil
	}
	if s.Mode & ^fs.ModePerm != 0 {
		return Snapshot{}, fmt.Errorf("%w: unsupported file mode", ErrInvalid)
	}
	if runtime.GOOS != "windows" && s.Mode&0400 == 0 {
		return Snapshot{}, fmt.Errorf("%w: snapshot requires owner read permission", ErrInvalid)
	}
	s.Mode = platformMode(s.Mode)
	return s, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func observe(ctx context.Context, root *os.Root, path string) (fingerprint, string, error) {
	p, err := openParent(root, path)
	if err != nil {
		return fingerprint{}, "", err
	}
	defer p.Close()
	return observeLeaf(ctx, p, filepath.Base(path))
}

func observeLeaf(ctx context.Context, parent *os.Root, name string) (fingerprint, string, error) {
	if err := ctx.Err(); err != nil {
		return fingerprint{}, "", err
	}
	if _, err := parent.Lstat(name); os.IsNotExist(err) {
		return fingerprint{}, "", nil
	}
	if err := checkLeaf(parent, name, false); err != nil {
		return fingerprint{}, "", err
	}
	f, err := parent.OpenFile(name, readFlags(), 0)
	if err != nil {
		return fingerprint{}, "", err
	}
	defer f.Close()
	if err := verifyOpened(parent, name, f); err != nil {
		return fingerprint{}, "", err
	}
	info, err := f.Stat()
	if err != nil {
		return fingerprint{}, "", err
	}
	id, _, err := fileIdentity(f)
	if err != nil {
		return fingerprint{}, "", err
	}
	h := sha256.New()
	size, err := io.Copy(h, contextReader{ctx, f})
	if err != nil {
		return fingerprint{}, "", err
	}
	last, err := f.Stat()
	if err != nil {
		return fingerprint{}, "", err
	}
	if last.Size() != size || info.Size() != size || info.ModTime() != last.ModTime() || info.Mode() != last.Mode() {
		return fingerprint{}, "", fmt.Errorf("%w: %s changed while reading", ErrConflict, name)
	}
	if err := verifyOpened(parent, name, f); err != nil {
		return fingerprint{}, "", err
	}
	return fingerprint{Exists: true, Size: size, SHA256: fmt.Sprintf("%x", h.Sum(nil)), Mode: info.Mode().Perm()}, id, nil
}
