// Package fsblob 是 artifact.BlobStore 的本地文件系统实现，用于单二进制模式。
package fsblob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"yanshi/internal/artifact"
)

type Store struct{ Dir string }

var safeKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s Store) path(key string) (string, error) {
	if !safeKey.MatchString(key) {
		return "", errors.New("fsblob: invalid key")
	}
	return filepath.Join(s.Dir, key), nil
}

func (s Store) Put(_ context.Context, key string, r io.Reader) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

func (s Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, artifact.ErrNotFound
	}
	return f, err
}

func (s Store) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
