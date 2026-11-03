package cache

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // failInserts opens a second connection to cache.db

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// acctWait bounds every wait in the Account tests. It is a deadline for a
// test that hangs, never a sleep: each wait returns once its condition holds.
const acctWait = 5 * time.Second

// acctStart is where every test clock starts: 09:00 UTC, fifteen hours
// before the budget day ends.
var acctStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// acctClock is a watch.Clock whose timers fire only when the test advances
// it. It counts After calls by duration, so a test can wait until a worker
// waits on a timer before it advances past it.
type acctClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []acctTimer
	afters  map[time.Duration]int // After calls, by duration
	taken   map[time.Duration]int // After calls expectAfter has accounted for
	changed chan struct{}         // closed and replaced on every After call
}

type acctTimer struct {
	at time.Time
	ch chan time.Time
}

func newAcctClock() *acctClock {
	return &acctClock{
		now: acctStart, afters: map[time.Duration]int{}, taken: map[time.Duration]int{},
		changed: make(chan struct{}),
	}
}

func (c *acctClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *acctClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.afters[d]++
	if d <= 0 {
		ch <- c.now
	} else {
		c.timers = append(c.timers, acctTimer{at: c.now.Add(d), ch: ch})
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return ch
}

// Advance moves the clock forward by d and fires every timer now due.
func (c *acctClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var kept []acctTimer
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
func (c *acctClock) expectAfter(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.NewTimer(acctWait)
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

// acctSource syncs one fake folder through the fake's pooled SyncFolder,
// as the supervisor's poller does.
type acctSource struct {
	fs     *fake.Store
	folder string
}

func (s acctSource) Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	return s.fs.SyncFolder(ctx, s.folder, req)
}

// acctBusyMail is a MailStore whose background header fetches fail with
// mailstore.ErrBusy while busy is set, as a full pool makes them. It counts
// the fetches it refused and any made on a foreground context.
type acctBusyMail struct {
	*fake.Store
	busy       atomic.Bool
	refused    atomic.Int64
	foreground atomic.Int64
}

var _ MailStore = (*acctBusyMail)(nil)

func (m *acctBusyMail) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	if !mailstore.IsBackground(ctx) {
		m.foreground.Add(1)
	} else if m.busy.Load() {
		m.refused.Add(1)
		return nil, mailstore.ErrBusy
	}
	return m.Store.FetchHeaders(ctx, src, uids)
}

// acctOrderMail is a MailStore that records the folder of every header
// fetch, in order.
type acctOrderMail struct {
	*fake.Store
	mu      sync.Mutex
	fetched []string
}

var _ MailStore = (*acctOrderMail)(nil)

func (m *acctOrderMail) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	m.mu.Lock()
	m.fetched = append(m.fetched, src.Folder)
	m.mu.Unlock()
	return m.Store.FetchHeaders(ctx, src, uids)
}

func (m *acctOrderMail) folders() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.fetched)
}

// acctSettings are the cache settings every rig starts with: the config
// defaults, except that backfill_pause is 0 so batches need no clock
// advance.
func acctSettings() config.CacheSettings {
	return config.CacheSettings{
		Enabled: true, PrefetchBodies: 90 * 24 * time.Hour, ReadRetention: 30 * 24 * time.Hour,
		DailyDownload: 1 << 30, BackfillBatch: 250, BackfillPause: 0, FreshLag: 30 * time.Second,
	}
}

// acctAccount is the account acme on preset, with sync enabled and a five
// minute poll.
func acctAccount(t *testing.T, preset string, watchFolders, syncFolders []string) config.ResolvedAccount {
	t.Helper()
	p, ok := provider.Lookup(preset)
	if !ok {
		t.Fatalf("no preset %q", preset)
	}
	return config.ResolvedAccount{
		Name: "acme", Address: "me@example.com", Provider: p,
		Watch: watchFolders, SyncFolders: syncFolders, SyncPoll: 5 * time.Minute, SyncEnabled: true,
	}
}

// acctMsg is a message in folder with subject name and Message-ID
// name@example.com.
func acctMsg(folder, name string) fake.Message {
	return fake.Message{
		Folder: folder, Subject: name, Date: acctStart.Add(-time.Hour), Body: "hello " + name,
		From:      []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
		To:        []mailstore.Address{{Email: "me@example.com"}},
		MessageID: name + "@example.com",
	}
}

