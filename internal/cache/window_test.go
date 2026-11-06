package cache

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
)

// acctWindow is the prefetch window the window tests run with.
const acctWindow = 90 * 24 * time.Hour

// windowSince is a query's lower date bound inside acctWindow.
var windowSince = acctStart.Add(-24 * time.Hour)

// runBuilder runs the rig's builder alone, without the backfill or the
// prefetcher, until the end of the test, and waits until it has opened its
// group and tracked the account's folders.
func (r *acctRig) runBuilder() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.a.runBuilder(ctx)
	}()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(acctWait):
			r.t.Error("the builder did not return after cancel")
		}
	})
	r.eventually("the builder to open its group", func() (bool, error) { return r.a.applied.Load() >= 0, nil })
}

// markWindowDone records the folder's backfill and prefetch window as done.
func (r *acctRig) markWindowDone(folder string) {
	r.t.Helper()
	p := r.progress(folder)
	p.BackfillDone, p.WindowDone = true, true
	if err := r.store.PutProgress(r.t.Context(), p); err != nil {
		r.t.Fatal(err)
	}
}

// windowCovers is WindowCovers of folder from windowSince.
func (r *acctRig) windowCovers(folder string) bool {
	r.t.Helper()
	ok, err := r.a.WindowCovers(r.t.Context(), []string{folder}, windowSince)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// waitWindowDone waits until the prefetcher has completed the folder's window.
func (r *acctRig) waitWindowDone(folder string) {
	r.t.Helper()
	r.eventually("the window of "+folder, func() (bool, error) {
		p, ok, err := r.store.Progress(r.t.Context(), r.acct.Name, folder)
		return ok && p.WindowDone, err
	})
}

// A new message inside the prefetch window that has no body makes its
// folder's window incomplete in the same commit as its row, and wakes the
// prefetcher, so a text search never answers complete without its body.
// A new message older than the window, or one whose body the cache already
// holds, leaves the window as it was and wakes nothing.
func TestBuilderNewMailInTheWindowClearsWindowDone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		age      time.Duration // of the new message's date, before acctStart
		haveBody bool          // the cache holds its body before the event
		wantDone bool
	}{
		{name: "in the window", age: time.Hour},
		{name: "older than the window", age: acctWindow + 24*time.Hour, wantDone: true},
		{name: "in the window with its body cached", age: time.Hour, haveBody: true, wantDone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", false)
			r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = acctWindow })
			r.pass("INBOX")
			r.runBuilder()
			r.markWindowDone("INBOX")

			m := acctMsg("INBOX", "code")
			m.Date = acctStart.Add(-tt.age)
			rf := fs.Add(m)
			if tt.haveBody {
				r.putRows(Row{
					Account: "acme", Folder: "INBOX", UIDValidity: rf.UIDValidity, UID: rf.UID, Subject: "code",
					MessageID: "code@example.com", InternalDate: m.Date, Date: m.Date,
				})
				if _, err := r.store.PutBody(t.Context(), rf, Body{Raw: []byte("Subject: code\r\n\r\nhello code\r\n")}, true); err != nil {
					t.Fatal(err)
				}
			}
			r.pass("INBOX")
			r.waitApplied()

			if _, ok := r.row(rf); !ok {
				t.Fatal("the new message is not cached")
			}
			if got := r.progress("INBOX").WindowDone; got != tt.wantDone {
				t.Errorf("window_done = %v, want %v", got, tt.wantDone)
			}
			if got := r.windowCovers("INBOX"); got != tt.wantDone {
				t.Errorf("WindowCovers = %v, want %v", got, tt.wantDone)
			}
			wantWake := 0
			if !tt.wantDone {
				wantWake = 1
			}
			if n := len(r.a.prefetchWake); n != wantWake {
				t.Errorf("prefetcher wakes queued = %d, want %d", n, wantWake)
			}
		})
	}
}

// With every worker running, a new message's body arrives without the
// prefetcher's idle minute passing, and the window covers the folder again
// only once that body is stored.
func TestNewMailBodyArrivesBeforeTheWindowCovers(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	stall := newAcctBodyStall(fs)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.mail = stall
	r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = acctWindow })
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	r.waitWindowDone("INBOX")

	rf := r.add("INBOX", "code")[0]
	r.waitApplied()
	if r.windowCovers("INBOX") {
		t.Fatal("WindowCovers = true once the new message is applied, before its body is stored")
	}
	select {
	case <-stall.entered:
	case <-time.After(acctWait):
		t.Fatal("the prefetcher did not fetch the new body without its idle minute passing")
	}
	if r.windowCovers("INBOX") {
		t.Error("WindowCovers = true while the new body is still being fetched")
	}
	close(stall.release)
	r.eventually("the new body and the window", func() (bool, error) {
		return hasBody(t, r.store, rf) && r.windowCovers("INBOX"), nil
	})
}

