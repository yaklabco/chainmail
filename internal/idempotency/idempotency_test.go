package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
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
	if err := s.Finish(ctx, "acct", "stale", Done, nil); err != nil {
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

// waitForWaiters blocks until n callers are waiting on the in-process attempt
// for (account, key), or fails the test after a deadline.
func waitForWaiters(t *testing.T, s *Store, account, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		got := 0
		if att, ok := s.attempts[rowKey{account, key}]; ok {
			got = att.waiting
		}
		s.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d callers waiting on %s/%s, want %d", got, account, key, n)
		}
		time.Sleep(time.Millisecond)
	}
}

type beginResult struct {
	claim Claim
	err   error
}

// beginAsync runs Begin in a goroutine bounded by a timeout, so a regression
// that never wakes the caller fails the test instead of hanging it.
func beginAsync(t *testing.T, s *Store, account, key string, hash [32]byte) <-chan beginResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	out := make(chan beginResult, 1)
	go func() {
		defer cancel()
		claim, err := s.Begin(ctx, account, key, hash)
		out <- beginResult{claim, err}
	}()
	return out
}

func receive(t *testing.T, ch <-chan beginResult) beginResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Begin did not return")
		return beginResult{}
	}
}

// A duplicate that waited on an attempt gets that attempt's outcome, even
// when it is Retryable. It never becomes the next attempt itself, because the
// daemon never retries a send on its own.
func TestWaiterReceivesRetryableOutcome(t *testing.T) {
	t.Parallel()
	s, conn, _ := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	if _, err := s.Begin(ctx, "acct", "k", h); err != nil {
		t.Fatal(err)
	}
	waiter := beginAsync(t, s, "acct", "k", h)
	waitForWaiters(t, s, "acct", "k", 1)
	if err := s.Finish(ctx, "acct", "k", Retryable, []byte("4xx")); err != nil {
		t.Fatal(err)
	}
	r := receive(t, waiter)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.claim.Proceed || r.claim.State != Retryable || string(r.claim.Result) != "4xx" {
		t.Errorf("waiter claim = %+v, want Retryable 4xx without proceeding", r.claim)
	}
	if got := stateOf(t, conn, "acct", "k"); got != Retryable {
		t.Errorf("stored state = %q, want retryable", got)
	}
	// A later request, not waiting on that attempt, takes the retry.
	later := receive(t, beginAsync(t, s, "acct", "k", h))
	if later.err != nil {
		t.Fatal(later.err)
	}
	if !later.claim.Proceed || later.claim.State != InProgress {
		t.Errorf("later claim = %+v, want a fresh in_progress claim", later.claim)
	}
}

// A retry claimed and finished before a woken waiter runs does not replace
// the outcome of the attempt that waiter was waiting for.
func TestWaiterKeepsItsAttemptOutcome(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := t.Context()
	h := hashOf("req")
	if _, err := s.Begin(ctx, "acct", "k", h); err != nil {
		t.Fatal(err)
	}
	waiter := beginAsync(t, s, "acct", "k", h)
	waitForWaiters(t, s, "acct", "k", 1)
	if err := s.Finish(ctx, "acct", "k", Retryable, []byte("4xx")); err != nil {
		t.Fatal(err)
	}
	retry := receive(t, beginAsync(t, s, "acct", "k", h))
	if retry.err != nil {
		t.Fatal(retry.err)
	}
	if retry.claim.Proceed {
		if err := s.Finish(ctx, "acct", "k", Done, []byte("ok")); err != nil {
			t.Fatal(err)
		}
	}
	r := receive(t, waiter)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.claim.Proceed || r.claim.State != Retryable || string(r.claim.Result) != "4xx" {
		t.Errorf("waiter claim = %+v, want Retryable 4xx from its own attempt", r.claim)
	}
}

