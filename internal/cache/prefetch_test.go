package cache

import (
	"context"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
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