// A new message arriving once the day's budget is spent keeps the window
// uncovered until the next UTC day's budget fetches its body: the
// prefetcher stops before it would look at the window.
func TestNewMailWhileTheBudgetIsSpentLeavesTheWindowUncovered(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = acctWindow })
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	r.waitWindowDone("INBOX")
	if _, err := r.store.AddBudget(t.Context(), "acme", "2026-11-02", r.cacheSettings().DailyDownload); err != nil {
		t.Fatal(err)
	}

	rf := r.add("INBOX", "code")[0]
	r.waitApplied()
	r.clock.expectAfter(t, untilUTCMidnight(acctStart)) // the woken prefetcher found the budget spent
	if r.windowCovers("INBOX") {
		t.Error("WindowCovers = true with the new body waiting for the next budget day")
	}
	if hasBody(t, r.store, rf) {
		t.Fatal("the body was fetched past a spent budget")
	}
	r.clock.Advance(untilUTCMidnight(acctStart))
	r.eventually("the new body and the window on the next day", func() (bool, error) {
		return hasBody(t, r.store, rf) && r.windowCovers("INBOX"), nil
	})
}

// evictDuring makes every insert into messages for folder first clear the
// folder's window_done, as an eviction landing at that moment would,
// through a trigger added on a second connection. The returned function
// removes it.
func (r *acctRig) evictDuring(folder string) func() {
	r.t.Helper()
	dsn := "file:" + r.cachePath + "?" + url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		r.t.Fatal(err)
	}
	trigger := `CREATE TRIGGER acct_evict_during AFTER INSERT ON messages
		WHEN NEW.folder = '` + folder + `'
		BEGIN UPDATE folders SET window_done = 0 WHERE account = NEW.account AND folder = NEW.folder; END`
	if _, err := conn.ExecContext(r.t.Context(), trigger); err != nil {
		r.t.Fatalf("adding the evicting trigger: %v", err)
	}
	return func() {
		r.t.Helper()
		if _, err := conn.ExecContext(r.t.Context(), "DROP TRIGGER acct_evict_during"); err != nil {
			r.t.Fatalf("dropping the evicting trigger: %v", err)
		}
		_ = conn.Close() // a close error changes nothing for the test
	}
}

