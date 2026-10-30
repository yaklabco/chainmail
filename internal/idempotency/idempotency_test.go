package idempotency

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/db"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// clock is a settable time source safe for concurrent use.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	return conn
}

func newStore(t *testing.T) (*Store, *sql.DB, *clock) {
	t.Helper()
	conn := openDB(t)
	c := &clock{now: t0}
	return New(conn, c.Now), conn, c
}

func hashOf(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func stateOf(t *testing.T, conn *sql.DB, account, key string) State {
	t.Helper()
	var s string
	err := conn.QueryRowContext(t.Context(),
		"SELECT state FROM idempotency WHERE account = ? AND key = ?", account, key).Scan(&s)
	if err != nil {
		t.Fatalf("reading state of %s/%s: %v", account, key, err)
	}
	return State(s)
}

func countRows(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM idempotency").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestValidKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want bool
	}{
		{"", false},
		{"a", true},
		{"cli-ABCDEFGHIJ234567", true},
		{"A-z_0.9:x", true},
		{strings.Repeat("k", 128), true},
		{strings.Repeat("k", 129), false},
		{"has space", false},
		{"slash/no", false},
		{"plus+no", false},
		{"tab\tno", false},
		{"newline\n", false},
		{"café", false},
		{"semi;no", false},
	}
	for _, tt := range tests {
		if got := ValidKey(tt.key); got != tt.want {
			t.Errorf("ValidKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestNewKeyProceeds(t *testing.T) {
	t.Parallel()
	s, conn, _ := newStore(t)
	claim, err := s.Begin(t.Context(), "acct", "k1", hashOf("req"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !claim.Proceed || claim.State != InProgress {
		t.Errorf("claim = %+v, want Proceed with state in_progress", claim)
	}
	if got := stateOf(t, conn, "acct", "k1"); got != InProgress {
		t.Errorf("stored state = %q, want in_progress", got)
	}
}

func TestFinishedStatesReturnStoredResult(t *testing.T) {
	t.Parallel()
	for _, state := range []State{Done, Failed, Unknown} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			s, conn, _ := newStore(t)
			ctx := t.Context()
			h := hashOf("req")
			if _, err := s.Begin(ctx, "acct", "k", h); err != nil {
				t.Fatal(err)
			}
			result := []byte("result-" + string(state))
			if err := s.Finish(ctx, "acct", "k", state, result); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			claim, err := s.Begin(ctx, "acct", "k", h)
			if err != nil {
				t.Fatalf("second Begin: %v", err)
			}
			if claim.Proceed {
				t.Error("second Begin proceeded, want the stored outcome")
			}
			if claim.State != state || string(claim.Result) != string(result) {
				t.Errorf("claim = {%q %q}, want {%q %q}", claim.State, claim.Result, state, result)
			}
			if got := stateOf(t, conn, "acct", "k"); got != state {
				t.Errorf("stored state = %q, want %q", got, state)
			}
		})
	}
}

func TestRetryableProceedsAgain(t *testing.T) {
	t.Parallel()
	s, conn, c := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	if _, err := s.Begin(ctx, "acct", "k", h); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "acct", "k", Retryable, []byte("4xx")); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(time.Minute))
	claim, err := s.Begin(ctx, "acct", "k", h)
	if err != nil {
		t.Fatalf("Begin after retryable: %v", err)
	}
	if !claim.Proceed || claim.State != InProgress || len(claim.Result) != 0 {
		t.Errorf("claim = %+v, want a fresh in_progress claim", claim)
	}
	if got := stateOf(t, conn, "acct", "k"); got != InProgress {
		t.Errorf("stored state = %q, want in_progress", got)
	}
	// The retry finishes normally and a later duplicate sees Done.
	if err := s.Finish(ctx, "acct", "k", Done, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	claim, err = s.Begin(ctx, "acct", "k", h)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Proceed || claim.State != Done || string(claim.Result) != "ok" {
		t.Errorf("claim = %+v, want Done ok", claim)
	}
}

func TestDifferentHashIsKeyMismatch(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	if _, err := s.Begin(ctx, "acct", "k", hashOf("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "acct", "k", Done, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Begin(ctx, "acct", "k", hashOf("two"))
	if !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("err = %v, want ErrKeyMismatch", err)
	}
}

func TestDifferentHashWhileInProgressIsKeyMismatch(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	if _, err := s.Begin(ctx, "acct", "k", hashOf("one")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Begin(ctx, "acct", "k", hashOf("two"))
	if !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("err = %v, want ErrKeyMismatch without waiting", err)
	}
}

func TestConcurrentBeginsOneProceeds(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	const n = 20
	claims := make([]Claim, n)
	errs := make([]error, n)
	start := make(chan struct{})
	proceeded := make(chan int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			claims[i], errs[i] = s.Begin(ctx, "acct", "k", h)
			if errs[i] == nil && claims[i].Proceed {
				proceeded <- i
				// Give the other callers time to reach the wait.
				time.Sleep(20 * time.Millisecond)
				if err := s.Finish(ctx, "acct", "k", Done, []byte("sent")); err != nil {
					t.Errorf("Finish: %v", err)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	close(proceeded)
	var winners []int
	for i := range proceeded {
		winners = append(winners, i)
	}
	if len(winners) != 1 {
		t.Fatalf("%d callers proceeded, want exactly 1", len(winners))
	}
	for i := range n {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
			continue
		}
		if i == winners[0] {
			continue
		}
		if claims[i].Proceed || claims[i].State != Done || string(claims[i].Result) != "sent" {
			t.Errorf("caller %d claim = %+v, want Done sent", i, claims[i])
		}
	}
}

func TestWaitHonorsContext(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	h := hashOf("req")
	if _, err := s.Begin(t.Context(), "acct", "k", h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.Begin(ctx, "acct", "k", h)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Begin returned %v before cancel, want it to wait", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Begin did not return after cancel")
	}
	// The holder can still finish, and the key is usable afterwards.
	if err := s.Finish(t.Context(), "acct", "k", Done, []byte("ok")); err != nil {
		t.Fatalf("Finish after a cancelled waiter: %v", err)
	}
}

func TestFinishRejectsInProgress(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	if _, err := s.Begin(ctx, "acct", "k", hashOf("req")); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "acct", "k", InProgress, nil); err == nil {
		t.Error("Finish(InProgress) succeeded, want an error")
	}
	if err := s.Finish(ctx, "acct", "k", State("bogus"), nil); err == nil {
		t.Error("Finish(bogus) succeeded, want an error")
	}
}

func TestFinishWithoutClaimFails(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	if err := s.Finish(t.Context(), "acct", "missing", Done, nil); err == nil {
		t.Error("Finish of an unclaimed key succeeded, want an error")
	}
}

func TestRecoverMarksLeftoversUnknown(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	c := &clock{now: t0}
	ctx := t.Context()
	h := hashOf("req")

	// An earlier process claims two keys and finishes one, then dies.
	old := New(conn, c.Now)
	for _, k := range []string{"left", "finished"} {
		if _, err := old.Begin(ctx, "acct", k, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Finish(ctx, "acct", "finished", Done, []byte("ok")); err != nil {
		t.Fatal(err)
	}

	s := New(conn, c.Now)
	n, err := s.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("Recover = %d, want 1", n)
	}
	if got := stateOf(t, conn, "acct", "left"); got != Unknown {
		t.Errorf("left state = %q, want unknown", got)
	}
	if got := stateOf(t, conn, "acct", "finished"); got != Done {
		t.Errorf("finished state = %q, want done", got)
	}
	claim, err := s.Begin(ctx, "acct", "left", h)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Proceed || claim.State != Unknown {
		t.Errorf("claim = %+v, want Unknown without proceeding", claim)
	}
}

func TestPruneByCutoff(t *testing.T) {
	t.Parallel()
	s, conn, c := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	for i, k := range []string{"old", "edge", "new"} {
		c.Set(t0.Add(time.Duration(i) * time.Hour))
		if _, err := s.Begin(ctx, "acct", k, h); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, "acct", k, Done, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Rows created strictly before the cutoff go; "edge" was created at it.
	n, err := s.Prune(ctx, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("Prune = %d, want 1", n)
	}
	if got := countRows(t, conn); got != 2 {
		t.Errorf("%d rows left, want 2", got)
	}
	var k string
	if err := conn.QueryRowContext(ctx, "SELECT key FROM idempotency ORDER BY created_at LIMIT 1").Scan(&k); err != nil {
		t.Fatal(err)
	}
	if k != "edge" {
		t.Errorf("oldest remaining key = %q, want edge", k)
	}
}

func TestPruneUsesCreatedNotUpdated(t *testing.T) {
	t.Parallel()
	s, conn, c := newStore(t)
	ctx := t.Context()
	if _, err := s.Begin(ctx, "acct", "k", hashOf("req")); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(48 * time.Hour))
	if err := s.Finish(ctx, "acct", "k", Done, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, conn); got != 0 {
		t.Errorf("%d rows left, want 0", got)
	}
}

func TestRunPrunesAndStops(t *testing.T) {
	t.Parallel()
	s, conn, c := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	c.Set(t0.Add(-8 * 24 * time.Hour))
	if _, err := s.Begin(ctx, "acct", "stale", h); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(-6 * 24 * time.Hour))
	if _, err := s.Begin(ctx, "acct", "recent", h); err != nil {
		t.Fatal(err)
	}
	c.Set(t0)

	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		s.Run(runCtx)
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for countRows(t, conn) != 1 {
		if time.Now().After(deadline) {
			cancel()
			<-stopped
			t.Fatal("Run did not prune the stale row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if got := stateOf(t, conn, "acct", "recent"); got != InProgress {
		t.Errorf("recent state = %q, want in_progress", got)
	}
}

func TestSameKeyOnTwoAccountsIsIndependent(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	a, err := s.Begin(ctx, "alice", "k", hashOf("one"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Begin(ctx, "bob", "k", hashOf("two"))
	if err != nil {
		t.Fatalf("Begin for a second account: %v", err)
	}
	if !a.Proceed || !b.Proceed {
		t.Errorf("claims = %+v, %+v, want both to proceed", a, b)
	}
	if err := s.Finish(ctx, "alice", "k", Done, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "bob", "k", Failed, []byte("b")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		account, req string
		state        State
		result       string
	}{
		{"alice", "one", Done, "a"},
		{"bob", "two", Failed, "b"},
	} {
		claim, err := s.Begin(ctx, tt.account, "k", hashOf(tt.req))
		if err != nil {
			t.Fatal(err)
		}
		if claim.State != tt.state || string(claim.Result) != tt.result {
			t.Errorf("%s claim = %+v, want %s %s", tt.account, claim, tt.state, tt.result)
		}
	}
}

func TestInProgressHeldElsewhereDoesNotWait(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	c := &clock{now: t0}
	ctx := t.Context()
	h := hashOf("req")
	// Another Store over the same database stands in for another process.
	if _, err := New(conn, c.Now).Begin(ctx, "acct", "k", h); err != nil {
		t.Fatal(err)
	}
	claim, err := New(conn, c.Now).Begin(ctx, "acct", "k", h)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Proceed || claim.State != InProgress {
		t.Errorf("claim = %+v, want InProgress without proceeding", claim)
	}
}

func TestWaiterProceedsAfterRetryable(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	if _, err := s.Begin(ctx, "acct", "k", h); err != nil {
		t.Fatal(err)
	}
	got := make(chan Claim, 1)
	errc := make(chan error, 1)
	go func() {
		claim, err := s.Begin(ctx, "acct", "k", h)
		got <- claim
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.Finish(ctx, "acct", "k", Retryable, []byte("4xx")); err != nil {
		t.Fatal(err)
	}
	claim := <-got
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !claim.Proceed || claim.State != InProgress {
		t.Errorf("claim = %+v, want the waiter to take the retry", claim)
	}
}
