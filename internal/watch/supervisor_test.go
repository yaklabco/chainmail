package watch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/provider"
)

// supWait bounds every wait in these tests. It is a deadline for a test
// that hangs, never a sleep: each wait returns as soon as its condition holds.
const supWait = 5 * time.Second

// supAllMail is the fake Gmail account's \All folder.
const supAllMail = "[Gmail]/All Mail"

// supHint is the hint the auth state gives for the test account.
const supHint = "run: chainmail secret set accounts/acme"

var supStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// supClock is a Clock whose timers fire only when the test advances it. It
// counts After calls by duration, so a test can wait until the supervisor
// waits on a timer before it advances past it.
type supClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []supTimer
	afters  map[time.Duration]int // After calls, by duration
	taken   map[time.Duration]int // After calls expectAfter has accounted for
	changed chan struct{}         // closed and replaced on every After call
}

type supTimer struct {
	at time.Time
	ch chan time.Time
}

func newSupClock() *supClock {
	return &supClock{
		now: supStart, afters: map[time.Duration]int{}, taken: map[time.Duration]int{},
		changed: make(chan struct{}),
	}
}

func (c *supClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *supClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.afters[d]++
	if d <= 0 {
		ch <- c.now
	} else {
		c.timers = append(c.timers, supTimer{at: c.now.Add(d), ch: ch})
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return ch
}

// Advance moves the clock forward by d and fires every timer now due.
func (c *supClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var kept []supTimer
	for _, tm := range c.timers {
		if tm.at.After(c.now) {
			kept = append(kept, tm)
			continue
		}
		tm.ch <- c.now
	}
	c.timers = kept
}

// expectAfter blocks until the code under test has called After(d) once
// more than earlier expectAfter calls for d have accounted for.
func (c *supClock) expectAfter(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.NewTimer(supWait)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		ok := c.afters[d] > c.taken[d]
		if ok {
			c.taken[d]++
		}
		changed := c.changed
		seen := fmt.Sprint(c.afters)
		c.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("no After(%v) call; After calls by duration: %s", d, seen)
		}
	}
}

// supRig runs one Supervisor against a fake store and a real state.db.
type supRig struct {
	t     *testing.T
	fs    *fake.Store
	store mailstore.Store // what Store hands out; set it before start
	clock *supClock
	log   *Log
	state *State
	sup   *Supervisor
	// acquired counts Store calls, held the ones not yet released.
	acquired atomic.Int64
	held     atomic.Int64
	pos      int64 // waitFor's position in the log
	cancel   context.CancelFunc
	done     chan struct{}
}

func newSupRig(t *testing.T, fs *fake.Store, acct config.ResolvedAccount, tweak ...func(*SupervisorOptions)) *supRig {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	r := &supRig{t: t, fs: fs, store: fs, clock: newSupClock()}
	r.log = NewLog(conn, r.clock.Now, nil)
	r.state = NewState(conn)
	o := SupervisorOptions{
		Account: acct,
		Settings: config.WatchSettings{
			Retention: 168 * time.Hour, MaxEvents: 200000,
			ExpungeSweep: time.Hour, IdleFallbackPoll: 30 * time.Second,
		},
		Store:        r.openStore,
		Engine:       NewEngine(conn, r.log, r.state, EngineOptions{Now: r.clock.Now}),
		Log:          r.log,
		State:        r.state,
		AuthRequired: func(string) (bool, string) { return false, "" },
		Clock:        r.clock,
		Jitter:       func() float64 { return 0.5 }, // Backoff(n, 0.5) is exactly 1s << n
	}
	for _, f := range tweak {
		f(&o)
	}
	r.sup = NewSupervisor(o)
	// The store ends each IDLE on the test clock, as a real store ends it
	// at Idle's limit. A test can restore time.After with SetAfter(nil).
	fs.SetAfter(r.clock.After)
	return r
}

// openStore is SupervisorOptions.Store: it hands out r.store and counts
// acquisitions and releases, so a test can check none is held while idle.
func (r *supRig) openStore(context.Context) (mailstore.Store, func(), error) {
	r.acquired.Add(1)
	r.held.Add(1)
	var once sync.Once
	return r.store, func() { once.Do(func() { r.held.Add(-1) }) }, nil
}

// start runs the supervisor until the test ends.
func (r *supRig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		r.sup.Run(ctx)
	}()
	r.t.Cleanup(r.stop)
}

// stop cancels Run and waits for it to return. Calling it twice is safe.
func (r *supRig) stop() {
	r.t.Helper()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(supWait):
		r.t.Fatal("Run did not return after cancel")
	}
}

// scan returns the first event after *after that pred accepts, moving
// *after past every event it reads.
func (r *supRig) scan(desc string, after *int64, pred func(Event) bool) Event {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), supWait)
	defer cancel()
	for {
		page, err := r.log.Read(ctx, Query{After: *after, Limit: 1000})
		if err != nil {
			r.t.Fatalf("waiting for %s: %v", desc, err)
		}
		for _, e := range page.Events {
			if e.ID > 0 {
				*after = e.ID
			}
			if pred(e) {
				return e
			}
		}
		*after = page.Next
		if err := r.log.Wait(ctx, *after); err != nil {
			r.t.Fatalf("waiting for %s: %v; events so far:%s", desc, err, supDump(r.all()))
		}
	}
}

// waitFor returns the next event pred accepts after the last one waitFor
// returned, for events that arrive in a known order.
func (r *supRig) waitFor(desc string, pred func(Event) bool) Event {
	r.t.Helper()
	return r.scan(desc, &r.pos, pred)
}

// waitSeen returns the first event in the whole log that pred accepts, for
// events of concurrent folders whose order is not fixed.
func (r *supRig) waitSeen(desc string, pred func(Event) bool) Event {
	r.t.Helper()
	var after int64
	return r.scan(desc, &after, pred)
}

