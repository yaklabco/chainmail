package cache

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// prefetchRig runs one prefetcher for account acme against a fake store
// and a fresh cache, with the test clock.
type prefetchRig struct {
	t   *testing.T
	clk *prefetchClock
	st  *Store
	fk  *fake.Store
	// mail is what the prefetcher's Mail hands out: fk unless a test wraps it.
	mail mailstore.Store
	set  config.CacheSettings
	max  int64
	p    *prefetcher
}

func newPrefetchRig(t *testing.T) *prefetchRig {
	t.Helper()
	r := &prefetchRig{
		t: t, clk: newPrefetchClock(t10Start), fk: fake.New("acme", false), max: 1 << 40,
		set: config.CacheSettings{
			Enabled: true, PrefetchBodies: 90 * oneDay, ReadRetention: 30 * oneDay,
			DailyDownload: 1 << 30, BackfillBatch: 250, FreshLag: 30 * time.Second,
		},
	}
	r.mail = r.fk
	r.st = openEvictStore(t, r.clk.Now)
	r.p = newPrefetcher(prefetchOptions{
		Account:  "acme",
		Settings: func() config.CacheSettings { return r.set },
		MaxSize:  func() int64 { return r.max },
		Store:    r.st,
		Mail:     func(context.Context) (mailstore.Store, func(), error) { return r.mail, func() {}, nil },
		Clock:    r.clk,
	})
	return r
}

// add delivers a message that arrived age ago to the fake's INBOX, caches
// its header row as the backfill would, and returns its ref.
func (r *prefetchRig) add(age time.Duration, subject string) ref.Ref {
	r.t.Helper()
	at := r.clk.Now().Add(-age)
	got := r.fk.Add(fake.Message{Folder: "INBOX", Date: at, InternalDate: at, Subject: subject, Body: "text of " + subject})
	row := Row{Account: got.Account, Folder: got.Folder, UIDValidity: got.UIDValidity, UID: got.UID, InternalDate: at, Date: at, Size: 100, Subject: subject}
	if err := r.st.PutRows(r.t.Context(), []Row{row}); err != nil {
		r.t.Fatal(err)
	}
	return got
}

// track records INBOX's cache progress with the backfill done or not and
// window_done clear.
func (r *prefetchRig) track(backfillDone bool) {
	r.t.Helper()
	fs, err := r.fk.FolderState(r.t.Context(), "INBOX")
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.PutProgress(r.t.Context(), Progress{
		Account: "acme", Folder: "INBOX", UIDValidity: fs.UIDValidity, BaselineUIDNext: fs.UIDNext,
		BackfillLowUID: 1, BackfillDone: backfillDone,
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *prefetchRig) windowDone() bool {
	r.t.Helper()
	p, ok, err := r.st.Progress(r.t.Context(), "acme", "INBOX")
	if err != nil || !ok {
		r.t.Fatalf("Progress = %v, %v", ok, err)
	}
	return p.WindowDone
}

func (r *prefetchRig) has(x ref.Ref) bool { return hasBody(r.t, r.st, x) }

func (r *prefetchRig) fetches() int { return r.fk.Calls("FetchMessages") }

// step runs one prefetch step and checks the wait it returns.
func (r *prefetchRig) step(want time.Duration) {
	r.t.Helper()
	if got := r.p.step(r.t.Context()); got != want {
		r.t.Errorf("step waits %v, want %v", got, want)
	}
}

// Bodies inside the window, its start included, are fetched in one
// background batch; older ones are not.
func TestPrefetchWindow(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	inside := r.add(89*oneDay, "inside")
	edge := r.add(90*oneDay, "edge")
	outside := r.add(91*oneDay, "outside")
	r.track(true)
	r.step(0) // BackfillPause after a batch
	if !r.has(inside) || !r.has(edge) || r.has(outside) {
		t.Errorf("bodies cached: inside %v, edge %v, outside %v; want true, true, false", r.has(inside), r.has(edge), r.has(outside))
	}
	if n := r.fk.BackgroundCalls("FetchMessages"); n != 1 || r.fetches() != 1 {
		t.Errorf("FetchMessages calls = %d, background %d; want 1 background call", r.fetches(), n)
	}
}

// Nothing is fetched for a folder whose backfill has not finished, while
// the cache is off, or with a window of 0d.
func TestPrefetchIdles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		backfill bool
		set      func(*config.CacheSettings)
	}{
		{name: "backfill not done", backfill: false, set: func(*config.CacheSettings) {}},
		{name: "cache disabled", backfill: true, set: func(s *config.CacheSettings) { s.Enabled = false }},
		{name: "window of 0d", backfill: true, set: func(s *config.CacheSettings) { s.PrefetchBodies = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newPrefetchRig(t)
			m := r.add(oneDay, "recent")
			r.track(tt.backfill)
			tt.set(&r.set)
			r.step(prefetchIdle)
			if r.fetches() != 0 || r.has(m) {
				t.Errorf("fetches = %d, body cached = %v; want none", r.fetches(), r.has(m))
			}
		})
	}
}