// A backfill batch that commits after an eviction cleared its folder's
// window_done, between the batch's progress read and its progress write,
// keeps the clear: commitBatch writes only the backfill's own columns.
func TestCommitBatchKeepsAConcurrentWindowClear(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	one := fs.Add(acctMsg("INBOX", "one"))
	two := fs.Add(acctMsg("INBOX", "two"))
	r.pass("INBOX")
	if err := r.a.track(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetWindowDone(t.Context(), "acme", "INBOX", true); err != nil {
		t.Fatal(err)
	}
	p := r.progress("INBOX")
	w := &walk{folder: "INBOX", uidValidity: two.UIDValidity, epoch: r.a.epoch.Load(), baseline: p.BaselineUIDNext, low: p.BackfillLowUID, uids: []uint32{two.UID, one.UID}}
	row := Row{Account: "acme", Folder: "INBOX", UIDValidity: two.UIDValidity, UID: two.UID, Subject: "two", MessageID: "two@example.com", InternalDate: acctStart, Date: acctStart}
	defer r.evictDuring("INBOX")()

	if _, _, err := r.a.commitBatch(t.Context(), w, []uint32{two.UID}, []Row{row}, nil); err != nil {
		t.Fatal(err)
	}
	got := r.progress("INBOX")
	if got.WindowDone {
		t.Error("window_done = true, want the eviction's clear kept")
	}
	if got.BackfillLowUID != two.UID || got.BackfillDone {
		t.Errorf("progress = %+v, want low UID %d, not done", got, two.UID)
	}
}

// evictBeforeRewrite makes a write that inserts over the folder's existing
// progress row, as a read-then-rewrite of the whole row does, first clear
// its window_done, as an eviction landing between the read and the write
// would. It reports, through the returned function, whether that clear
// ran, and removes the trigger.
func (r *acctRig) evictBeforeRewrite() func() bool {
	r.t.Helper()
	dsn := "file:" + r.cachePath + "?" + url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE acct_evictions (folder TEXT NOT NULL)`,
		`CREATE TRIGGER acct_evict_before_rewrite BEFORE INSERT ON folders
		 WHEN EXISTS (SELECT 1 FROM folders WHERE account = NEW.account AND folder = NEW.folder)
		 BEGIN
		   UPDATE folders SET window_done = 0 WHERE account = NEW.account AND folder = NEW.folder;
		   INSERT INTO acct_evictions (folder) VALUES (NEW.folder);
		 END`,
	} {
		if _, err := conn.ExecContext(r.t.Context(), q); err != nil {
			r.t.Fatalf("adding the evicting trigger: %v", err)
		}
	}
	return func() bool {
		r.t.Helper()
		var n int
		if err := conn.QueryRowContext(r.t.Context(), "SELECT count(*) FROM acct_evictions").Scan(&n); err != nil {
			r.t.Fatal(err)
		}
		if _, err := conn.ExecContext(r.t.Context(), "DROP TRIGGER acct_evict_before_rewrite"); err != nil {
			r.t.Fatalf("dropping the evicting trigger: %v", err)
		}
		_ = conn.Close() // a close error changes nothing for the test
		return n > 0
	}
}

// The builder's progress writes that run on every commit, stampApplied,
// and on new mail skipped in error, recordRewalks, set only their own
// columns: an eviction's window_done clear that lands while one runs is
// never undone by a rewrite of the whole progress row read before the
// clear.
func TestBuilderProgressWritesKeepAConcurrentWindowClear(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write func(r *acctRig) error
		check func(t *testing.T, p Progress)
	}{
		{
			name:  "stampApplied",
			write: func(r *acctRig) error { return r.a.stampApplied(r.t.Context(), map[string]bool{"INBOX": true}) },
			check: func(t *testing.T, p Progress) {
				t.Helper()
				if !p.LastAppliedAt.Equal(acctStart.Add(time.Minute)) || !p.BackfillDone {
					t.Errorf("progress = %+v, want last applied at %v and the backfill still done", p, acctStart.Add(time.Minute))
				}
			},
		},
		{
			name: "recordRewalks",
			write: func(r *acctRig) error {
				return r.a.recordRewalks(r.t.Context(), map[string]skipped{"INBOX": {uidValidity: r.progress("INBOX").UIDValidity, high: 40}})
			},
			check: func(t *testing.T, p Progress) {
				t.Helper()
				if p.BaselineUIDNext != 41 || p.BackfillLowUID != 0 || p.BackfillDone {
					t.Errorf("progress = %+v, want a new walk below 41", p)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", false)
			r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			r.pass("INBOX")
			if err := r.a.track(t.Context()); err != nil {
				t.Fatal(err)
			}
			r.markWindowDone("INBOX")
			r.clock.Advance(time.Minute)
			evicted := r.evictBeforeRewrite()

			if err := tt.write(r); err != nil {
				t.Fatal(err)
			}
			cleared := evicted()
			got := r.progress("INBOX")
			if cleared && got.WindowDone {
				t.Errorf("window_done = true, want the clear an eviction made during %s kept", tt.name)
			}
			if !cleared && !got.WindowDone {
				t.Errorf("window_done = false, want %s to leave it alone", tt.name)
			}
			tt.check(t, got)
		})
	}
}

// The narrow progress writers change only their own columns, and none
// creates a row for a folder without one.
func TestNarrowProgressWritesKeepTheOtherColumns(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	ctx := t.Context()
	base := Progress{Account: "acme", Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 10, BackfillLowUID: 5, WindowDone: true, LastAppliedAt: acctStart}
	if err := s.PutProgress(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastApplied(ctx, "acme", "INBOX", acctStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackfill(ctx, "acme", "INBOX", 3, true); err != nil {
		t.Fatal(err)
	}
	want := base
	want.LastAppliedAt, want.BackfillLowUID, want.BackfillDone = acctStart.Add(time.Hour), 3, true
	if got, ok, err := s.Progress(ctx, "acme", "INBOX"); err != nil || !ok || got != want {
		t.Errorf("progress = %+v, %v, %v; want %+v", got, ok, err, want)
	}
	if err := s.RestartBackfill(ctx, "acme", "INBOX", 20); err != nil {
		t.Fatal(err)
	}
	want.BaselineUIDNext, want.BackfillLowUID, want.BackfillDone = 20, 0, false
	if got, ok, err := s.Progress(ctx, "acme", "INBOX"); err != nil || !ok || got != want {
		t.Errorf("progress after a restarted walk = %+v, %v, %v; want %+v", got, ok, err, want)
	}
	if err := s.SetLastApplied(ctx, "acme", "Archive", acctStart); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackfill(ctx, "acme", "Archive", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RestartBackfill(ctx, "acme", "Archive", 5); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Progress(ctx, "acme", "Archive"); err != nil || ok {
		t.Errorf("Archive progress = %v, %v; want none", ok, err)
	}
}