// all returns every event in the log.
func (r *supRig) all() []Event {
	r.t.Helper()
	page, err := r.log.Read(context.Background(), Query{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Events
}

// row returns the folder's sync_folders row.
func (r *supRig) row(folder string) FolderRow {
	r.t.Helper()
	row, ok, err := r.state.Folder(context.Background(), "acme", folder)
	if err != nil || !ok {
		r.t.Fatalf("sync_folders row for %s: ok %v, err %v", folder, ok, err)
	}
	return row
}

// waitIdle blocks until a watch connection on folder enters IDLE. It
// discards IDLE starts on other folders.
func (r *supRig) waitIdle(folder string) {
	r.t.Helper()
	deadline := time.NewTimer(supWait)
	defer deadline.Stop()
	for {
		select {
		case f := <-r.fs.IdleStarted():
			if f == folder {
				return
			}
		case <-deadline.C:
			r.t.Fatalf("no IDLE on %s", folder)
		}
	}
}

// supFake is a fake account with IDLE and CONDSTORE.
func supFake(gmail bool) *fake.Store {
	fs := fake.New("acme", gmail)
	fs.SetIdle(true)
	fs.SetCondStore(true)
	return fs
}

// supAccount is the account acme on preset, resolved as config.Parse would
// with sync enabled and the default poll.
func supAccount(t *testing.T, preset string, watch, syncFolders []string) config.ResolvedAccount {
	t.Helper()
	p, ok := provider.Lookup(preset)
	if !ok {
		t.Fatalf("no preset %q", preset)
	}
	return config.ResolvedAccount{
		Name: "acme", Address: "me@example.com", Provider: p,
		Watch: watch, SyncFolders: syncFolders, SyncPoll: 5 * time.Minute, SyncEnabled: true,
	}
}

func supMsg(folder, subject string) fake.Message {
	return fake.Message{
		Folder: folder, Subject: subject, Date: supStart.Add(-time.Hour), Body: "hello",
		From: []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
		To:   []mailstore.Address{{Email: "me@example.com"}},
	}
}

func supGmailMsg(subject string, labels ...string) fake.Message {
	m := supMsg(supAllMail, subject)
	m.Labels = labels
	return m
}

func supStatus(folder string, state Status) func(Event) bool {
	return func(e Event) bool { return e.Kind == KindStatus && e.Folder == folder && e.State == state }
}

func supNew(folder, subject string) func(Event) bool {
	return func(e Event) bool {
		return e.Kind == KindNew && e.Folder == folder && e.Message != nil && e.Message.Subject == subject
	}
}

func supKind(folder string, k Kind) func(Event) bool {
	return func(e Event) bool { return e.Kind == k && e.Folder == folder }
}

func supCount(events []Event, pred func(Event) bool) int {
	n := 0
	for _, e := range events {
		if pred(e) {
			n++
		}
	}
	return n
}

// supDump is a short listing of events for failure messages.
func supDump(events []Event) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "\n  %d %s %q", e.ID, e.Kind, e.Folder)
		if e.Kind == KindStatus {
			fmt.Fprintf(&b, " state=%s detail=%q", e.State, e.Detail)
		}
		if e.Message != nil {
			fmt.Fprintf(&b, " subject=%q", e.Message.Subject)
		}
	}
	return b.String()
}

// An account with sync disabled starts nothing and touches nothing.
func TestSupervisorSyncDisabledReturnsAtOnce(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	acct := supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Sent"})
	acct.SyncEnabled = false
	r := newSupRig(t, fs, acct)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.sup.Run(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(supWait):
		t.Fatal("Run with sync disabled did not return")
	}
	if n := r.acquired.Load(); n != 0 {
		t.Errorf("Store opened %d times, want 0", n)
	}
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() = %d, want 0", n)
	}
	if events := r.all(); len(events) != 0 {
		t.Errorf("events:%s, want none", supDump(events))
	}
}

// The first run baselines existing mail silently; mail that arrives later
// is a new event, through an IDLE wake on a connection the store does not
// hold.
func TestSupervisorBaselinesThenReportsNewMail(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	for _, subject := range []string{"old one", "old two", "old three"} {
		fs.Add(supMsg("INBOX", subject))
	}
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.start()
	r.waitFor("INBOX baselined", supStatus("INBOX", StateBaselined))
	r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
	r.waitIdle("INBOX")
	if n := supCount(r.all(), supKind("INBOX", KindNew)); n != 0 {
		t.Fatalf("the baseline produced %d new events, want 0", n)
	}
	if row := r.row("INBOX"); row.Mode != ModeIdle || row.State != StateWatching || row.LastError != "" {
		t.Errorf("row = mode %q state %q last_error %q, want idle, watching and no error", row.Mode, row.State, row.LastError)
	}
	if n := r.held.Load(); n != 0 {
		t.Errorf("store held %d times while idle, want 0", n)
	}
	want := fs.Add(supMsg("INBOX", "fresh"))
	e := r.waitFor("new event for fresh", supNew("INBOX", "fresh"))
	if e.Ref != want.String() {
		t.Errorf("ref = %q, want %q", e.Ref, want.String())
	}
	if n := supCount(r.all(), supKind("INBOX", KindNew)); n != 1 {
		t.Errorf("%d new events, want 1:%s", n, supDump(r.all()))
	}
}

// A Gmail account watches its \All folder, found by role, and nothing else.
func TestSupervisorGmailWatchesAllMailOnly(t *testing.T) {
	t.Parallel()
	fs := supFake(true)
	fs.Add(supGmailMsg("old", `\Inbox`))
	r := newSupRig(t, fs, supAccount(t, "gmail", nil, nil))
	r.start()
	r.waitFor("All Mail watching", supStatus(supAllMail, StateWatching))
	r.waitIdle(supAllMail)
	if n := fs.WatchConns(); n != 1 {
		t.Errorf("WatchConns() = %d, want 1", n)
	}
	if n := fs.Calls("ListFolders"); n != 1 {
		t.Errorf("ListFolders ran %d times, want 1", n)
	}
	rows, err := r.state.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Folder != supAllMail || rows[0].Mode != ModeIdle {
		t.Errorf("rows = %+v, want one idle row for %s", rows, supAllMail)
	}
	fs.Add(supGmailMsg("hello", `\Inbox`))
	e := r.waitFor("new event for hello", supNew(supAllMail, "hello"))
	if !slices.Contains(e.Message.Labels, `\Inbox`) {
		t.Errorf("labels = %q, want \\Inbox", e.Message.Labels)
	}
}

// supGoogleCheck is the google_check the Google tests set: a value no
// default or fallback uses, so a test sees the setting reach the watcher.
const supGoogleCheck = 7 * time.Second

// supGoogleRig is a rig for a quiet Google account on preset, whose server
// reports nothing to a waiting IDLE, with google_check at supGoogleCheck.
func supGoogleRig(t *testing.T, fs *fake.Store, preset string) *supRig {
	t.Helper()
	fs.SetQuietIdle(true)
	return newSupRig(t, fs, supAccount(t, preset, nil, nil), func(o *SupervisorOptions) {
		o.Settings.GoogleCheck = supGoogleCheck
	})
}

// Gmail tells an IDLE about new mail late, often 30 seconds after it
// arrives, so a Google account's watcher ends each IDLE after google_check
// and syncs All Mail, on the same connection, whether or not the server
// spoke. Other accounts keep their IDLE until idleRestart.
func TestSupervisorGoogleChecksWhileIdle(t *testing.T) {
	t.Parallel()
	for _, preset := range []string{"gmail", "google-workspace"} {
		t.Run(preset, func(t *testing.T) {
			t.Parallel()
			fs := supFake(true)
			r := supGoogleRig(t, fs, preset)
			r.start()
			r.waitFor("All Mail watching", supStatus(supAllMail, StateWatching))
			r.waitIdle(supAllMail)
			r.clock.expectAfter(t, supGoogleCheck)
			fs.Add(supGmailMsg("quiet", `\Inbox`))
			r.clock.Advance(supGoogleCheck)
			r.waitFor("new event for quiet", supNew(supAllMail, "quiet"))
			r.waitIdle(supAllMail)
			r.clock.expectAfter(t, supGoogleCheck)
			if n := fs.Calls("OpenWatch"); n != 1 {
				t.Errorf("OpenWatch ran %d times, want 1", n)
			}
		})
	}
	t.Run("hostinger", func(t *testing.T) {
		t.Parallel()
		fs := supFake(false)
		fs.SetQuietIdle(true)
		r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}), func(o *SupervisorOptions) {
			o.Settings.GoogleCheck = supGoogleCheck
		})
		r.start()
		r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
		r.waitIdle("INBOX")
		r.clock.expectAfter(t, idleRestart)
		r.clock.mu.Lock()
		n := r.clock.afters[supGoogleCheck]
		r.clock.mu.Unlock()
		if n != 0 {
			t.Errorf("After(%v) ran %d times on a non-Google account, want 0", supGoogleCheck, n)
		}
	})
}