// window_done is set once the window has nothing left to fetch, cleared
// when new mail arrives in it or the window grows over uncached mail, and
// set again once that is fetched.
func TestPrefetchWindowDone(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	r.add(oneDay, "first")
	old := r.add(60*oneDay, "old")
	r.set.PrefetchBodies = 30 * oneDay
	r.track(true)

	r.step(0)
	if r.windowDone() {
		t.Fatal("window_done set in the step that fetched")
	}
	r.step(prefetchIdle)
	if !r.windowDone() {
		t.Fatal("window_done not set once the window had no body left to fetch")
	}

	arrived := r.add(2*oneDay, "arrived")
	r.step(0)
	if r.windowDone() || !r.has(arrived) {
		t.Errorf("after new mail: window_done = %v, body cached = %v; want false, true", r.windowDone(), r.has(arrived))
	}
	r.step(prefetchIdle)
	if !r.windowDone() {
		t.Error("window_done not set again after the new mail was fetched")
	}

	r.set.PrefetchBodies = 90 * oneDay
	r.step(0)
	if r.windowDone() || !r.has(old) {
		t.Errorf("after the window grew: window_done = %v, old body cached = %v; want false, true", r.windowDone(), r.has(old))
	}
	r.step(prefetchIdle)
	if !r.windowDone() {
		t.Error("window_done not set after the grown window was fetched")
	}
	if r.fetches() != 3 {
		t.Errorf("FetchMessages calls = %d, want 3", r.fetches())
	}
}

// offered reports whether x is due for a fetch now: no body, and no
// failure waiting out its retry gap or final.
func (r *prefetchRig) offered(x ref.Ref) bool {
	r.t.Helper()
	now := r.clk.Now()
	refs, err := r.st.BodylessSince(r.t.Context(), "acme", "INBOX", now.Add(-r.set.PrefetchBodies), maxBodyAttempts, now.Add(-bodyRetryGap), 1000)
	if err != nil {
		r.t.Fatal(err)
	}
	return slices.ContainsFunc(refs, func(o ref.Ref) bool { return o.UID == x.UID })
}

// finalFailures is INBOX's count of final body failures.
func (r *prefetchRig) finalFailures() uint64 {
	r.t.Helper()
	stats, err := r.st.Stats(r.t.Context(), "acme", r.clk.Now().Add(-r.set.PrefetchBodies))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range stats {
		if s.Folder == "INBOX" {
			return s.BodyFailures
		}
	}
	return 0
}

// rawBreaker hands out the fake's messages, with raw bytes the renderer
// cannot parse (a header line without a colon) for the UIDs in broken.
type rawBreaker struct {
	*fake.Store
	broken map[uint32]bool
}

func (b rawBreaker) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out := b.Store.FetchMessages(ctx, refs)
	for i, x := range refs {
		if b.broken[x.UID] && out[i].Message != nil {
			msg := *out[i].Message
			msg.Raw = []byte("Subject: hi\r\nno colon here\r\n\r\nbody\r\n")
			out[i].Message = &msg
		}
	}
	return out
}

func TestBodyFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "busy pool", err: mailstore.ErrBusy, want: ""},
		{name: "server unavailable", err: fmt.Errorf("%w: reset", mailstore.ErrUnavailable), want: ""},
		{name: "connection limit", err: fmt.Errorf("%w: %w", mailstore.ErrUnavailable, mailstore.ErrConnLimit), want: ""},
		{name: "auth required", err: &auth.RequiredError{Account: "acme", Hint: "run: chainmail auth login acme"}, want: ""},
		{name: "auth rejected", err: fmt.Errorf("login: %w", auth.ErrRejected), want: ""},
		{name: "canceled", err: context.Canceled, want: ""},
		{name: "deadline", err: fmt.Errorf("fetch: %w", context.DeadlineExceeded), want: ""},
		{name: "stale ref", err: fmt.Errorf("%w: message is gone", mailstore.ErrStaleRef), want: failGone},
		{name: "not found", err: mailstore.ErrNotFound, want: failGone},
		{name: "anything else", err: errors.New("BAD unexpected answer"), want: failFetch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := bodyFailure(tt.err); got != tt.want {
				t.Errorf("bodyFailure(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// A body that keeps failing is retried 8 hours after each attempt and is
// final after the third, which then counts as done for window_done.
func TestPrefetchFailuresGoFinal(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "failing")
	r.track(true)
	r.fk.Fail("FetchMessages", errors.New("BAD unexpected answer"))
	for attempt := 1; attempt <= maxBodyAttempts; attempt++ {
		r.step(0)
		if r.fetches() != attempt {
			t.Fatalf("attempt %d: FetchMessages calls = %d", attempt, r.fetches())
		}
		if r.offered(m) {
			t.Fatalf("attempt %d: offered again before the retry gap", attempt)
		}
		if attempt < maxBodyAttempts {
			r.step(prefetchIdle)
			if r.fetches() != attempt || r.windowDone() {
				t.Fatalf("attempt %d: within the gap, calls = %d, window_done = %v; want no fetch, not done", attempt, r.fetches(), r.windowDone())
			}
		}
		// The retry is due once the last attempt is strictly older than the gap.
		r.clk.Advance(bodyRetryGap + time.Second)
	}
	r.step(prefetchIdle)
	if r.fetches() != maxBodyAttempts {
		t.Errorf("FetchMessages calls after the final failure = %d, want %d", r.fetches(), maxBodyAttempts)
	}
	if !r.windowDone() || r.finalFailures() != 1 {
		t.Errorf("window_done = %v, final failures = %d; want true, 1", r.windowDone(), r.finalFailures())
	}
}

// A message gone from the server and a body the renderer cannot parse are
// recorded as failures; the rest of the batch is still stored.
func TestPrefetchGoneAndUnparsable(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	gone := r.add(3*oneDay, "gone")
	broken := r.add(2*oneDay, "broken")
	good := r.add(oneDay, "good")
	r.track(true)
	r.fk.ExternalExpunge("INBOX", gone.UID)
	r.mail = rawBreaker{Store: r.fk, broken: map[uint32]bool{broken.UID: true}}
	r.step(0)
	if !r.has(good) || r.has(gone) || r.has(broken) {
		t.Errorf("bodies cached: good %v, gone %v, broken %v; want true, false, false", r.has(good), r.has(gone), r.has(broken))
	}
	if r.offered(gone) || r.offered(broken) {
		t.Error("a failed message is offered again before its retry gap")
	}
}

// Errors about the connection or the account record nothing and end the
// batch with the retry wait; the message is fetched once they pass.
func TestPrefetchTransientErrorsRecordNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fail func(*prefetchRig)
		heal func(*prefetchRig)
	}{
		{
			name: "server unavailable",
			fail: func(r *prefetchRig) { r.fk.Fail("FetchMessages", fmt.Errorf("%w: reset", mailstore.ErrUnavailable)) },
			heal: func(r *prefetchRig) { r.fk.Fail("FetchMessages", nil) },
		},
		{
			name: "busy pool",
			fail: func(r *prefetchRig) { r.fk.SetBusy(true) },
			heal: func(r *prefetchRig) { r.fk.SetBusy(false) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newPrefetchRig(t)
			m := r.add(oneDay, "waiting")
			r.track(true)
			tt.fail(r)
			r.step(retryFloor)
			if r.has(m) || !r.offered(m) {
				t.Errorf("after the error: body cached = %v, offered = %v; want false, true", r.has(m), r.offered(m))
			}
			tt.heal(r)
			r.step(0)
			if !r.has(m) {
				t.Error("body not fetched once the error passed")
			}
			if n := r.fk.BackgroundCalls("FetchMessages"); n != 2 {
				t.Errorf("background FetchMessages calls = %d, want 2", n)
			}
		})
	}
}

// With the day's budget spent nothing is fetched until 00:00 UTC; a batch
// adds len(raw) of each body to the day it ran on.
func TestPrefetchBudget(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	a := r.add(oneDay, "a")
	b := r.add(2*oneDay, "b")
	r.track(true)
	ctx := t.Context()
	if _, err := r.st.AddBudget(ctx, "acme", "2026-11-03", r.set.DailyDownload); err != nil {
		t.Fatal(err)
	}
	r.step(14 * time.Hour)
	if r.fetches() != 0 {
		t.Fatalf("FetchMessages calls with the budget spent = %d, want 0", r.fetches())
	}
	r.clk.Set(time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC))
	r.step(0)
	var want int64
	for _, x := range []ref.Ref{a, b} {
		body, ok, err := r.st.Body(ctx, x)
		if err != nil || !ok {
			t.Fatalf("Body(%v) = %v, %v", x, ok, err)
		}
		want += int64(len(body.Raw))
	}
	got, err := r.st.Budget(ctx, "acme", "2026-11-04")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("budget used on 2026-11-04 = %d, want %d", got, want)
	}
}

