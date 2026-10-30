// Package upload holds attachment bytes uploaded to the daemon until a send
// uses them.
//
// Each upload is a 0600 file in one directory, named by its handle. The
// client's filename is kept as metadata only and never becomes part of a path.
// Uploads expire after a fixed time to live and the index lives in memory, so
// a restarted daemon starts with an empty directory.
package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Errors returned by the store.
var (
	ErrUnknownHandle = errors.New("unknown or expired upload")
	ErrTooLarge      = errors.New("upload too large")
)

// handlePrefix starts every upload handle.
const handlePrefix = "up_"

// sweepInterval is how often Run removes expired uploads.
const sweepInterval = time.Minute

// handleEncoding renders 16 random bytes as 26 lowercase base32 characters.
var handleEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Info describes one stored upload.
type Info struct {
	Handle      string
	Filename    string
	ContentType string
	Size        int64
	SHA256      [32]byte
}

type entry struct {
	info    Info
	created time.Time
}

// Store keeps uploaded bytes on disk under dir until they expire.
type Store struct {
	dir string
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	index map[string]entry
}

// New creates dir (0700) and removes files left from an earlier run. A nil
// now uses time.Now.
func New(dir string, ttl time.Duration, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: create dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: chmod dir: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("upload: read dir: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return nil, fmt.Errorf("upload: remove leftover: %w", err)
		}
	}
	return &Store{dir: dir, ttl: ttl, now: now, index: make(map[string]entry)}, nil
}

// Put stores r under a new handle, refusing more than maxBytes bytes. On any
// error, including ErrTooLarge and a canceled ctx, nothing is left on disk.
func (s *Store) Put(ctx context.Context, filename, contentType string, r io.Reader, maxBytes int64) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	handle, err := newHandle()
	if err != nil {
		return Info{}, err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return Info{}, fmt.Errorf("upload: create temp: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	// CreateTemp already uses 0600. Set it anyway so the mode never depends
	// on that detail.
	if err := tmp.Chmod(0o600); err != nil {
		return Info{}, fmt.Errorf("upload: chmod: %w", err)
	}

	h := sha256.New()
	// Read one byte past the limit so an oversized upload is detected without
	// reading the rest of it.
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(ctxReader{ctx: ctx, r: r}, limit))
	if err != nil {
		return Info{}, fmt.Errorf("upload: write: %w", err)
	}
	if n > maxBytes {
		return Info{}, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return Info{}, fmt.Errorf("upload: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Info{}, fmt.Errorf("upload: close: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, handle)); err != nil {
		return Info{}, fmt.Errorf("upload: rename: %w", err)
	}
	keep = true

	info := Info{
		Handle:      handle,
		Filename:    filename,
		ContentType: contentType,
		Size:        n,
	}
	copy(info.SHA256[:], h.Sum(nil))

	s.mu.Lock()
	s.index[handle] = entry{info: info, created: s.now()}
	s.mu.Unlock()
	return info, nil
}

// Open returns the info and bytes of a live handle. An unknown or expired
// handle returns ErrUnknownHandle.
func (s *Store) Open(handle string) (Info, []byte, error) {
	info, err := s.Stat(handle)
	if err != nil {
		return Info{}, nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, info.Handle))
	if errors.Is(err, os.ErrNotExist) {
		// Swept between the lookup and the read.
		return Info{}, nil, ErrUnknownHandle
	}
	if err != nil {
		return Info{}, nil, fmt.Errorf("upload: read: %w", err)
	}
	return info, data, nil
}

// Stat returns the info of a live handle without reading its bytes, so a
// caller can check sizes first. An unknown or expired handle returns
// ErrUnknownHandle.
func (s *Store) Stat(handle string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index[handle]
	if ok && s.expired(e) {
		s.removeLocked(handle) // a failed removal is retried by the next Sweep
		ok = false
	}
	if !ok {
		return Info{}, ErrUnknownHandle
	}
	return e.info, nil
}

// Sweep removes expired uploads and returns how many.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for handle, e := range s.index {
		if s.expired(e) && s.removeLocked(handle) {
			n++
		}
	}
	return n
}

// Run sweeps every minute until ctx ends.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep()
		}
	}
}

// expired reports whether e has reached its time to live.
func (s *Store) expired(e entry) bool {
	return !s.now().Before(e.created.Add(s.ttl))
}

// removeLocked deletes handle's file and then drops it from the index. When
// the file cannot be deleted the entry stays, still expired, so the next
// Sweep tries again. It reports whether the upload is gone. The caller
// holds s.mu.
func (s *Store) removeLocked(handle string) bool {
	if err := os.Remove(filepath.Join(s.dir, handle)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	delete(s.index, handle)
	return true
}

// newHandle returns "up_" and 26 lowercase base32 characters from 16 random
// bytes.
func newHandle() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("upload: random handle: %w", err)
	}
	return handlePrefix + handleEncoding.EncodeToString(b[:]), nil
}

// ctxReader fails reads once ctx ends, so a canceled upload stops copying.
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
