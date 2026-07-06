// Package objstore abstracts the S3-compatible blob store (spec §4.1).
// The fs backend is the dev default; the s3 backend (MinIO/AWS) plugs in
// behind the same interface (spec §14.2 escape-hatch discipline).
package objstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lazorfuzz/memba/internal/config"
)

// Store is the blob interface memd uses.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// New builds the configured backend.
func New(cfg config.ObjStore) (Store, error) {
	switch cfg.Backend {
	case "", "fs":
		dir := cfg.Dir
		if dir == "" {
			dir = "./data/objstore"
		}
		return &FS{Root: dir}, nil
	case "s3":
		return NewS3(context.Background(), cfg)
	default:
		return nil, fmt.Errorf("unknown objstore backend %q (fs|s3)", cfg.Backend)
	}
}

// FS is a filesystem-backed blob store.
type FS struct{ Root string }

func (f *FS) path(key string) (string, error) {
	clean := filepath.Clean("/" + key) // force-anchor, strips ..
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("bad key")
	}
	return filepath.Join(f.Root, clean), nil
}

func (f *FS) Put(_ context.Context, key string, r io.Reader) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	w, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		os.Remove(tmp)
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (f *FS) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (f *FS) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