// The prefetcher pauses the whole account while one of its folders is
// auth_required or reconnecting, skips a folder in error alone, and ignores
// another account's trouble (integration note 12).
func TestPrefetchPausesOnWatchTrouble(t *testing.T) {
	t.Parallel()
	tests := []struct {
		account string
		folder  string
		state   watch.Status
		// wait is what the step returns; fetched whether INBOX's body came.
		wait    time.Duration
		fetched bool
	}{
		{account: "acme", folder: "INBOX", state: watch.StateAuthRequired, wait: retryFloor},
		{account: "acme", folder: "Archive", state: watch.StateAuthRequired, wait: retryFloor},
		{account: "acme", folder: "INBOX", state: watch.StateReconnecting, wait: retryFloor},
		{account: "acme", folder: "INBOX", state: watch.StateError, wait: prefetchIdle},
		{account: "acme", folder: "Archive", state: watch.StateError, wait: 0, fetched: true},
		{account: "acme", folder: "INBOX", state: watch.StateWatching, wait: 0, fetched: true},
		{account: "acme", folder: "INBOX", state: watch.StatePolling, wait: 0, fetched: true},
		{account: "other", folder: "INBOX", state: watch.StateAuthRequired, wait: 0, fetched: true},
	}
	for _, tt := range tests {
		t.Run(tt.account+" "+tt.folder+" "+string(tt.state), func(t *testing.T) {
			t.Parallel()
			r := newPrefetchRig(t)
			stateDB, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stateDB.Close() })
			state := watch.NewState(stateDB)
			if err := state.SetStatus(t.Context(), tt.account, tt.folder, watch.ModeIdle, tt.state, ""); err != nil {
				t.Fatal(err)
			}
			r.p.o.State = state
			m := r.add(oneDay, "m")
			r.track(true)
			r.step(tt.wait)
			if got := r.has(m); got != tt.fetched {
				t.Errorf("body cached = %v, want %v", got, tt.fetched)
			}
			if !tt.fetched && r.fetches() != 0 {
				t.Errorf("FetchMessages calls = %d, want 0", r.fetches())
			}
			if !tt.fetched && r.windowDone() {
				t.Error("window_done set for a folder that was not prefetched")
			}
		})
	}
}

// After the cap took window bodies, the prefetcher fetches nothing while
// the file is at or over 80% of max_size, still clearing window_done, and
// resumes and drops the hold once the file is under 80%.
func TestPrefetchCapHysteresis(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "evicted under the cap")
	r.track(true)
	if err := r.st.SetWindowDone(t.Context(), "acme", "INBOX", true); err != nil {
		t.Fatal(err)
	}
	r.st.evict.hold.Store(true)
	size, err := r.st.SizeBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.max = size * 100 / refetchPercent // the file is exactly at 80%
	r.step(prefetchIdle)
	if r.fetches() != 0 || r.windowDone() || !r.st.evict.hold.Load() {
		t.Errorf("at 80%%: calls = %d, window_done = %v, hold = %v; want 0, false, true", r.fetches(), r.windowDone(), r.st.evict.hold.Load())
	}
	r.max = size * 2
	r.step(0)
	if !r.has(m) || r.st.evict.hold.Load() {
		t.Errorf("under 80%%: body cached = %v, hold = %v; want true, false", r.has(m), r.st.evict.hold.Load())
	}
}

// A prefetched body that leaves the file over max_size kicks the evictor.
func TestPrefetchKicksTheEvictor(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	r.add(oneDay, "m")
	r.track(true)
	r.max = 1
	r.step(0)
	if n := len(r.st.evict.kickChan()); n != 1 {
		t.Errorf("kicks queued = %d, want 1", n)
	}
}

// run fetches until ctx ends, and returns then.
func TestPrefetchRun(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "m")
	r.track(true)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.p.run(ctx) })
	pollUntil(t, "the prefetched body", func() bool { return r.has(m) })
	cancel()
	wg.Wait()
}