// An expunge Gmail does not report while the short IDLE runs is found by
// the next pass, which still schedules the sweep at the sweep floor, not at
// expunge_sweep.
func TestSupervisorGoogleQuietExpungeSweepsAtTheFloor(t *testing.T) {
	t.Parallel()
	fs := supFake(true)
	old := fs.Add(supGmailMsg("old", `\Inbox`))
	r := supGoogleRig(t, fs, "gmail")
	r.start()
	r.waitFor("All Mail watching", supStatus(supAllMail, StateWatching))
	r.waitIdle(supAllMail)
	r.clock.expectAfter(t, supGoogleCheck)
	fs.ExternalExpunge(supAllMail, old.UID)
	r.clock.Advance(supGoogleCheck)
	// The pass after the check reports the expunge, which schedules the
	// sweep at the floor. Until then the folder idles in short checks.
	r.waitIdle(supAllMail)
	r.clock.expectAfter(t, supGoogleCheck)
	if n := supCount(r.all(), supKind(supAllMail, KindExpunged)); n != 0 {
		t.Fatalf("%d expunged events before the sweep floor, want 0", n)
	}
	r.clock.Advance(ExpungeSweepFloor)
	e := r.waitFor("expunged event for old", supKind(supAllMail, KindExpunged))
	if e.Ref != old.String() {
		t.Errorf("expunged ref = %q, want %q", e.Ref, old.String())
	}
}

// A server without IDLE polls the watched folder on its own connection
// every idle_fallback_poll.
func TestSupervisorFallsBackToPollingWithoutIdle(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	fs.SetIdle(false)
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.start()
	r.waitFor("INBOX polling", supStatus("INBOX", StatePolling))
	r.clock.expectAfter(t, 30*time.Second)
	if row := r.row("INBOX"); row.Mode != ModePoll || row.State != StatePolling {
		t.Errorf("row = mode %q state %q, want poll and polling", row.Mode, row.State)
	}
	if n := fs.WatchConns(); n != 1 {
		t.Errorf("WatchConns() = %d, want 1", n)
	}
	fs.Add(supMsg("INBOX", "late"))
	r.clock.Advance(30 * time.Second)
	r.waitFor("new event for late", supNew("INBOX", "late"))
	r.clock.expectAfter(t, 30*time.Second)
}

// IDLE ends after IdleRestart and the folder syncs again on the same
// connection, without a reconnect: normally because the store's own timer
// for Idle's limit runs out, and for a stuck store because the supervisor
// cancels the IDLE idleGrace later.
func TestSupervisorRestartsIdle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// realStoreTimer leaves the fake on time.After, so only the
		// supervisor's backstop can end the IDLE.
		realStoreTimer bool
		wait           time.Duration
	}{
		{name: "the store ends IDLE at its limit", wait: idleRestart},
		{name: "the supervisor cancels a stuck IDLE", realStoreTimer: true, wait: idleRestart + idleGrace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := supFake(false)
			r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			if tt.realStoreTimer {
				fs.SetAfter(nil)
			}
			r.start()
			r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
			r.waitIdle("INBOX")
			r.clock.expectAfter(t, tt.wait)
			r.clock.Advance(tt.wait)
			r.waitIdle("INBOX")
			if got, want := r.row("INBOX").LastSyncAt, supStart.Add(tt.wait); !got.Equal(want) {
				t.Errorf("last_sync_at = %v, want %v", got, want)
			}
			if n := fs.WatchConns(); n != 1 {
				t.Errorf("WatchConns() = %d, want 1", n)
			}
			if n := fs.Calls("OpenWatch"); n != 1 {
				t.Errorf("OpenWatch ran %d times, want 1", n)
			}
			if n := supCount(r.all(), supStatus("INBOX", StateReconnecting)); n != 0 {
				t.Errorf("%d reconnecting events, want 0", n)
			}
		})
	}
}

// An EXPUNGE during IDLE schedules a sweep no sooner than the sweep floor
// after the last sweep, which here is the first pass: ExpungeSweepFloor by
// default, SupervisorOptions.SweepFloor when set.
func TestSupervisorExpungeSweepFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sweepFloor time.Duration // SupervisorOptions.SweepFloor
		want       time.Duration // the floor in effect
	}{
		{name: "default floor", want: ExpungeSweepFloor},
		{name: "configured floor", sweepFloor: 30 * time.Second, want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := supFake(false)
			gone := fs.Add(supMsg("INBOX", "doomed"))
			fs.Add(supMsg("INBOX", "keeper"))
			r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}), func(o *SupervisorOptions) {
				o.SweepFloor = tt.sweepFloor
			})
			r.start()
			r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
			r.waitIdle("INBOX")
			fs.ExternalExpunge("INBOX", gone.UID)
			// The wake runs a pass without a sweep, then idles until the floor.
			r.waitIdle("INBOX")
			r.clock.expectAfter(t, tt.want)
			if n := supCount(r.all(), supKind("INBOX", KindExpunged)); n != 0 {
				t.Fatalf("%d expunged events before the sweep, want 0", n)
			}
			r.clock.Advance(tt.want - time.Second)
			if n := supCount(r.all(), supKind("INBOX", KindExpunged)); n != 0 {
				t.Fatalf("%d expunged events before the floor, want 0", n)
			}
			r.clock.Advance(time.Second)
			e := r.waitFor("expunged event", supKind("INBOX", KindExpunged))
			if e.Ref != gone.String() {
				t.Errorf("ref = %q, want %q", e.Ref, gone.String())
			}
			if d := e.At.Sub(supStart); d < tt.want {
				t.Errorf("swept %v after the last sweep, want at least %v", d, tt.want)
			}
		})
	}
}

// Cancelling Run closes every watch connection, and the store is never
// held while the watchers wait.
func TestSupervisorShutdownClosesConnections(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	folders := []string{"INBOX", "Archive"}
	r := newSupRig(t, fs, supAccount(t, "hostinger", folders, folders))
	r.start()
	r.waitSeen("INBOX watching", supStatus("INBOX", StateWatching))
	r.waitSeen("Archive watching", supStatus("Archive", StateWatching))
	if n := fs.WatchConns(); n != 2 {
		t.Errorf("WatchConns() = %d, want 2", n)
	}
	if n := r.held.Load(); n != 0 {
		t.Errorf("store held %d times while watching, want 0", n)
	}
	r.stop()
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() after Run = %d, want 0", n)
	}
	if n := r.held.Load(); n != 0 {
		t.Errorf("store held %d times after Run, want 0", n)
	}
}

