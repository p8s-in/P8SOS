package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Object is one file at a target.
type Object struct {
	Key  string `json:"key"` // path below the target's prefix, "/"-separated
	Size int64  `json:"size"`
}

// Backend stores files somewhere else: a folder (USB disk, NAS mount),
// S3-compatible storage, or another server over SSH.
type Backend interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string, w io.Writer) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

var ErrNotFound = errors.New("not found at the target")

// safeKey refuses keys that could leave the target's folder.
func safeKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return fmt.Errorf("bad key %q", key)
	}
	for _, p := range strings.Split(key, "/") {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("bad key %q", key)
		}
	}
	return nil
}

// dirBackend writes into a local folder (e.g. a mounted USB disk or NFS share).
type dirBackend struct{ root string }

func (d dirBackend) path(key string) (string, error) {
	if err := safeKey(key); err != nil {
		return "", err
	}
	return filepath.Join(d.root, filepath.FromSlash(key)), nil
}

func (d dirBackend) ready() error {
	st, err := os.Stat(d.root)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("folder %s is not there (is the disk mounted?)", d.root)
	}
	return nil
}

func (d dirBackend) Put(ctx context.Context, key string, r io.Reader) error {
	if err := d.ready(); err != nil {
		return err
	}
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, ctxReader{ctx, r}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

func (d dirBackend) Get(ctx context.Context, key string, w io.Writer) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, ctxReader{ctx, f})
	return err
}

func (d dirBackend) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	out := []Object{}
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.HasSuffix(p, ".partial") {
			return nil
		}
		rel, _ := filepath.Rel(d.root, p)
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		if info, err := e.Info(); err == nil {
			out = append(out, Object{Key: key, Size: info.Size()})
		}
		return nil
	})
	return out, err
}

func (d dirBackend) Delete(ctx context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if dir := filepath.Dir(p); dir != filepath.Clean(d.root) {
		_ = os.Remove(dir) // the guest's folder, once empty (never the target itself)
	}
	return nil
}

// ctxReader stops a copy when the context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
