package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/watch"
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
	return r.addSized(age, subject, 100)
}

// addSized is add with the header row's message size, the size the
// prefetcher admits a body by.
func (r *prefetchRig) addSized(age time.Duration, subject string, size int64) ref.Ref {
	r.t.Helper()
	at := r.clk.Now().Add(-age)
	got := r.fk.Add(fake.Message{Folder: "INBOX", Date: at, InternalDate: at, Subject: subject, Body: "text of " + subject})
	row := Row{Account: got.Account, Folder: got.Folder, UIDValidity: got.UIDValidity, UID: got.UID, InternalDate: at, Date: at, Size: size, Subject: subject}
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

// Bodies inside the window, its start included, are fetched in one step,
// one background download each so the budget is checked before each;
// older ones are not.
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
	if n := r.fk.BackgroundCalls("FetchMessages"); n != 2 || r.fetches() != 2 {
		t.Errorf("FetchMessages calls = %d, background %d; want 2 background calls, one per body", r.fetches(), n)
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

// batchTweak hands out the fake's batch results as tweak edits them, and
// records how many raw bytes the results it returned carry.
type batchTweak struct {
	*fake.Store
	tweak func(refs []ref.Ref, out []mailstore.MessageResult) []mailstore.MessageResult
	mu    *sync.Mutex
	total *int64
}

func (b batchTweak) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out := b.tweak(refs, b.Store.FetchMessages(ctx, refs))
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, res := range out {
		if res.Message != nil {
			*b.total += int64(len(res.Message.Raw))
		}
	}
	return out
}

// Every body received from the server is charged len(raw), whether it
// then parses and stores or not, and a step that ends early, or a download
// answer the prefetcher rejects, still charges the bodies received. A
// result carrying only an error costs nothing.
func TestPrefetchBudgetChargesEveryBodyReceived(t *testing.T) {
	t.Parallel()
	type tweak = func(refs []ref.Ref, out []mailstore.MessageResult) []mailstore.MessageResult
	tests := []struct {
		name string
		// rig breaks the batch at middle, the second of its three messages.
		rig func(r *prefetchRig, middle ref.Ref) tweak
		// want is the step's wait.
		want time.Duration
	}{
		{
			name: "malformed body",
			rig: func(_ *prefetchRig, middle ref.Ref) tweak {
				return func(refs []ref.Ref, out []mailstore.MessageResult) []mailstore.MessageResult {
					for i, x := range refs {
						if x.UID == middle.UID && out[i].Message != nil {
							msg := *out[i].Message
							msg.Raw = []byte("Subject: hi\r\nno colon here\r\n\r\nbody\r\n")
							out[i].Message = &msg
						}
					}
					return out
				}
			},
			want: 0,
		},
		{
			name: "busy mid-batch",
			rig: func(_ *prefetchRig, middle ref.Ref) tweak {
				return func(refs []ref.Ref, out []mailstore.MessageResult) []mailstore.MessageResult {
					for i, x := range refs {
						if x.UID == middle.UID {
							out[i] = mailstore.MessageResult{Err: mailstore.ErrBusy}
						}
					}
					return out
				}
			},
			want: retryFloor,
		},
		{
			name: "later item fails to store",
			rig: func(r *prefetchRig, middle ref.Ref) tweak {
				if _, err := r.st.db.ExecContext(r.t.Context(), fmt.Sprintf(
					"CREATE TRIGGER no_body BEFORE INSERT ON bodies WHEN NEW.msg = %d BEGIN SELECT RAISE(ABORT, 'disk full'); END", r.msgID(middle))); err != nil {
					r.t.Fatal(err)
				}
				return func(_ []ref.Ref, out []mailstore.MessageResult) []mailstore.MessageResult { return out }
			},
			want: retryFloor,
		},
		{
			// Each download asks for one body, so a second answer is the
			// wrong number. Both bodies received are charged.
			name: "wrong number of results",
			rig: func(*prefetchRig, ref.Ref) tweak {
				return func(_ []ref.Ref, out []mailstore.MessageResult) []mailstore.MessageResult { return append(out, out...) }
			},
			want: retryFloor,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newPrefetchRig(t)
			r.add(3*oneDay, "oldest")
			middle := r.add(2*oneDay, "middle")
			r.add(oneDay, "newest")
			r.track(true)
			var total int64
			r.mail = batchTweak{Store: r.fk, tweak: tt.rig(r, middle), mu: &sync.Mutex{}, total: &total}
			r.step(tt.want)
			if total == 0 {
				t.Fatal("the batch returned no bodies")
			}
			got, err := r.st.Budget(t.Context(), "acme", "2026-11-03")
			if err != nil {
				t.Fatal(err)
			}
			if got != total {
				t.Errorf("budget used = %d, want %d (len(raw) of every body received)", got, total)
			}
		})
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

// msgID is the cache row id of x.
func (r *prefetchRig) msgID(x ref.Ref) int64 {
	r.t.Helper()
	var id int64
	if err := r.st.db.QueryRowContext(r.t.Context(), "SELECT id FROM messages WHERE account = ? AND folder = ? AND uid = ?", x.Account, x.Folder, x.UID).Scan(&id); err != nil {
		r.t.Fatal(err)
	}
	return id
}

// An eviction of a window body between the prefetcher's check and its
// write leaves window_done false: completion is one guarded update that
// sees the body gone.
func TestCompleteWindowLosesToAnEviction(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "kept")
	r.track(true)
	r.step(0)
	ctx := t.Context()
	now := r.clk.Now()
	since := now.Add(-r.set.PrefetchBodies)
	left, err := r.st.BodylessSince(ctx, "acme", "INBOX", since, maxBodyAttempts, now.Add(windowCheckAhead), 1)
	if err != nil || len(left) != 0 {
		t.Fatalf("window check = %v, %v; want nothing left", left, err)
	}
	if _, _, err := r.st.dropBodies(ctx, []evictVictim{{
		msg: r.msgID(m), size: 1, account: "acme", folder: "INBOX", uidValidity: m.UIDValidity, uid: m.UID, winStart: since.Unix(), inWindow: true,
	}}); err != nil {
		t.Fatal(err)
	}
	set, err := r.st.CompleteWindow(ctx, "acme", "INBOX", since, maxBodyAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if set || r.windowDone() {
		t.Errorf("CompleteWindow after the eviction = %v, window_done = %v; want false, false", set, r.windowDone())
	}
}

// CompleteWindow sets window_done only while no row of the folder inside
// the window lacks a body, a final failure counting as done, and reports
// whether it set it.
func TestCompleteWindow(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	ctx := t.Context()
	now := r.clk.Now()
	since := now.Add(-r.set.PrefetchBodies)
	r.add(91*oneDay, "outside")
	failing := r.add(2*oneDay, "failing")
	r.track(true)
	if r.completeWindow(since) {
		t.Fatal("set with a window body never fetched")
	}
	for attempt := 1; attempt < maxBodyAttempts; attempt++ {
		if _, err := r.st.RecordBodyFailure(ctx, failing, failFetch); err != nil {
			t.Fatal(err)
		}
		if r.completeWindow(since) {
			t.Fatalf("set with a failure of %d attempts, not final", attempt)
		}
	}
	if _, err := r.st.RecordBodyFailure(ctx, failing, failFetch); err != nil {
		t.Fatal(err)
	}
	if !r.completeWindow(since) {
		t.Fatal("not set with only a final failure and a body outside the window left")
	}
	if set, err := r.st.CompleteWindow(ctx, "acme", "Archive", since, maxBodyAttempts); err != nil || set {
		t.Errorf("CompleteWindow(Archive) with no progress row = %v, %v; want false", set, err)
	}
}

// completeWindow runs CompleteWindow on INBOX and checks that what it
// reports matches window_done.
func (r *prefetchRig) completeWindow(since time.Time) bool {
	r.t.Helper()
	set, err := r.st.CompleteWindow(r.t.Context(), "acme", "INBOX", since, maxBodyAttempts)
	if err != nil {
		r.t.Fatal(err)
	}
	if set != r.windowDone() {
		r.t.Fatalf("CompleteWindow = %v but window_done = %v", set, r.windowDone())
	}
	return set
}

// A prefetcher that found nothing to fetch, because no folder's backfill
// had finished, starts fetching as soon as one finishes, without waiting
// out prefetchIdle. The clock never moves, so only that wake can start it.
func TestPrefetchWakesWhenABackfillCompletes(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = 90 * oneDay })
	one, two := fs.Add(acctMsg("INBOX", "one")), fs.Add(acctMsg("INBOX", "two"))
	r.pass("INBOX")
	stall := newAcctStallMail(fs)
	r.mail = stall
	r.start()
	select {
	case <-stall.entered:
	case <-time.After(acctWait):
		t.Fatal("the backfill never fetched a batch")
	}
	r.clock.expectAfter(t, prefetchIdle) // the prefetcher idles with nothing done
	if r.fs.Calls("FetchMessages") != 0 {
		t.Fatal("bodies fetched before the backfill finished")
	}
	close(stall.release)
	r.waitBackfilled("INBOX")
	r.eventually("the window bodies", func() (bool, error) {
		return hasBody(t, r.store, one) && hasBody(t, r.store, two), nil
	})
}