// A store that cannot watch ends Run instead of retrying forever.
func TestSupervisorStoreWithoutWatcherEndsRun(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Sent"}))
	r.store = struct{ mailstore.Store }{fs} // hides OpenWatch, FolderState and SyncFolder
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.sup.Run(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(supWait):
		t.Fatal("Run did not return for a store that cannot watch")
	}
	if n := r.held.Load(); n != 0 {
		t.Errorf("store held %d times, want 0", n)
	}
}

// A dropped connection reports reconnecting once per outage, backs off 1s
// then 2s while reconnects fail, catches up on mail that arrived during
// the outage, and starts the backoff over after a successful pass.
func TestSupervisorReconnectsAfterDrop(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.start()
	r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
	r.waitIdle("INBOX")

	// FailWatch is one-shot: it fails the first reconnect only.
	fs.FailWatch(fmt.Errorf("%w: test outage", mailstore.ErrUnavailable))
	fs.DropWatches()
	r.waitFor("INBOX reconnecting", supStatus("INBOX", StateReconnecting))
	fs.Add(supMsg("INBOX", "during one"))
	fs.Add(supMsg("INBOX", "during two"))
	r.clock.expectAfter(t, backoff(0, 0.5))
	r.clock.Advance(backoff(0, 0.5)) // this reconnect fails
	r.clock.expectAfter(t, backoff(1, 0.5))
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() during the outage = %d, want 0", n)
	}
	if row := r.row("INBOX"); row.State != StateReconnecting || row.LastError != "SERVER_UNAVAILABLE" {
		t.Errorf("row = state %q last_error %q, want reconnecting and SERVER_UNAVAILABLE", row.State, row.LastError)
	}
	r.clock.Advance(backoff(1, 0.5))
	r.waitFor("new event for during one", supNew("INBOX", "during one"))
	r.waitFor("new event for during two", supNew("INBOX", "during two"))
	r.waitFor("INBOX watching again", supStatus("INBOX", StateWatching))
	if n := supCount(r.all(), supStatus("INBOX", StateReconnecting)); n != 1 {
		t.Errorf("%d reconnecting events for one outage, want 1:%s", n, supDump(r.all()))
	}
	if row := r.row("INBOX"); row.LastError != "" {
		t.Errorf("last_error after recovery = %q, want empty", row.LastError)
	}

	// The pass after the reconnect succeeded, so the next outage waits 1s again.
	r.waitIdle("INBOX")
	fs.DropWatches()
	r.waitFor("INBOX reconnecting again", supStatus("INBOX", StateReconnecting))
	r.clock.expectAfter(t, backoff(0, 0.5))
	r.clock.Advance(backoff(0, 0.5))
	r.waitFor("INBOX watching after the second outage", supStatus("INBOX", StateWatching))
}

// While the auth state says the account needs a human, the watcher opens
// nothing, reports auth_required once with the hint, and checks again
// every AuthRecheck. It resumes once the state clears.
func TestSupervisorParksWhileAuthRequired(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	var required atomic.Bool
	required.Store(true)
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}), func(o *SupervisorOptions) {
		o.AuthRequired = func(account string) (bool, string) {
			if account != "acme" {
				t.Errorf("AuthRequired(%q), want acme", account)
			}
			return required.Load(), supHint
		}
	})
	r.start()
	e := r.waitFor("account auth_required", supStatus("", StateAuthRequired))
	if e.Detail != supHint {
		t.Errorf("detail = %q, want %q", e.Detail, supHint)
	}
	r.clock.expectAfter(t, authRecheck)
	if n := r.acquired.Load(); n != 0 {
		t.Errorf("Store opened %d times while parked, want 0", n)
	}
	if row := r.row("INBOX"); row.State != StateAuthRequired || row.LastError != "AUTH_REQUIRED" {
		t.Errorf("row = state %q last_error %q, want auth_required and AUTH_REQUIRED", row.State, row.LastError)
	}
	r.clock.Advance(authRecheck) // still required
	r.clock.expectAfter(t, authRecheck)
	required.Store(false)
	r.clock.Advance(authRecheck)
	r.waitFor("INBOX watching after auth", supStatus("INBOX", StateWatching))
	if n := supCount(r.all(), supStatus("", StateAuthRequired)); n != 1 {
		t.Errorf("%d auth_required events, want 1:%s", n, supDump(r.all()))
	}
}

// A refusal of the credentials parks the watcher with the error's hint,
// with no reconnect backoff, and a later connection resumes it.
func TestSupervisorParksOnRejectedCredentials(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	// One-shot: the first OpenWatch is refused, the next one works.
	fs.FailWatch(&auth.RejectedError{Hint: supHint, Err: errors.New("LOGIN refused")})
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.start()
	e := r.waitFor("account auth_required", supStatus("", StateAuthRequired))
	if e.Detail != supHint {
		t.Errorf("detail = %q, want %q", e.Detail, supHint)
	}
	r.clock.expectAfter(t, authRecheck)
	r.clock.Advance(authRecheck)
	r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
	if n := supCount(r.all(), supStatus("INBOX", StateReconnecting)); n != 0 {
		t.Errorf("%d reconnecting events for an auth refusal, want 0", n)
	}
}

// A watched folder that does not exist reports FOLDER_NOT_FOUND and retries
// every sync.poll, while the account's other folder keeps running.
func TestSupervisorFolderNotFoundIsolated(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	acct := supAccount(t, "hostinger", []string{"INBOX", "Projects"}, []string{"INBOX", "Projects"})
	r := newSupRig(t, fs, acct)
	r.start()
	e := r.waitSeen("Projects error", supStatus("Projects", StateError))
	if e.Detail != reasonFolderNotFound {
		t.Errorf("detail = %q, want %q", e.Detail, reasonFolderNotFound)
	}
	r.waitSeen("INBOX watching", supStatus("INBOX", StateWatching))
	r.waitIdle("INBOX")
	fs.Add(supMsg("INBOX", "still flowing"))
	r.waitSeen("new event for still flowing", supNew("INBOX", "still flowing"))
	r.clock.expectAfter(t, acct.SyncPoll)
	if row := r.row("Projects"); row.State != StateError || row.LastError != reasonFolderNotFound {
		t.Errorf("Projects row = state %q last_error %q, want error and %s", row.State, row.LastError, reasonFolderNotFound)
	}
	fs.AddFolder("Projects", mailstore.RoleNone)
	r.clock.Advance(acct.SyncPoll)
	r.waitSeen("Projects watching", supStatus("Projects", StateWatching))
	if row := r.row("Projects"); row.LastError != "" {
		t.Errorf("Projects last_error = %q, want empty", row.LastError)
	}
	for _, folder := range []string{"INBOX", "Projects"} {
		if n := supCount(r.all(), supStatus(folder, StateReconnecting)); n != 0 {
			t.Errorf("%d reconnecting events for %s, want 0", n, folder)
		}
	}
}