// Prune leaves a claim this process holds, so a retry taken just before the
// row's seven days run out cannot be deleted from under its waiters, and a new
// Begin waits on it instead of starting a second send.
func TestPruneKeepsActiveClaim(t *testing.T) {
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
	c.Set(t0.Add(retention - time.Minute))
	retry, err := s.Begin(ctx, "acct", "k", h)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Proceed {
		t.Fatalf("retry claim = %+v, want it to proceed", retry)
	}
	first := beginAsync(t, s, "acct", "k", h)
	waitForWaiters(t, s, "acct", "k", 1)

	c.Set(t0.Add(retention + time.Minute))
	n, err := s.Prune(ctx, c.Now().Add(-retention))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("Prune = %d, want 0 while the claim is active", n)
	}

	second := beginAsync(t, s, "acct", "k", h)
	waitForWaiters(t, s, "acct", "k", 2)

	if err := s.Finish(ctx, "acct", "k", Done, []byte("sent")); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for i, ch := range []<-chan beginResult{first, second} {
		r := receive(t, ch)
		if r.err != nil {
			t.Errorf("waiter %d: %v", i, r.err)
			continue
		}
		if r.claim.Proceed || r.claim.State != Done || string(r.claim.Result) != "sent" {
			t.Errorf("waiter %d claim = %+v, want Done sent", i, r.claim)
		}
	}
	// Once settled, the row is old enough to go.
	n, err = s.Prune(ctx, c.Now().Add(-retention))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || countRows(t, conn) != 0 {
		t.Errorf("Prune after Finish = %d with %d rows left, want 1 and 0", n, countRows(t, conn))
	}
}

// An in_progress row with no holder in this process is a leftover from
// another process and is pruned by age like any other row.
func TestPruneDeletesInProgressHeldElsewhere(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	c := &clock{now: t0}
	ctx := t.Context()
	if _, err := New(conn, c.Now).Begin(ctx, "acct", "k", hashOf("req")); err != nil {
		t.Fatal(err)
	}
	n, err := New(conn, c.Now).Prune(ctx, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Prune = %d, want 1", n)
	}
}

// syncBuffer is a bytes.Buffer safe for a writer and a reader in different goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunLogsPruneFailure(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	s := New(conn, (&clock{now: t0}).Now)
	var logs syncBuffer
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := conn.Close(); err != nil { // every prune now fails
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "pruning idempotency keys") {
		if time.Now().After(deadline) {
			cancel()
			<-stopped
			t.Fatalf("no prune failure logged; log = %q", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("log = %q, want an error-level record", logs.String())
	}
}

// A prune cut short by shutdown is expected and is not reported.
func TestRunDoesNotLogCancellation(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	var logs syncBuffer
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.Run(ctx)
	if got := logs.String(); got != "" {
		t.Errorf("log = %q, want nothing on cancellation", got)
	}
}

func TestLookupReadsWithoutClaiming(t *testing.T) {
	t.Parallel()
	s, conn, _ := newStore(t)
	ctx := t.Context()
	if _, ok, err := s.Lookup(ctx, "acct", "k"); err != nil || ok {
		t.Fatalf("absent key: ok = %v, err = %v, want no row", ok, err)
	}
	if _, err := s.Begin(ctx, "acct", "k", hashOf("req")); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.Lookup(ctx, "acct", "k")
	if err != nil || !ok || row.State != InProgress || row.Hash != hashOf("req") || row.Result != nil {
		t.Fatalf("in progress: row = %+v, ok = %v, err = %v", row, ok, err)
	}
	if err := s.Finish(ctx, "acct", "k", Done, []byte("result")); err != nil {
		t.Fatal(err)
	}
	row, ok, err = s.Lookup(ctx, "acct", "k")
	if err != nil || !ok || row.State != Done || row.Hash != hashOf("req") || string(row.Result) != "result" {
		t.Fatalf("done: row = %+v, ok = %v, err = %v", row, ok, err)
	}
	if got := stateOf(t, conn, "acct", "k"); got != Done {
		t.Errorf("state after Lookup = %q, want done", got)
	}
	if _, ok, err := s.Lookup(ctx, "other", "k"); err != nil || ok {
		t.Errorf("another account: ok = %v, err = %v, want no row", ok, err)
	}
}
