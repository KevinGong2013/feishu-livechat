// Package localblob is a livechat.BlobStore on the local disk, for
// standalone deployments. Back the directory up with the database.
package localblob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Store keeps blobs as files under Dir.
type Store struct {
	Dir string
}

var _ livechat.BlobStore = (*Store)(nil)

// New returns a Store rooted at dir, creating it if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if key == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("localblob: invalid key")
	}
	return filepath.Join(s.Dir, clean), nil
}

// Put writes the blob atomically (temp file + rename).
func (s *Store) Put(_ context.Context, key, _ string, data []byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}