// After FolderNotFound a Gmail watcher resolves its \All folder again from
// ListFolders before the next connection.
func TestSupervisorGmailReresolvesAllMail(t *testing.T) {
	t.Parallel()
	fs := supFake(true)
	acct := supAccount(t, "gmail", nil, nil)
	r := newSupRig(t, fs, acct)
	r.start()
	r.waitFor("All Mail watching", supStatus(supAllMail, StateWatching))
	r.waitIdle(supAllMail)
	// One-shot: the first reconnect finds no folder; the next one resolves again.
	fs.FailWatch(&mailstore.FolderNotFoundError{Name: supAllMail, Requested: supAllMail})
	fs.DropWatches()
	r.waitFor("All Mail reconnecting", supStatus(supAllMail, StateReconnecting))
	r.clock.expectAfter(t, backoff(0, 0.5))
	r.clock.Advance(backoff(0, 0.5))
	e := r.waitFor("All Mail error", supStatus(supAllMail, StateError))
	if e.Detail != reasonFolderNotFound {
		t.Errorf("detail = %q, want %q", e.Detail, reasonFolderNotFound)
	}
	r.clock.expectAfter(t, acct.SyncPoll)
	lists := fs.Calls("ListFolders")
	r.clock.Advance(acct.SyncPoll)
	r.waitFor("All Mail watching again", supStatus(supAllMail, StateWatching))
	if got := fs.Calls("ListFolders"); got != lists+1 {
		t.Errorf("ListFolders ran %d times after the error, want 1", got-lists)
	}
}

// A synced folder that is not watched is checked with STATUS on a pooled
// connection every sync.poll. New mail shows on the next tick. With
// CONDSTORE an unchanged folder gets no pass; without, every tick passes,
// since STATUS cannot show flag changes.
func TestSupervisorPollsUnwatchedFolders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		condStore bool
		idlePass  int // SyncFolder calls on a tick with no change
	}{
		{name: "condstore skips an unchanged folder", condStore: true, idlePass: 0},
		{name: "without condstore every tick passes", condStore: false, idlePass: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := supFake(false)
			fs.SetCondStore(tt.condStore)
			acct := supAccount(t, "hostinger", nil, []string{"Sent"})
			r := newSupRig(t, fs, acct)
			r.start()
			r.waitFor("Sent polling", supStatus("Sent", StatePolling))
			r.clock.expectAfter(t, acct.SyncPoll)
			if n := r.held.Load(); n != 0 {
				t.Errorf("store held %d times between ticks, want 0", n)
			}
			if n := fs.WatchConns(); n != 0 {
				t.Errorf("WatchConns() = %d, want 0: polled folders use the pool", n)
			}
			if row := r.row("Sent"); row.Mode != ModePoll {
				t.Errorf("Sent mode = %q, want poll", row.Mode)
			}
			fs.Add(supMsg("Sent", "report"))
			r.clock.Advance(acct.SyncPoll)
			r.waitFor("new event for report", supNew("Sent", "report"))
			r.clock.expectAfter(t, acct.SyncPoll)
			states, syncs := fs.Calls("FolderState"), fs.Calls("SyncFolder")
			r.clock.Advance(acct.SyncPoll)
			r.clock.expectAfter(t, acct.SyncPoll)
			if got := fs.Calls("FolderState") - states; got != 1 {
				t.Errorf("FolderState ran %d times on a tick, want 1", got)
			}
			if got := fs.Calls("SyncFolder") - syncs; got != tt.idlePass {
				t.Errorf("SyncFolder ran %d times on an unchanged tick, want %d", got, tt.idlePass)
			}
		})
	}
}

