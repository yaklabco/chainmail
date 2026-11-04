package cache

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// t0 is the start time of every stub clock in this package's tests.
var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// stubClock is a settable time source safe for concurrent use.
type stubClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// openStore opens a fresh cache.db in the test's temp dir with a stub clock at t0.
func openStore(t *testing.T) (*Store, *stubClock) {
	t.Helper()
	c := &stubClock{now: t0}
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "cache.db"), OpenOptions{Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	return s, c
}

// countRows runs a count(*) query on the store's database.
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