// acctRig runs one Account against a fake store, a real state.db and a
// real cache.db.
type acctRig struct {
	t         *testing.T
	fs        *fake.Store
	mail      MailStore // what Mail hands out; set it before start
	clock     *acctClock
	stateDB   *sql.DB
	log       *watch.Log
	state     *watch.State
	engine    *watch.Engine
	store     *Store
	cachePath string
	acct      config.ResolvedAccount

	mu       sync.Mutex
	settings config.CacheSettings

	a      *Account
	cancel context.CancelFunc
	done   chan struct{}
}

func newAcctRig(t *testing.T, fs *fake.Store, acct config.ResolvedAccount) *acctRig {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	clock := newAcctClock()
	r := &acctRig{t: t, fs: fs, mail: fs, clock: clock, stateDB: conn, acct: acct, settings: acctSettings()}
	r.log = watch.NewLog(conn, clock.Now, nil)
	r.state = watch.NewState(conn)
	r.engine = watch.NewEngine(conn, r.log, r.state, watch.EngineOptions{Now: clock.Now})
	r.cachePath = filepath.Join(t.TempDir(), "cache.db")
	if r.store, err = Open(t.Context(), r.cachePath, OpenOptions{Now: clock.Now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.store.Close() }) // a close error at test end changes nothing
	r.a = r.newAccount()
	return r
}

func (r *acctRig) newAccount() *Account {
	return NewAccount(AccountOptions{
		Account: r.acct, Settings: r.cacheSettings, MaxSize: func() int64 { return 2 << 30 },
		Store: r.store, Log: r.log, State: r.state, Mail: r.openMail, Clock: r.clock,
	})
}

// renew replaces the rig's Account with a new one over the same databases,
// as a daemon restart does.
func (r *acctRig) renew() { r.a = r.newAccount() }

func (r *acctRig) openMail(context.Context) (MailStore, func(), error) {
	return r.mail, func() {}, nil
}

func (r *acctRig) cacheSettings() config.CacheSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

func (r *acctRig) setSettings(f func(*config.CacheSettings)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.settings)
}

// start runs the rig's Account until stop or the end of the test.
func (r *acctRig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	a := r.a
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	r.t.Cleanup(r.stop)
}

// stop cancels Run and waits for it to return. Calling it twice is safe.
func (r *acctRig) stop() {
	r.t.Helper()
	if r.cancel == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(acctWait):
		r.t.Fatal("Run did not return after cancel")
	}
}

// passWith runs one SP6 sync pass on folder through the real engine.
func (r *acctRig) passWith(folder string, sweep bool) {
	r.t.Helper()
	opts := watch.PassOptions{Sweep: sweep, Mode: watch.ModeIdle}
	if _, err := r.engine.Pass(r.t.Context(), r.acct.Name, folder, acctSource{fs: r.fs, folder: folder}, opts); err != nil {
		r.t.Fatalf("pass on %s: %v", folder, err)
	}
}

// pass runs a pass on folder and marks it watching, as a watcher does.
func (r *acctRig) pass(folder string) {
	r.t.Helper()
	r.passWith(folder, false)
	r.setState(folder, watch.StateWatching)
}

// sweep runs a pass that compares the full UID set, which reports expunges.
func (r *acctRig) sweep(folder string) {
	r.t.Helper()
	r.passWith(folder, true)
	r.setState(folder, watch.StateWatching)
}

// add stores a message per name in folder, runs a pass, and returns their refs.
func (r *acctRig) add(folder string, names ...string) []ref.Ref {
	r.t.Helper()
	refs := make([]ref.Ref, 0, len(names))
	for _, n := range names {
		refs = append(refs, r.fs.Add(acctMsg(folder, n)))
	}
	r.pass(folder)
	return refs
}

func (r *acctRig) setState(folder string, s watch.Status) {
	r.t.Helper()
	if err := r.state.SetStatus(r.t.Context(), r.acct.Name, folder, watch.ModeIdle, s, ""); err != nil {
		r.t.Fatalf("SetStatus(%s, %s): %v", folder, s, err)
	}
}