// A CONDSTORE poll that STATUS shows unchanged runs no pass, but it still
// confirms the folder is current, so it stamps last_sync_at. The cache
// counts a polled folder fresh only while last_sync_at is within twice
// sync.poll, so a quiet folder must not age out between changes.
func TestSupervisorUnchangedPollStampsLastSync(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	fs.SetCondStore(true)
	acct := supAccount(t, "hostinger", nil, []string{"Sent"})
	r := newSupRig(t, fs, acct)
	r.start()
	r.waitFor("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
	first := r.row("Sent")
	syncs := fs.Calls("SyncFolder")
	r.clock.Advance(acct.SyncPoll)
	r.clock.expectAfter(t, acct.SyncPoll)
	if got := fs.Calls("SyncFolder") - syncs; got != 0 {
		t.Fatalf("SyncFolder ran %d times on an unchanged tick, want 0", got)
	}
	row := r.row("Sent")
	if want := first.LastSyncAt.Add(acct.SyncPoll); !row.LastSyncAt.Equal(want) {
		t.Errorf("last_sync_at after an unchanged tick = %v, want %v", row.LastSyncAt, want)
	}
	row.LastSyncAt = first.LastSyncAt
	if row != first {
		t.Errorf("row after an unchanged tick = %+v, want only last_sync_at moved from %+v", row, first)
	}
}

// A polled folder that does not exist reports FOLDER_NOT_FOUND once, the
// other polled folder keeps working, and the folder recovers on the tick
// after it appears.
func TestSupervisorPolledFolderNotFound(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	acct := supAccount(t, "hostinger", nil, []string{"Reports", "Sent"})
	r := newSupRig(t, fs, acct)
	r.start()
	e := r.waitSeen("Reports error", supStatus("Reports", StateError))
	if e.Detail != reasonFolderNotFound {
		t.Errorf("detail = %q, want %q", e.Detail, reasonFolderNotFound)
	}
	r.waitSeen("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
	fs.Add(supMsg("Sent", "report"))
	r.clock.Advance(acct.SyncPoll)
	r.waitSeen("new event for report", supNew("Sent", "report"))
	r.clock.expectAfter(t, acct.SyncPoll)
	if n := supCount(r.all(), supStatus("Reports", StateError)); n != 1 {
		t.Errorf("%d error events for Reports over two ticks, want 1", n)
	}
	fs.AddFolder("Reports", mailstore.RoleNone)
	r.clock.Advance(acct.SyncPoll)
	r.waitSeen("Reports polling", supStatus("Reports", StatePolling))
	if row := r.row("Reports"); row.LastError != "" {
		t.Errorf("Reports last_error = %q, want empty", row.LastError)
	}
}

// The poller parks every polled folder while the account needs a human,
// without opening the store, and resumes when the state clears.
func TestSupervisorPollerParksWhileAuthRequired(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	var required atomic.Bool
	required.Store(true)
	acct := supAccount(t, "hostinger", nil, []string{"Sent"})
	r := newSupRig(t, fs, acct, func(o *SupervisorOptions) {
		o.AuthRequired = func(string) (bool, string) { return required.Load(), supHint }
	})
	r.start()
	e := r.waitFor("account auth_required", supStatus("", StateAuthRequired))
	if e.Detail != supHint {
		t.Errorf("detail = %q, want %q", e.Detail, supHint)
	}
	r.clock.expectAfter(t, authRecheck)
	if n := r.acquired.Load(); n != 0 {
		t.Errorf("Store opened %d times while parked, want 0", n)
	}
	if row := r.row("Sent"); row.State != StateAuthRequired || row.LastError != "AUTH_REQUIRED" || row.Mode != ModePoll {
		t.Errorf("Sent row = mode %q state %q last_error %q, want poll, auth_required and AUTH_REQUIRED", row.Mode, row.State, row.LastError)
	}
	required.Store(false)
	r.clock.Advance(authRecheck)
	r.waitFor("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
}

// supAfter returns a scan position just past e, for events that follow it
// in no fixed order.
func supAfter(e Event) *int64 {
	pos := e.ID
	return &pos
}

// When the auth state turns required while the account's watchers and
// poller run, every loop stops and closes its connection before the
// account parks: one auth_required event with the hint, and an
// auth_required row for every folder. All of them resume once it clears.
func TestSupervisorParksEveryLoopMidSession(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	var required atomic.Bool
	watched := []string{"INBOX", "Archive"}
	acct := supAccount(t, "hostinger", watched, []string{"INBOX", "Archive", "Sent"})
	r := newSupRig(t, fs, acct, func(o *SupervisorOptions) {
		o.AuthRequired = func(string) (bool, string) { return required.Load(), supHint }
	})
	r.start()
	r.waitSeen("INBOX watching", supStatus("INBOX", StateWatching))
	r.waitSeen("Archive watching", supStatus("Archive", StateWatching))
	r.waitSeen("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, authRecheck)   // the running account's auth check
	r.clock.expectAfter(t, acct.SyncPoll) // the poller waits for its next tick

	required.Store(true)
	r.clock.Advance(authRecheck)
	e := r.waitFor("account auth_required", supStatus("", StateAuthRequired))
	if e.Detail != supHint {
		t.Errorf("detail = %q, want %q", e.Detail, supHint)
	}
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() while parked = %d, want 0", n)
	}
	if n := r.held.Load(); n != 0 {
		t.Errorf("store held %d times while parked, want 0", n)
	}
	for _, folder := range []string{"INBOX", "Archive", "Sent"} {
		if row := r.row(folder); row.State != StateAuthRequired || row.LastError != "AUTH_REQUIRED" {
			t.Errorf("%s row = state %q last_error %q, want auth_required and AUTH_REQUIRED", folder, row.State, row.LastError)
		}
	}
	opens, states := fs.Calls("OpenWatch"), fs.Calls("FolderState")
	r.clock.expectAfter(t, authRecheck)
	r.clock.Advance(authRecheck) // still required
	r.clock.expectAfter(t, authRecheck)
	if got := fs.Calls("OpenWatch"); got != opens {
		t.Errorf("OpenWatch ran %d times while parked, want 0", got-opens)
	}
	if got := fs.Calls("FolderState"); got != states {
		t.Errorf("FolderState ran %d times while parked, want 0", got-states)
	}
	if n := supCount(r.all(), supKind("", KindStatus)); n != 1 {
		t.Errorf("%d account status events while parked, want 1:%s", n, supDump(r.all()))
	}
	for _, folder := range []string{"INBOX", "Archive", "Sent"} {
		if n := supCount(r.all(), supStatus(folder, StateAuthRequired)); n != 0 {
			t.Errorf("%d auth_required events for %s, want 0: the account event covers it", n, folder)
		}
	}

	required.Store(false)
	r.clock.Advance(authRecheck)
	r.scan("INBOX watching after auth", supAfter(e), supStatus("INBOX", StateWatching))
	r.scan("Archive watching after auth", supAfter(e), supStatus("Archive", StateWatching))
	r.scan("Sent polling after auth", supAfter(e), supStatus("Sent", StatePolling))
	if n := fs.WatchConns(); n != 2 {
		t.Errorf("WatchConns() after auth = %d, want 2", n)
	}
	for _, folder := range []string{"INBOX", "Archive", "Sent"} {
		if row := r.row(folder); row.LastError != "" {
			t.Errorf("%s last_error after auth = %q, want empty", folder, row.LastError)
		}
	}
}

// A polled folder whose STATUS or pass fails reports reconnecting once,
// retries with backoff capped at sync.poll, reports polling again after the
// first success, and starts the backoff over for the next outage.
func TestSupervisorPollerBacksOffOutages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string // the fake method that fails
	}{
		{name: "STATUS fails", method: "FolderState"},
		{name: "the pass fails", method: "SyncFolder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outage := fmt.Errorf("%w: test outage", mailstore.ErrUnavailable)
			fs := supFake(false)
			acct := supAccount(t, "hostinger", nil, []string{"Sent"})
			// Short enough that the backoff reaches it: Backoff(2, 0.5) is 4s.
			acct.SyncPoll = 3 * time.Second
			r := newSupRig(t, fs, acct)
			r.start()
			r.waitFor("Sent polling", supStatus("Sent", StatePolling))
			r.clock.expectAfter(t, acct.SyncPoll)

			fs.Fail(tt.method, outage)
			fs.Add(supMsg("Sent", "during")) // STATUS shows a change, so the pass runs
			r.clock.Advance(acct.SyncPoll)
			r.waitFor("Sent reconnecting", supStatus("Sent", StateReconnecting))
			r.clock.expectAfter(t, backoff(0, 0.5))
			if row := r.row("Sent"); row.State != StateReconnecting || row.LastError != "SERVER_UNAVAILABLE" {
				t.Errorf("row = state %q last_error %q, want reconnecting and SERVER_UNAVAILABLE", row.State, row.LastError)
			}
			r.clock.Advance(backoff(0, 0.5))
			r.clock.expectAfter(t, backoff(1, 0.5))
			r.clock.Advance(backoff(1, 0.5))
			r.clock.expectAfter(t, acct.SyncPoll) // Backoff(2, 0.5) is 4s, capped at the 3s poll

			fs.Fail(tt.method, nil)
			r.clock.Advance(acct.SyncPoll)
			r.waitFor("new event for during", supNew("Sent", "during"))
			r.waitFor("Sent polling again", supStatus("Sent", StatePolling))
			r.clock.expectAfter(t, acct.SyncPoll)
			if n := supCount(r.all(), supStatus("Sent", StateReconnecting)); n != 1 {
				t.Errorf("%d reconnecting events for one outage, want 1:%s", n, supDump(r.all()))
			}
			if row := r.row("Sent"); row.State != StatePolling || row.LastError != "" {
				t.Errorf("row after recovery = state %q last_error %q, want polling and no error", row.State, row.LastError)
			}

			// The success reset the backoff, so the next outage retries after 1s again.
			fs.Fail(tt.method, outage)
			fs.Add(supMsg("Sent", "later"))
			r.clock.Advance(acct.SyncPoll)
			r.waitFor("Sent reconnecting again", supStatus("Sent", StateReconnecting))
			r.clock.expectAfter(t, backoff(0, 0.5))
		})
	}
}

