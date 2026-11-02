package watch

import (
	"context"
	"errors"
	"fmt"
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
	r.log = NewLog(conn, r.clock.Now)
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

func supStatus(folder, state string) func(Event) bool {
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
	if row := r.row("INBOX"); row.Mode != passModeIdle || row.State != StateWatching || row.LastError != "" {
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
	if len(rows) != 1 || rows[0].Folder != supAllMail || rows[0].Mode != passModeIdle {
		t.Errorf("rows = %+v, want one idle row for %s", rows, supAllMail)
	}
	fs.Add(supGmailMsg("hello", `\Inbox`))
	e := r.waitFor("new event for hello", supNew(supAllMail, "hello"))
	if !slices.Contains(e.Message.Labels, `\Inbox`) {
		t.Errorf("labels = %q, want \\Inbox", e.Message.Labels)
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
	if row := r.row("INBOX"); row.Mode != passModePoll || row.State != StatePolling {
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
		{name: "the store ends IDLE at its limit", wait: IdleRestart},
		{name: "the supervisor cancels a stuck IDLE", realStoreTimer: true, wait: IdleRestart + idleGrace},
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
	r.clock.expectAfter(t, Backoff(0, 0.5))
	r.clock.Advance(Backoff(0, 0.5)) // this reconnect fails
	r.clock.expectAfter(t, Backoff(1, 0.5))
	if n := fs.WatchConns(); n != 0 {
		t.Errorf("WatchConns() during the outage = %d, want 0", n)
	}
	if row := r.row("INBOX"); row.State != StateReconnecting || row.LastError != "SERVER_UNAVAILABLE" {
		t.Errorf("row = state %q last_error %q, want reconnecting and SERVER_UNAVAILABLE", row.State, row.LastError)
	}
	r.clock.Advance(Backoff(1, 0.5))
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
	r.clock.expectAfter(t, Backoff(0, 0.5))
	r.clock.Advance(Backoff(0, 0.5))
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
	e := r.waitFor("INBOX auth_required", supStatus("INBOX", StateAuthRequired))
	if e.Detail != supHint {
		t.Errorf("detail = %q, want %q", e.Detail, supHint)
	}
	r.clock.expectAfter(t, AuthRecheck)
	if n := r.acquired.Load(); n != 0 {
		t.Errorf("Store opened %d times while parked, want 0", n)
	}
	if row := r.row("INBOX"); row.State != StateAuthRequired || row.LastError != "AUTH_REQUIRED" {
		t.Errorf("row = state %q last_error %q, want auth_required and AUTH_REQUIRED", row.State, row.LastError)
	}
	r.clock.Advance(AuthRecheck) // still required
	r.clock.expectAfter(t, AuthRecheck)
	required.Store(false)
	r.clock.Advance(AuthRecheck)
	r.waitFor("INBOX watching after auth", supStatus("INBOX", StateWatching))
	if n := supCount(r.all(), supStatus("INBOX", StateAuthRequired)); n != 1 {
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
	e := r.waitFor("INBOX auth_required", supStatus("INBOX", StateAuthRequired))
	if e.Detail != supHint {
		t.Errorf("detail = %q, want %q", e.Detail, supHint)
	}
	r.clock.expectAfter(t, AuthRecheck)
	r.clock.Advance(AuthRecheck)
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
	if e.Detail != ReasonFolderNotFound {
		t.Errorf("detail = %q, want %q", e.Detail, ReasonFolderNotFound)
	}
	r.waitSeen("INBOX watching", supStatus("INBOX", StateWatching))
	r.waitIdle("INBOX")
	fs.Add(supMsg("INBOX", "still flowing"))
	r.waitSeen("new event for still flowing", supNew("INBOX", "still flowing"))
	r.clock.expectAfter(t, acct.SyncPoll)
	if row := r.row("Projects"); row.State != StateError || row.LastError != ReasonFolderNotFound {
		t.Errorf("Projects row = state %q last_error %q, want error and %s", row.State, row.LastError, ReasonFolderNotFound)
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
	r.clock.expectAfter(t, Backoff(0, 0.5))
	r.clock.Advance(Backoff(0, 0.5))
	e := r.waitFor("All Mail error", supStatus(supAllMail, StateError))
	if e.Detail != ReasonFolderNotFound {
		t.Errorf("detail = %q, want %q", e.Detail, ReasonFolderNotFound)
	}
	r.clock.expectAfter(t, acct.SyncPoll)
	lists := fs.Calls("ListFolders")
	r.clock.Advance(acct.SyncPoll)
	r.waitFor("All Mail watching again", supStatus(supAllMail, StateWatching))
	if got := fs.Calls("ListFolders"); got != lists+1 {
		t.Errorf("ListFolders ran %d times after the error, want 1", got-lists)
	}
}
