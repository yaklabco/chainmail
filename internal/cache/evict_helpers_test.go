package cache

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

// oneDay is a day of the prefetch window and retention.
const oneDay = 24 * time.Hour

// t10Start is when the prefetch and eviction test clocks start: 10:00 UTC,
// so the next budget day is 14 hours away.
var t10Start = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

// prefetchClock is a watch.Clock whose timers fire only when the test moves
// it. After with a duration of zero or less fires at once.
type prefetchClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []prefetchTimer
}

type prefetchTimer struct {
	at time.Time
	ch chan time.Time
}

func newPrefetchClock(now time.Time) *prefetchClock { return &prefetchClock{now: now} }

func (c *prefetchClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *prefetchClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, prefetchTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Set moves the clock to t and fires every timer now due. Moving it back
// fires nothing.
func (c *prefetchClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(c.now) {
			kept = append(kept, tm)
			continue
		}
		tm.ch <- c.now
	}
	c.timers = kept
}

// Advance moves the clock forward by d.
func (c *prefetchClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// waiting is how many timers have not fired yet.
func (c *prefetchClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// openEvictStore opens a fresh cache.db in the test's temp dir whose clock
// is now.
func openEvictStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "cache.db"), OpenOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// evictTextRaw is a small text/plain message whose body is word.
func evictTextRaw(word string) []byte {
	return []byte("Subject: s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + word + "\r\n")
}

// evictPadLines makes the padding of evictPaddedRaw about 64 KiB.
const evictPadLines = 840

// evictPaddedRaw is a message whose text part is word and whose second,
// non-text part holds about 64 KiB of padding, so the body is large while
// its FTS terms stay few.
func evictPaddedRaw(word string) []byte {
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"cut\"\r\n\r\n")
	b.WriteString("--cut\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + word + "\r\n")
	b.WriteString("--cut\r\nContent-Type: application/octet-stream\r\n\r\n")
	line := strings.Repeat("A", 76) + "\r\n"
	for range evictPadLines {
		b.WriteString(line)
	}
	b.WriteString("--cut--\r\n")
	return []byte(b.String())
}

// evictSeed is one cached message with a body, for seedEvict.
type evictSeed struct {
	account string
	folder  string // "" means INBOX
	uid     uint32
	arrived time.Time
	readAt  time.Time // zero: never read
	raw     []byte
}

// seedEvict stores each seed's header row (UIDVALIDITY 7, subject
// "keepword subject<uid>", from Ann Example) and body, read at readAt when
// set, and returns their refs. It moves clk to readAt for the read and back.
func seedEvict(t *testing.T, st *Store, clk *prefetchClock, seeds ...evictSeed) []ref.Ref {
	t.Helper()
	ctx := t.Context()
	back := clk.Now()
	out := make([]ref.Ref, len(seeds))
	for i, s := range seeds {
		row := Row{
			Account: s.account, Folder: cmp.Or(s.folder, "INBOX"), UIDValidity: 7, UID: s.uid,
			InternalDate: s.arrived, Date: s.arrived, Size: int64(len(s.raw)),
			Subject: fmt.Sprintf("keepword subject%d", s.uid),
			From:    []mailstore.Address{{Name: "Ann Example", Email: "ann@example.com"}},
		}
		if err := st.PutRows(ctx, []Row{row}); err != nil {
			t.Fatal(err)
		}
		r := row.Ref()
		read := !s.readAt.IsZero()
		if read {
			clk.Set(s.readAt)
		}
		ok, err := st.PutBody(ctx, r, Body{Raw: s.raw}, read)
		clk.Set(back)
		if err != nil || !ok {
			t.Fatalf("PutBody(%v) = %v, %v", r, ok, err)
		}
		out[i] = r
	}
	return out
}

// hasBody reports whether r's body is cached.
func hasBody(t *testing.T, st *Store, r ref.Ref) bool {
	t.Helper()
	_, ok, err := st.Body(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// pollUntil checks cond every 5ms until it holds, failing the test after 10s.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// constDur is a per-account setting that is d for every account.
func constDur(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}