// A status write that fails is not published and not remembered: the
// watcher backs off as for a database error, and the retry records the
// status exactly once.
func TestSupervisorRetriesFailedStatusWrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		failSetStatus bool // else Publish fails
	}{
		{name: "SetStatus fails", failSetStatus: true},
		{name: "Publish fails"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := supFake(false)
			r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			errDisk := errors.New("disk full")
			var failed atomic.Bool
			if tt.failSetStatus {
				next := r.sup.writeStatus
				r.sup.writeStatus = func(ctx context.Context, account, folder string, mode Mode, state Status, lastError string) error {
					if state == StateWatching && failed.CompareAndSwap(false, true) {
						return errDisk
					}
					return next(ctx, account, folder, mode, state, lastError)
				}
			} else {
				next := r.sup.publishEvents
				r.sup.publishEvents = func(ctx context.Context, events ...Event) error {
					if len(events) == 1 && events[0].State == StateWatching && failed.CompareAndSwap(false, true) {
						return errDisk
					}
					return next(ctx, events...)
				}
			}
			r.start()
			r.waitFor("INBOX reconnecting", supStatus("INBOX", StateReconnecting))
			if !failed.Load() {
				t.Fatal("the status write never failed")
			}
			if n := supCount(r.all(), supStatus("INBOX", StateWatching)); n != 0 {
				t.Fatalf("%d watching events after the failed write, want 0:%s", n, supDump(r.all()))
			}
			r.clock.expectAfter(t, backoff(0, 0.5))
			r.clock.Advance(backoff(0, 0.5))
			r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
			r.waitIdle("INBOX")
			if n := supCount(r.all(), supStatus("INBOX", StateWatching)); n != 1 {
				t.Errorf("%d watching events, want 1:%s", n, supDump(r.all()))
			}
			if row := r.row("INBOX"); row.State != StateWatching || row.LastError != "" {
				t.Errorf("row = state %q last_error %q, want watching and no error", row.State, row.LastError)
			}
		})
	}
}

// supStatuses lists folder's status events as "state" strings, in order.
func supStatuses(events []Event, folder string) []Status {
	var out []Status
	for _, e := range events {
		if e.Kind == KindStatus && e.Folder == folder {
			out = append(out, e.State)
		}
	}
	return out
}

// An outage whose reconnecting row lands but whose event does not leaves no
// stale row: the next transition completes or undoes it, so after recovery
// the row says watching with no error, and the log holds exactly the events
// that were published.
func TestSupervisorRepairsPartialStatusTransition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// failReconnect makes the first reconnect fail too, so the outage
		// asks for reconnecting a second time.
		failReconnect bool
		want          []Status
	}{
		{name: "the next connection works", want: []Status{StateBaselined, StateWatching}},
		{
			name: "the next connection fails too", failReconnect: true,
			want: []Status{StateBaselined, StateWatching, StateReconnecting, StateWatching},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := supFake(false)
			r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			var failed atomic.Bool
			next := r.sup.publishEvents
			r.sup.publishEvents = func(ctx context.Context, events ...Event) error {
				if len(events) == 1 && events[0].State == StateReconnecting && failed.CompareAndSwap(false, true) {
					return errors.New("disk full")
				}
				return next(ctx, events...)
			}
			r.start()
			r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
			r.waitIdle("INBOX")

			if tt.failReconnect {
				fs.FailWatch(fmt.Errorf("%w: test outage", mailstore.ErrUnavailable)) // one-shot: the first reconnect
			}
			fs.DropWatches()
			r.clock.expectAfter(t, backoff(0, 0.5))
			if !failed.Load() {
				t.Fatal("publishing reconnecting never failed")
			}
			if row := r.row("INBOX"); row.State != StateReconnecting || row.LastError != "SERVER_UNAVAILABLE" {
				t.Fatalf("row after the half-written transition = state %q last_error %q, want reconnecting and SERVER_UNAVAILABLE", row.State, row.LastError)
			}
			r.clock.Advance(backoff(0, 0.5))
			if tt.failReconnect {
				r.waitFor("INBOX reconnecting, published on the next attempt", supStatus("INBOX", StateReconnecting))
				r.clock.expectAfter(t, backoff(1, 0.5))
				r.clock.Advance(backoff(1, 0.5))
			}
			r.waitIdle("INBOX") // the new connection has passed and settled
			if row := r.row("INBOX"); row.State != StateWatching || row.LastError != "" {
				t.Errorf("row after recovery = state %q last_error %q, want watching and no error", row.State, row.LastError)
			}
			if got := supStatuses(r.all(), "INBOX"); !slices.Equal(got, tt.want) {
				t.Errorf("INBOX status events = %q, want %q", got, tt.want)
			}
		})
	}
}

// supSlowStatus is a fake store whose FolderState takes d on the test clock
// while slow is set, as a STATUS that times out would.
type supSlowStatus struct {
	*fake.Store
	clock *supClock
	d     time.Duration
	slow  atomic.Bool
}

func (s *supSlowStatus) FolderState(ctx context.Context, folder string) (mailstore.FolderState, error) {
	if s.slow.Load() {
		s.clock.Advance(s.d)
	}
	return s.Store.FolderState(ctx, folder)
}

// A poll that takes time to fail waits its whole backoff after the failure,
// not from when the tick began.
func TestSupervisorPollerBackoffStartsAfterTheFailure(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	acct := supAccount(t, "hostinger", nil, []string{"Sent"})
	r := newSupRig(t, fs, acct)
	slow := &supSlowStatus{Store: fs, clock: r.clock, d: 5 * time.Second}
	r.store = slow
	r.start()
	r.waitFor("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)

	slow.slow.Store(true)
	fs.Fail("FolderState", fmt.Errorf("%w: STATUS timed out", mailstore.ErrUnavailable))
	r.clock.Advance(acct.SyncPoll) // the STATUS takes 5s, then fails
	r.waitFor("Sent reconnecting", supStatus("Sent", StateReconnecting))
	r.clock.expectAfter(t, backoff(0, 0.5))
	slow.slow.Store(false)
	fs.Fail("FolderState", nil)
	calls := fs.Calls("FolderState")
	r.clock.Advance(backoff(0, 0.5) - time.Millisecond)
	if got := fs.Calls("FolderState"); got != calls {
		t.Fatalf("FolderState ran %d times before the backoff ran out, want 0", got-calls)
	}
	r.clock.Advance(time.Millisecond)
	r.waitFor("Sent polling again", supStatus("Sent", StatePolling))
	if got := fs.Calls("FolderState"); got != calls+1 {
		t.Errorf("FolderState ran %d times after the backoff, want 1", got-calls)
	}
}

// supStuckStore is a fake store whose watch connections' Idle ignores
// cancellation and returns only once Close is called, as a real IDLE whose
// DONE the server never answers waits out its command timeout.
type supStuckStore struct {
	*fake.Store
	idling chan struct{} // gets a token each time a stuck Idle starts
}

func (s *supStuckStore) OpenWatch(ctx context.Context, folder string) (mailstore.WatchConn, error) {
	c, err := s.Store.OpenWatch(ctx, folder)
	if err != nil {
		return nil, err
	}
	return &supStuckConn{WatchConn: c, idling: s.idling, closed: make(chan struct{})}, nil
}

type supStuckConn struct {
	mailstore.WatchConn
	idling chan struct{}
	once   sync.Once
	closed chan struct{}
}

func (c *supStuckConn) Idle(context.Context, time.Duration) (mailstore.Wake, error) {
	select {
	case c.idling <- struct{}{}:
	default:
	}
	<-c.closed
	return mailstore.Wake{}, fmt.Errorf("%w: closed during IDLE", mailstore.ErrUnavailable)
}

func (c *supStuckConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.WatchConn.Close()
}

// Cancelling Run while an Idle ignores the cancellation closes the
// connection under it after ShutdownLogout on the supervisor's clock, so Run
// returns within the shutdown bound instead of waiting out the IDLE.
func TestSupervisorShutdownClosesAStuckIdle(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	stuck := &supStuckStore{Store: fs, idling: make(chan struct{}, 1)}
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.store = stuck
	r.start()
	r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
	select {
	case <-stuck.idling:
	case <-time.After(supWait):
		t.Fatal("the watcher never started IDLE")
	}
	r.cancel()
	r.clock.expectAfter(t, ShutdownLogout)
	select {
	case <-r.done:
		t.Fatal("Run returned before ShutdownLogout passed, with Idle still running")
	default:
	}
	r.clock.Advance(ShutdownLogout)
	select {
	case <-r.done:
	case <-time.After(supWait):
		t.Fatal("Run did not return once ShutdownLogout passed")
	}
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() after Run = %d, want 0", n)
	}
}