// putFolder writes a sync_folders row as a committed pass would.
func (r *acctRig) putFolder(row watch.FolderRow) {
	r.t.Helper()
	tx, err := r.stateDB.BeginTx(r.t.Context(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if err := watch.PutFolder(r.t.Context(), tx, row); err != nil {
		r.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *acctRig) publish(events ...watch.Event) {
	r.t.Helper()
	if err := r.log.Publish(r.t.Context(), events...); err != nil {
		r.t.Fatalf("Publish: %v", err)
	}
}

// eventually polls cond every 5ms until it holds or acctWait passes.
func (r *acctRig) eventually(desc string, cond func() (bool, error)) {
	r.t.Helper()
	deadline := time.Now().Add(acctWait)
	for {
		ok, err := cond()
		if err != nil {
			r.t.Fatalf("%s: %v", desc, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// advanceUntil moves the clock forward by step until cond holds, for waits
// whose timers several workers register in no fixed order.
func (r *acctRig) advanceUntil(desc string, step time.Duration, cond func() (bool, error)) {
	r.t.Helper()
	r.eventually(desc, func() (bool, error) {
		ok, err := cond()
		if ok || err != nil {
			return ok, err
		}
		r.clock.Advance(step)
		return false, nil
	})
}

// waitApplied waits until the builder has acknowledged the whole log.
func (r *acctRig) waitApplied() {
	r.t.Helper()
	r.eventually("the builder to catch up", func() (bool, error) {
		tail, err := r.log.Tail(r.t.Context())
		return r.a.applied.Load() >= tail, err
	})
}

// waitBackfilled waits until each folder's backfill is done.
func (r *acctRig) waitBackfilled(folders ...string) {
	r.t.Helper()
	for _, f := range folders {
		r.eventually("backfill of "+f, func() (bool, error) {
			p, ok, err := r.store.Progress(r.t.Context(), r.acct.Name, f)
			return ok && p.BackfillDone, err
		})
	}
}

func (r *acctRig) progress(folder string) Progress {
	r.t.Helper()
	p, ok, err := r.store.Progress(r.t.Context(), r.acct.Name, folder)
	if err != nil || !ok {
		r.t.Fatalf("progress of %s: ok %v, err %v", folder, ok, err)
	}
	return p
}

func (r *acctRig) syncRow(folder string) watch.FolderRow {
	r.t.Helper()
	row, ok, err := r.state.Folder(r.t.Context(), r.acct.Name, folder)
	if err != nil || !ok {
		r.t.Fatalf("sync row of %s: ok %v, err %v", folder, ok, err)
	}
	return row
}

// count is how many rows the cache holds for folder under uidValidity.
func (r *acctRig) count(folder string, uidValidity uint32) uint32 {
	r.t.Helper()
	src := mailstore.Source{Folder: folder, UIDValidity: uidValidity}
	_, n, err := r.store.SearchUIDs(r.t.Context(), r.acct.Name, src, Where{}, 0, true)
	if err != nil {
		r.t.Fatalf("counting %s: %v", folder, err)
	}
	return n
}

func (r *acctRig) row(rf ref.Ref) (Row, bool) {
	r.t.Helper()
	row, ok, err := r.store.Row(r.t.Context(), rf)
	if err != nil {
		r.t.Fatalf("Row(%v): %v", rf, err)
	}
	return row, ok
}

func (r *acctRig) putRows(rows ...Row) {
	r.t.Helper()
	if err := r.store.PutRows(r.t.Context(), rows); err != nil {
		r.t.Fatalf("PutRows: %v", err)
	}
}

// acked is the builder group's acknowledged ID.
func (r *acctRig) acked() int64 {
	r.t.Helper()
	groups, err := r.log.Groups(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == watch.CacheGroup(r.acct.Name) {
			return g.AckedID
		}
	}
	r.t.Fatalf("no group %s", watch.CacheGroup(r.acct.Name))
	return 0
}

func (r *acctRig) tail() int64 {
	r.t.Helper()
	tail, err := r.log.Tail(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	return tail
}

func (r *acctRig) budget(day string) int64 {
	r.t.Helper()
	n, err := r.store.Budget(r.t.Context(), r.acct.Name, day)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// failInserts makes every insert into messages fail, as a SQLite write
// error would, through a trigger added on a second connection. The
// returned function removes it.
func (r *acctRig) failInserts() func() {
	r.t.Helper()
	dsn := "file:" + r.cachePath + "?" + url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		r.t.Fatal(err)
	}
	const trigger = `CREATE TRIGGER acct_fail_insert BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`
	if _, err := conn.ExecContext(r.t.Context(), trigger); err != nil {
		r.t.Fatalf("adding the failing trigger: %v", err)
	}
	return func() {
		r.t.Helper()
		if _, err := conn.ExecContext(r.t.Context(), "DROP TRIGGER acct_fail_insert"); err != nil {
			r.t.Fatalf("dropping the failing trigger: %v", err)
		}
		_ = conn.Close() // a close error changes nothing for the test
	}
}