// prefetchDegraded adds a message with invoice.pdf to the fake's INBOX,
// caches its row as a read without its body structure would, flagged
// \Flagged in the cache, and tracks INBOX with the backfill done.
func (r *prefetchRig) prefetchDegraded() ref.Ref {
	r.t.Helper()
	at := r.clk.Now().Add(-oneDay)
	got := r.fk.Add(fake.Message{
		Folder: "INBOX", Date: at, InternalDate: at, Subject: "invoice", Body: "see attached",
		Attachments: []fake.Attachment{{Part: "2", Filename: "invoice.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}},
	})
	row := Row{
		Account: got.Account, Folder: got.Folder, UIDValidity: got.UIDValidity, UID: got.UID,
		InternalDate: at, Date: at, Size: 100, Subject: "invoice", Flags: []string{`\Flagged`}, AttachmentsUnknown: true,
	}
	if err := r.st.PutRows(r.t.Context(), []Row{row}); err != nil {
		r.t.Fatal(err)
	}
	r.track(true)
	return got
}

// checkRepaired checks that x's cached row knows invoice.pdf, keeps the
// row's flags, and that a get answered from the cache, the row's Message
// over its body, returns the attachment.
func (r *prefetchRig) checkRepaired(x ref.Ref) {
	r.t.Helper()
	cached, ok, err := r.st.Row(r.t.Context(), x)
	if err != nil || !ok {
		r.t.Fatalf("Row: %v, %v", ok, err)
	}
	if cached.AttachmentsUnknown || !cached.HasAttachment || len(cached.Attachments) != 1 || cached.Attachments[0].Filename != "invoice.pdf" {
		r.t.Errorf("row attachments: unknown %v, has %v, %+v; want invoice.pdf known", cached.AttachmentsUnknown, cached.HasAttachment, cached.Attachments)
	}
	if !slices.Equal(cached.Flags, []string{`\Flagged`}) {
		r.t.Errorf("row flags = %v, want the row's own", cached.Flags)
	}
	body, ok, err := r.st.Body(r.t.Context(), x)
	if err != nil || !ok {
		r.t.Fatalf("Body: %v, %v", ok, err)
	}
	if got := cached.Message(body).Attachments; len(got) != 1 || got[0].Filename != "invoice.pdf" {
		r.t.Errorf("a cached get's attachments = %+v, want invoice.pdf", got)
	}
}

// A prefetched body of a message the cache holds without its body
// structure repairs the row's attachments from the full read, keeping the
// row's flags, so has:attachment and filename: match it afterwards.
func TestPrefetchRepairsAttachmentsUnknown(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	got := r.prefetchDegraded()
	r.step(0)
	if !r.has(got) {
		t.Fatal("no body prefetched")
	}
	r.checkRepaired(got)
}

// The body and the attachment repair commit together. When the repair
// fails, no body is stored either, so the message stays in the prefetch
// queue and the next step fetches and repairs it.
func TestPrefetchRetriesAFailedRepair(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	got := r.prefetchDegraded()
	dsn := "file:" + r.st.path + "?" + url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	const trigger = `CREATE TRIGGER prefetch_fail_repair BEFORE UPDATE OF attachments_unknown ON messages
		WHEN OLD.attachments_unknown = 1 AND NEW.attachments_unknown = 0
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`
	if _, err := conn.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	r.p.step(t.Context())
	if r.has(got) {
		t.Error("a body was stored beside a failed repair")
	}
	if _, err := conn.ExecContext(t.Context(), "DROP TRIGGER prefetch_fail_repair"); err != nil {
		t.Fatal(err)
	}
	fetches := r.fetches()
	r.p.step(t.Context())
	if r.fetches() != fetches+1 || !r.has(got) {
		t.Fatalf("next step: %d fetches, body %v; want the message fetched and stored", r.fetches()-fetches, r.has(got))
	}
	r.checkRepaired(got)
}

// A prefetch cancelled as its repair starts stores no body either, so a
// later step with a live context fetches and repairs the message.
func TestPrefetchRetriesACancelledRepair(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	got := r.prefetchDegraded()
	ctx, cancel := context.WithCancel(t.Context())
	r.st.beforeRepair = cancel
	r.p.step(ctx)
	r.st.beforeRepair = nil
	if r.has(got) {
		t.Error("a body was stored beside a cancelled repair")
	}
	fetches := r.fetches()
	r.p.step(t.Context())
	if r.fetches() != fetches+1 || !r.has(got) {
		t.Fatalf("next step: %d fetches, body %v; want the message fetched and stored", r.fetches()-fetches, r.has(got))
	}
	r.checkRepaired(got)
}