// supStallStore is a fake store whose watch connections end a cancelled
// Idle only when the test lets DONE complete, and whose Close stalls, as an
// unanswered LOGOUT does, until Abort.
type supStallStore struct {
	*fake.Store
	idling  chan struct{} // gets a token when an Idle starts
	doneOK  chan struct{} // closed by the test: DONE completes
	closing chan struct{} // gets a token when Close starts
	conn    atomic.Pointer[supStallConn]
}

func (s *supStallStore) OpenWatch(ctx context.Context, folder string) (mailstore.WatchConn, error) {
	c, err := s.Store.OpenWatch(ctx, folder)
	if err != nil {
		return nil, err
	}
	sc := &supStallConn{WatchConn: c, st: s, aborted: make(chan struct{})}
	s.conn.Store(sc)
	return sc, nil
}

type supStallConn struct {
	mailstore.WatchConn
	st      *supStallStore
	once    sync.Once
	aborted chan struct{}
}

func (c *supStallConn) Idle(ctx context.Context, _ time.Duration) (mailstore.Wake, error) {
	select {
	case c.st.idling <- struct{}{}:
	default:
	}
	<-ctx.Done()
	select {
	case <-c.st.doneOK:
		return mailstore.Wake{}, ctx.Err()
	case <-c.aborted:
		return mailstore.Wake{}, fmt.Errorf("%w: aborted during DONE", mailstore.ErrUnavailable)
	}
}

func (c *supStallConn) Close() error {
	select {
	case c.st.closing <- struct{}{}:
	default:
	}
	<-c.aborted
	_ = c.WatchConn.Close()
	return fmt.Errorf("%w: LOGOUT unanswered", mailstore.ErrUnavailable)
}

func (c *supStallConn) Abort() { c.once.Do(func() { close(c.aborted) }) }

// abortedNow reports whether Abort has run.
func (c *supStallConn) abortedNow() bool {
	select {
	case <-c.aborted:
		return true
	default:
		return false
	}
}

// One ShutdownLogout deadline, from the moment ctx ends, covers both DONE
// and LOGOUT: DONE that completes 4s in leaves LOGOUT 1s, and when LOGOUT
// stalls the connection is aborted at the deadline, not 5s after DONE.
func TestSupervisorShutdownIsOneDeadline(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	stall := &supStallStore{Store: fs, idling: make(chan struct{}, 1), doneOK: make(chan struct{}), closing: make(chan struct{}, 1)}
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.store = stall
	r.start()
	t.Cleanup(func() { // unblocks a stalled connection if the test fails first
		if c := stall.conn.Load(); c != nil {
			c.Abort()
		}
	})
	r.waitFor("INBOX watching", supStatus("INBOX", StateWatching))
	select {
	case <-stall.idling:
	case <-time.After(supWait):
		t.Fatal("the watcher never started IDLE")
	}
	r.cancel()
	r.clock.expectAfter(t, ShutdownLogout)
	r.clock.Advance(4 * time.Second)
	close(stall.doneOK) // DONE completes 4s after the cancel
	select {
	case <-stall.closing:
	case <-time.After(supWait):
		t.Fatal("the watcher never closed its connection")
	}
	r.clock.expectAfter(t, time.Second) // what is left of the one deadline
	if stall.conn.Load().abortedNow() {
		t.Fatal("aborted before the deadline")
	}
	select {
	case <-r.done:
		t.Fatal("Run returned with LOGOUT still stalled")
	default:
	}
	r.clock.Advance(time.Second)
	select {
	case <-r.done:
	case <-time.After(supWait):
		t.Fatal("Run did not return at the shutdown deadline")
	}
	if !stall.conn.Load().abortedNow() {
		t.Error("the stalled connection was not aborted at the deadline")
	}
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() after Run = %d, want 0", n)
	}
}

// supBudgetStore is a fake store that reports a connection budget, as
// imapstore.Store does. Its OpenWatch shrinks the budget before it fails,
// as imapstore makes room in the pool for a refused watcher.
type supBudgetStore struct {
	*fake.Store
	pool, open atomic.Int64
}

func (s *supBudgetStore) PoolSize() int  { return int(s.pool.Load()) }
func (s *supBudgetStore) OpenConns() int { return int(s.open.Load()) }

func (s *supBudgetStore) OpenWatch(ctx context.Context, folder string) (mailstore.WatchConn, error) {
	s.pool.Store(1)
	s.open.Store(1)
	return s.Store.OpenWatch(ctx, folder)
}

// A watch connection the server refuses for its connection limit logs the
// account's budget as the dial began, before the store made room, in
// fixed fields and without the
// server's text, then backs off and reconnects like any outage.
func TestSupervisorLogsTheBudgetOnAConnLimit(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	fs.FailWatch(fmt.Errorf("%w: %w: acme refused another connection: NO [LIMIT] server says hello", mailstore.ErrUnavailable, mailstore.ErrConnLimit))
	var logs syncBuffer
	r := newSupRig(t, fs, supAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}), func(o *SupervisorOptions) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	budget := &supBudgetStore{Store: fs}
	budget.pool.Store(2)
	budget.open.Store(3)
	r.store = budget
	r.start()
	r.waitFor("INBOX reconnecting", supStatus("INBOX", StateReconnecting))
	got := logs.String()
	want := `msg="server refused a watch connection for its connection limit" account=acme folder=INBOX pool=2 open=3 watchers=1`
	if !strings.Contains(got, want) {
		t.Errorf("logs =\n%s\nwant a line with\n%s", got, want)
	}
	if strings.Contains(got, "hello") {
		t.Errorf("logs carry the server's text:\n%s", got)
	}
	r.clock.expectAfter(t, backoff(0, 0.5))
	r.clock.Advance(backoff(0, 0.5))
	r.waitFor("INBOX watching after the refusal", supStatus("INBOX", StateWatching))
}
