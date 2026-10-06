// Package blob is content-addressed file storage behind a small interface.
// The filesystem implementation is enough for a single box; an S3 adapter
// can be added without touching callers.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("blob: not found")

// Store persists opaque bytes under keys.
type Store interface {
	// Put writes r and returns the key. Keys are "sha256/<hex>" so identical
	// content dedupes. Size is the number of bytes written.
	Put(ctx context.Context, r io.Reader) (key string, size int64, err error)
	// Get opens a blob for reading. Caller closes.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes a blob. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Stat returns the size of a blob.
	Stat(ctx context.Context, key string) (size int64, err error)
}

// FS stores blobs on the local filesystem.
type FS struct{ root string }

// NewFS creates the root directory if needed.
func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(filepath.Join(root, "sha256"), 0o750); err != nil {
		return nil, fmt.Errorf("blob: mkdir: %w", err)
	}
	return &FS{root: root}, nil
}

func (f *FS) path(key string) (string, error) {
	// key format: sha256/<64 hex>
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != 64 {
		return "", fmt.Errorf("blob: bad key %q", key)
	}
	for _, c := range parts[1] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", fmt.Errorf("blob: bad key %q", key)
		}
	}
	// two-level fanout to keep directories small
	return filepath.Join(f.root, "sha256", parts[1][:2], parts[1]), nil
}

// Put streams to a temp file while hashing, then renames into place.
func (f *FS) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(f.root, "put-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	cerr := tmp.Close()
	if err != nil {
		return "", 0, err
	}
	if cerr != nil {
		return "", 0, cerr
	}
	key := "sha256/" + hex.EncodeToString(h.Sum(nil))
	dst, _ := f.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, err
	}
	if _, err := os.Stat(dst); err == nil {
		return key, n, nil // already have it
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", 0, err
	}
	return key, n, nil
}

// Get opens the blob.
func (f *FS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return fh, err
}

// Delete removes the blob.
func (f *FS) Delete(ctx context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Stat returns the size.
func (f *FS) Stat(ctx context.Context, key string) (int64, error) {
	p, err := f.path(key)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// PutBytes is a convenience for small payloads.
func PutBytes(ctx context.Context, s Store, b []byte) (string, error) {
	key, _, err := s.Put(ctx, strings.NewReader(string(b)))
	return key, err
}

// GetBytes reads a whole blob.
func GetBytes(ctx context.Context, s Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
