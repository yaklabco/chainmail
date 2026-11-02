package fake

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// openWatch opens a watch connection on folder and closes it when the test ends.
func openWatch(t *testing.T, s *Store, folder string) mailstore.WatchConn {
	t.Helper()
	c, err := s.OpenWatch(t.Context(), folder)
	if err != nil {
		t.Fatalf("OpenWatch(%s): %v", folder, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// syncConn runs a pass on c or fails the test.
func syncConn(t *testing.T, c mailstore.WatchConn, req mailstore.SyncRequest) mailstore.Delta {
	t.Helper()
	d, err := c.Sync(t.Context(), req)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return d
}

// manualAfter is an After whose timers fire only when the test fires them.
type manualAfter struct {
	mu     sync.Mutex
	asked  []time.Duration
	timers []chan time.Time
}

func (a *manualAfter) After(d time.Duration) <-chan time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch := make(chan time.Time, 1)
	a.asked = append(a.asked, d)
	a.timers = append(a.timers, ch)
	return ch
}

// fire fires every timer handed out so far.
func (a *manualAfter) fire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ch := range a.timers {
		ch <- time.Time{}
	}
	a.timers = nil
}

// waits returns every duration After was asked for.
func (a *manualAfter) waits() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.asked)
}

type idleResult struct {
	wake mailstore.Wake
	err  error
}

// idleAsync runs c.Idle on a goroutine of its own, which the test joins
// when it ends, and gives its result.
func idleAsync(t *testing.T, c mailstore.WatchConn, limit time.Duration) <-chan idleResult {
	t.Helper()
	ctx := t.Context()
	out := make(chan idleResult, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		w, err := c.Idle(ctx, limit)
		out <- idleResult{w, err}
	})
	t.Cleanup(wg.Wait)
	return out
}

// result waits for an Idle's result, failing the test if it never comes.
func result(t *testing.T, ch <-chan idleResult) idleResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Idle did not return")
		return idleResult{}
	}
}

// waitIdle waits until an Idle on folder has started to wait.
func waitIdle(t *testing.T, s *Store, folder string) {
	t.Helper()
	select {
	case got := <-s.IdleStarted():
		if got != folder {
			t.Fatalf("IdleStarted = %q, want %q", got, folder)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Idle started")
	}
}

func TestWatchConn(t *testing.T) {
	t.Parallel()
	s, _ := seedWatch(t)
	ctx := t.Context()
	c := openWatch(t, s, "INBOX")
	if n := s.WatchConns(); n != 1 {
		t.Errorf("WatchConns = %d, want 1", n)
	}
	got := syncConn(t, c, mailstore.SyncRequest{})
	want, err := s.SyncFolder(ctx, "INBOX", mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Errorf("connection pass = %+v, pooled pass = %+v", viewOf(got), viewOf(want))
	}
	s.Add(Message{Folder: "INBOX", Subject: "d"})
	if d := syncConn(t, c, mailstore.SyncRequest{From: cp(1, 4, 4)}); len(d.New) != 1 || d.New[0].Subject != "d" {
		t.Errorf("new = %+v, want d", d.New)
	}

	// A connection keeps the CONDSTORE it opened with.
	s.SetCondStore(false)
	if st := syncConn(t, c, mailstore.SyncRequest{From: cp(1, 5, 5)}).State; !st.CondStore {
		t.Error("an open connection lost CONDSTORE")
	}
	plain := openWatch(t, s, "INBOX")
	if st := syncConn(t, plain, mailstore.SyncRequest{}).State; st.CondStore {
		t.Error("a connection opened without CONDSTORE has it")
	}
	if n := s.WatchConns(); n != 2 {
		t.Errorf("WatchConns = %d, want 2", n)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if n := s.WatchConns(); n != 1 {
		t.Errorf("WatchConns after Close = %d, want 1", n)
	}
	if _, err := c.Sync(ctx, mailstore.SyncRequest{}); !errors.Is(err, errWatchClosed) || errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("Sync after Close = %v, want errWatchClosed", err)
	}
	if _, err := c.Idle(ctx, time.Minute); !errors.Is(err, errWatchClosed) {
		t.Errorf("Idle after Close = %v, want errWatchClosed", err)
	}
	if s.Calls("OpenWatch") != 2 || s.Calls("Sync") != 5 || s.Calls("Idle") != 1 {
		t.Errorf("Calls = OpenWatch %d, Sync %d, Idle %d, want 2, 5, 1", s.Calls("OpenWatch"), s.Calls("Sync"), s.Calls("Idle"))
	}
}

func TestWatchOpenErrors(t *testing.T) {
	t.Parallel()
	s := newHost()
	var nf *mailstore.FolderNotFoundError
	if _, err := s.OpenWatch(t.Context(), "Nope"); !errors.As(err, &nf) || nf.Requested != "Nope" {
		t.Errorf("OpenWatch(Nope) = %v, want a FolderNotFoundError", err)
	}
	g := newGmail()
	if _, err := g.OpenWatch(t.Context(), "INBOX"); !errors.Is(err, mailstore.ErrUnsupported) {
		t.Errorf("OpenWatch on a Gmail label folder = %v, want ErrUnsupported", err)
	}
	openWatch(t, g, allMail)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.OpenWatch(ctx, "INBOX"); !errors.Is(err, context.Canceled) {
		t.Errorf("OpenWatch with a canceled context = %v", err)
	}
	s.Fail("OpenWatch", errBoom)
	for range 2 {
		if _, err := s.OpenWatch(t.Context(), "INBOX"); !errors.Is(err, errBoom) {
			t.Errorf("OpenWatch under Fail = %v, want boom", err)
		}
	}
	s.Fail("OpenWatch", nil)
	if n := s.WatchConns(); n != 0 {
		t.Errorf("failed opens left %d connections", n)
	}

	// The folder a connection is on can go away under it.
	c := openWatch(t, s, "Projects")
	if err := s.DeleteFolder(t.Context(), "Projects", false); err != nil {
		t.Fatal(err)
	}
	nf = nil
	if _, err := c.Sync(t.Context(), mailstore.SyncRequest{}); !errors.As(err, &nf) {
		t.Errorf("Sync on a deleted folder = %v, want a FolderNotFoundError", err)
	}
}

func TestFailWatchConns(t *testing.T) {
	t.Parallel()
	s, _ := seedWatch(t)
	ctx := t.Context()
	s.FailWatch(errBoom)
	if _, err := s.OpenWatch(ctx, "INBOX"); !errors.Is(err, errBoom) {
		t.Errorf("OpenWatch after FailWatch = %v, want boom", err)
	}
	c := openWatch(t, s, "INBOX")
	s.FailWatch(errBoom)
	if _, err := c.Sync(ctx, mailstore.SyncRequest{}); !errors.Is(err, errBoom) {
		t.Errorf("Sync after FailWatch = %v, want boom", err)
	}
	syncConn(t, c, mailstore.SyncRequest{})

	// Idle does not take a pending FailWatch; the next pass does.
	s.FailWatch(errBoom)
	if w, err := c.Idle(ctx, time.Millisecond); err != nil || w != (mailstore.Wake{}) {
		t.Errorf("Idle = %+v, %v, want the max to pass", w, err)
	}
	if _, err := c.Sync(ctx, mailstore.SyncRequest{}); !errors.Is(err, errBoom) {
		t.Errorf("Sync after Idle = %v, want the pending boom", err)
	}
}

// TestCloseClosesWatches checks that Store.Close closes every open watch
// connection without waiting for its owner, as C1 has it: an Idle blocked
// at the time returns, the connections no longer count, and their next
// Sync or Idle fails with ErrUnavailable so a supervisor reconnects.
func TestCloseClosesWatches(t *testing.T) {
	t.Parallel()
	s, _ := seedWatch(t)
	s.SetAfter((&manualAfter{}).After)
	ctx := t.Context()
	idling, other := openWatch(t, s, "INBOX"), openWatch(t, s, "Projects")
	syncConn(t, idling, mailstore.SyncRequest{})
	done := idleAsync(t, idling, time.Minute)
	waitIdle(t, s, "INBOX")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if r := result(t, done); !errors.Is(r.err, mailstore.ErrUnavailable) {
		t.Errorf("Idle blocked during Close = %+v, %v, want ErrUnavailable", r.wake, r.err)
	}
	if n := s.WatchConns(); n != 0 {
		t.Errorf("WatchConns after Close = %d, want 0", n)
	}
	for _, c := range []mailstore.WatchConn{idling, other} {
		if _, err := c.Sync(ctx, mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnavailable) {
			t.Errorf("Sync after Close = %v, want ErrUnavailable", err)
		}
		if _, err := c.Idle(ctx, time.Minute); !errors.Is(err, mailstore.ErrUnavailable) {
			t.Errorf("Idle after Close = %v, want ErrUnavailable", err)
		}
		if err := c.Close(); err != nil {
			t.Errorf("connection Close after Store.Close = %v", err)
		}
	}
	if n := s.WatchConns(); n != 0 {
		t.Errorf("WatchConns after the owners closed = %d, want 0", n)
	}

	// A closed store refuses new watch connections and pooled passes, as
	// imapstore's closed pool does.
	if c, err := s.OpenWatch(ctx, "INBOX"); !errors.Is(err, mailstore.ErrUnavailable) {
		if err == nil {
			_ = c.Close()
		}
		t.Errorf("OpenWatch on a closed store = %v, want ErrUnavailable", err)
	}
	if _, err := s.SyncFolder(ctx, "INBOX", mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("SyncFolder on a closed store = %v, want ErrUnavailable", err)
	}
	if _, err := s.FolderState(ctx, "INBOX"); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("FolderState on a closed store = %v, want ErrUnavailable", err)
	}
	if n := s.WatchConns(); n != 0 {
		t.Errorf("WatchConns after opens on a closed store = %d, want 0", n)
	}
}

// TestSyncKeepsPendingWake checks that a pass never clears a pending wake,
// as imapstore's Sync leaves what the server reported for Idle to take:
// whatever the pass, the next Idle returns the wake at once, and only that
// Idle consumes it. A pass that cannot see deletions (CONDSTORE without a
// sweep) thus still lets the watcher learn it should sweep.
func TestSyncKeepsPendingWake(t *testing.T) {
	t.Parallel()
	changed := mailstore.Wake{Changed: true}
	expunged := mailstore.Wake{Changed: true, Expunged: true}
	tests := []struct {
		name  string
		plain bool // the server has no CONDSTORE
		act   func(s *Store, refs []ref.Ref)
		req   mailstore.SyncRequest
		want  mailstore.Wake
	}{
		{
			name: "expunge, condstore pass without a sweep",
			act:  func(s *Store, refs []ref.Ref) { s.ExternalExpunge("INBOX", refs[1].UID) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4)}, want: expunged,
		},
		{
			name: "expunge, condstore sweep",
			act:  func(s *Store, refs []ref.Ref) { s.ExternalExpunge("INBOX", refs[1].UID) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4), Sweep: true}, want: expunged,
		},
		{
			name: "expunge, rebase",
			act:  func(s *Store, refs []ref.Ref) { s.ExternalExpunge("INBOX", refs[1].UID) },
			want: expunged,
		},
		{
			name: "expunge, full flags without condstore", plain: true,
			act: func(s *Store, refs []ref.Ref) { s.ExternalExpunge("INBOX", refs[1].UID) },
			req: mailstore.SyncRequest{From: cp(1, 4, 0)}, want: expunged,
		},
		{
			name: "arrival, the pass reports it",
			act:  func(s *Store, _ []ref.Ref) { s.Add(Message{Folder: "INBOX", Subject: "d"}) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4)}, want: changed,
		},
		{
			name: "flags, the pass reports them",
			act:  func(s *Store, refs []ref.Ref) { s.ExternalFlags("INBOX", refs[0].UID, []string{`\Seen`}, nil) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4)}, want: changed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, refs := seedWatch(t)
			s.SetCondStore(!tt.plain)
			timers := &manualAfter{}
			s.SetAfter(timers.After)
			c := openWatch(t, s, "INBOX")
			syncConn(t, c, mailstore.SyncRequest{})
			tt.act(s, refs)
			syncConn(t, c, tt.req)
			done := idleAsync(t, c, time.Minute)
			if r := result(t, done); r.err != nil || r.wake != tt.want {
				t.Errorf("Idle after the pass = %+v, %v, want the pending %+v", r.wake, r.err, tt.want)
			}
			select {
			case f := <-s.IdleStarted():
				t.Errorf("Idle with a pending wake started to wait on %s", f)
			default:
			}
			// That Idle consumed the wake: the next one waits for its max.
			done = idleAsync(t, c, time.Minute)
			waitIdle(t, s, "INBOX")
			timers.fire()
			if r := result(t, done); r.err != nil || r.wake != (mailstore.Wake{}) {
				t.Errorf("second Idle = %+v, %v, want the max to pass", r.wake, r.err)
			}
		})
	}
}

// TestIdleCancelWithReadyWake checks that an Idle whose context ends while
// a wake is ready returns ctx.Err() and leaves the wake pending for the
// next Idle, as imapstore rechecks ctx before it takes the report.
func TestIdleCancelWithReadyWake(t *testing.T) {
	t.Parallel()
	for i := range 20 {
		s := newHost()
		s.SetAfter((&manualAfter{}).After)
		c := openWatch(t, s, "INBOX")
		syncConn(t, c, mailstore.SyncRequest{})
		ctx, cancel := context.WithCancel(t.Context())
		out := make(chan idleResult, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			w, err := c.Idle(ctx, time.Minute)
			out <- idleResult{w, err}
		})
		waitIdle(t, s, "INBOX")
		// The wake is ready before ctx ends, and Idle cannot look at it
		// until both have happened.
		s.mu.Lock()
		s.changedLocked("INBOX", false)
		cancel()
		s.mu.Unlock()
		wg.Wait()
		if r := <-out; !errors.Is(r.err, context.Canceled) {
			t.Fatalf("round %d: Idle with a ready wake and an ended ctx = %+v, %v, want context.Canceled", i, r.wake, r.err)
		}
		if w, err := c.Idle(t.Context(), time.Minute); err != nil || w != (mailstore.Wake{Changed: true}) {
			t.Fatalf("round %d: next Idle = %+v, %v, want the wake still pending", i, w, err)
		}
	}
}

func TestDropWatches(t *testing.T) {
	t.Parallel()
	s, _ := seedWatch(t)
	ctx := t.Context()
	inbox, projects := openWatch(t, s, "INBOX"), openWatch(t, s, "Projects")
	s.DropWatches()
	for _, c := range []mailstore.WatchConn{inbox, projects} {
		if _, err := c.Sync(ctx, mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnavailable) {
			t.Errorf("Sync on a dropped connection = %v, want ErrUnavailable", err)
		}
		if _, err := c.Idle(ctx, time.Minute); !errors.Is(err, mailstore.ErrUnavailable) {
			t.Errorf("Idle on a dropped connection = %v, want ErrUnavailable", err)
		}
	}
	if n := s.WatchConns(); n != 2 {
		t.Errorf("WatchConns = %d, want 2: dropped connections count until closed", n)
	}
	fresh := openWatch(t, s, "INBOX")
	syncConn(t, fresh, mailstore.SyncRequest{})
	if err := inbox.Close(); err != nil {
		t.Errorf("Close of a dropped connection = %v", err)
	}
	if n := s.WatchConns(); n != 2 {
		t.Errorf("WatchConns = %d, want 2", n)
	}
}

// TestIdleWakes checks that each change to the watched folder ends an Idle
// with the wake a server's EXISTS, FETCH or EXPUNGE gives, and that a
// change elsewhere does not.
func TestIdleWakes(t *testing.T) {
	t.Parallel()
	changed := mailstore.Wake{Changed: true}
	expunged := mailstore.Wake{Changed: true, Expunged: true}
	tests := []struct {
		name string
		act  func(t *testing.T, s *Store, inbox, project ref.Ref)
		want mailstore.Wake // zero: the Idle ends at its max
	}{
		{name: "add", act: func(_ *testing.T, s *Store, _, _ ref.Ref) { s.Add(Message{Folder: "INBOX", Subject: "b"}) }, want: changed},
		{
			name: "append",
			act: func(t *testing.T, s *Store, _, _ ref.Ref) {
				if _, err := s.Append(t.Context(), "INBOX", nil, time.Time{}, raw("b", "b@x")); err != nil {
					t.Error(err)
				}
			},
			want: changed,
		},
		{
			name: "external flags",
			act: func(_ *testing.T, s *Store, inbox, _ ref.Ref) {
				s.ExternalFlags("INBOX", inbox.UID, []string{`\Seen`}, nil)
			},
			want: changed,
		},
		{
			name: "store flags",
			act: func(t *testing.T, s *Store, inbox, _ ref.Ref) {
				noErr(t, s.StoreFlags(t.Context(), []ref.Ref{inbox}, []string{`\Seen`}, nil))
			},
			want: changed,
		},
		{
			name: "external expunge",
			act:  func(_ *testing.T, s *Store, inbox, _ ref.Ref) { s.ExternalExpunge("INBOX", inbox.UID) },
			want: expunged,
		},
		{
			name: "expunge",
			act:  func(t *testing.T, s *Store, inbox, _ ref.Ref) { noErr(t, s.Expunge(t.Context(), []ref.Ref{inbox})) },
			want: expunged,
		},
		{
			name: "move out",
			act: func(t *testing.T, s *Store, inbox, _ ref.Ref) {
				noErr(t, s.Move(t.Context(), []ref.Ref{inbox}, "Projects"))
			},
			want: expunged,
		},
		{
			name: "move in",
			act: func(t *testing.T, s *Store, _, project ref.Ref) {
				noErr(t, s.Move(t.Context(), []ref.Ref{project}, "INBOX"))
			},
			want: changed,
		},
		{
			name: "another folder",
			act:  func(_ *testing.T, s *Store, _, _ ref.Ref) { s.Add(Message{Folder: "Projects", Subject: "q"}) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			timers := &manualAfter{}
			s.SetAfter(timers.After)
			inbox := s.Add(Message{Folder: "INBOX", Subject: "a"})
			project := s.Add(Message{Folder: "Projects", Subject: "p"})
			c := openWatch(t, s, "INBOX")
			syncConn(t, c, mailstore.SyncRequest{})
			done := idleAsync(t, c, time.Minute)
			waitIdle(t, s, "INBOX")
			tt.act(t, s, inbox, project)
			if tt.want == (mailstore.Wake{}) {
				timers.fire()
			}
			if got := result(t, done); got.err != nil || got.wake != tt.want {
				t.Errorf("Idle = %+v, %v, want %+v", got.wake, got.err, tt.want)
			}
			if w := timers.waits(); !slices.Equal(w, []time.Duration{time.Minute}) {
				t.Errorf("After asked for %v, want [1m]", w)
			}
		})
	}
}

func TestIdle(t *testing.T) {
	t.Parallel()
	t.Run("change before idle", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		c := openWatch(t, s, "INBOX")
		syncConn(t, c, mailstore.SyncRequest{})
		s.Add(Message{Folder: "INBOX", Subject: "a"})
		if w, err := c.Idle(t.Context(), time.Minute); err != nil || w != (mailstore.Wake{Changed: true}) {
			t.Errorf("Idle = %+v, %v, want the pending change at once", w, err)
		}
		select {
		case f := <-s.IdleStarted():
			t.Errorf("Idle with a pending change started to wait on %s", f)
		default:
		}
	})
	t.Run("a change pending before a sync survives it", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		s.SetAfter((&manualAfter{}).After)
		c := openWatch(t, s, "INBOX")
		syncConn(t, c, mailstore.SyncRequest{})
		s.Add(Message{Folder: "INBOX", Subject: "a"})
		if d := syncConn(t, c, mailstore.SyncRequest{From: cp(1, 1, 1)}); len(d.New) != 1 {
			t.Fatalf("pass found %d new, want 1", len(d.New))
		}
		// As against imapstore, the change the pass already reported still
		// wakes the next Idle at once, which then consumes it.
		if r := result(t, idleAsync(t, c, time.Minute)); r.err != nil || r.wake != (mailstore.Wake{Changed: true}) {
			t.Errorf("Idle = %+v, %v, want the pending change at once", r.wake, r.err)
		}
		select {
		case f := <-s.IdleStarted():
			t.Errorf("Idle with a pending change started to wait on %s", f)
		default:
		}
	})
	t.Run("max on the real clock", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		c := openWatch(t, s, "INBOX")
		syncConn(t, c, mailstore.SyncRequest{})
		if w, err := c.Idle(t.Context(), time.Millisecond); err != nil || w != (mailstore.Wake{}) {
			t.Errorf("Idle = %+v, %v, want the max to pass", w, err)
		}
	})
	t.Run("context", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		s.SetAfter((&manualAfter{}).After)
		c := openWatch(t, s, "INBOX")
		syncConn(t, c, mailstore.SyncRequest{})
		ctx, cancel := context.WithCancel(t.Context())
		out := make(chan idleResult, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			w, err := c.Idle(ctx, time.Minute)
			out <- idleResult{w, err}
		})
		waitIdle(t, s, "INBOX")
		cancel()
		wg.Wait()
		if r := <-out; !errors.Is(r.err, context.Canceled) {
			t.Errorf("Idle after cancel = %+v, %v, want context.Canceled", r.wake, r.err)
		}
		// A cancelled Idle leaves the connection usable.
		s.Add(Message{Folder: "INBOX", Subject: "after"})
		if d := syncConn(t, c, mailstore.SyncRequest{From: cp(1, 1, 1)}); len(d.New) != 1 {
			t.Errorf("Sync after a cancelled Idle found %d new, want 1", len(d.New))
		}
		if _, err := c.Idle(ctx, time.Minute); !errors.Is(err, context.Canceled) {
			t.Errorf("Idle on the cancelled context = %v, want context.Canceled at once", err)
		}
		if n := s.WatchConns(); n != 1 {
			t.Errorf("WatchConns after a cancelled Idle = %d, want 1", n)
		}
	})
	t.Run("dropped while idle", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		s.SetAfter((&manualAfter{}).After)
		c := openWatch(t, s, "INBOX")
		syncConn(t, c, mailstore.SyncRequest{})
		done := idleAsync(t, c, time.Minute)
		waitIdle(t, s, "INBOX")
		s.DropWatches()
		if r := result(t, done); !errors.Is(r.err, mailstore.ErrUnavailable) {
			t.Errorf("Idle on a drop = %v, want ErrUnavailable", r.err)
		}
	})
	t.Run("no IDLE", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		before := openWatch(t, s, "INBOX")
		s.SetIdle(false)
		c := openWatch(t, s, "INBOX")
		if !before.IdleSupported() || c.IdleSupported() {
			t.Errorf("IdleSupported = %v before SetIdle(false), %v after, want true then false", before.IdleSupported(), c.IdleSupported())
		}
		if _, err := c.Idle(t.Context(), time.Minute); !errors.Is(err, mailstore.ErrUnsupported) {
			t.Errorf("Idle without IDLE = %v, want ErrUnsupported", err)
		}
		syncConn(t, c, mailstore.SyncRequest{})
	})
	t.Run("standing failure", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		c := openWatch(t, s, "INBOX")
		s.Fail("Idle", errBoom)
		if _, err := c.Idle(t.Context(), time.Minute); !errors.Is(err, errBoom) || s.Calls("Idle") != 1 {
			t.Errorf("Idle under Fail = %v, calls %d", err, s.Calls("Idle"))
		}
	})
}

// TestIdleStartedKeepsLatest checks that IdleStarted drops the oldest
// starts when nobody reads them, so it never blocks an Idle.
func TestIdleStartedKeepsLatest(t *testing.T) {
	t.Parallel()
	s := newHost()
	for i := range idleStartedCap + 6 {
		s.noteIdle(strconv.Itoa(i))
	}
	var got []string
	for len(s.IdleStarted()) > 0 {
		got = append(got, <-s.IdleStarted())
	}
	if len(got) != idleStartedCap || got[0] != "6" || got[len(got)-1] != strconv.Itoa(idleStartedCap+5) {
		t.Errorf("IdleStarted held %d starts, %v to %v, want %d, 6 to %d", len(got), got[0], got[len(got)-1], idleStartedCap, idleStartedCap+5)
	}
}

// TestWatchConcurrent idles and syncs while another goroutine adds mail and
// flags, for -race, and checks that no wake is lost: the watcher's Idle
// timer never fires, so a lost wake hangs it until the deadline.
func TestWatchConcurrent(t *testing.T) {
	t.Parallel()
	const n = 200
	s := newHost()
	s.SetAfter((&manualAfter{}).After)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := openWatch(t, s, "INBOX")
	d := syncConn(t, c, mailstore.SyncRequest{})
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		for i := range n {
			s.Add(Message{Folder: "INBOX", Subject: strconv.Itoa(i)})
			if i%3 == 0 {
				s.ExternalFlags("INBOX", 1, []string{`\Seen`}, nil)
			}
		}
	})
	seen := len(d.UIDs)
	for seen < n {
		if _, err := c.Idle(ctx, time.Hour); err != nil {
			t.Fatalf("Idle after %d of %d messages: %v", seen, n, err)
		}
		d = syncConn(t, c, mailstore.SyncRequest{From: cp(d.State.UIDValidity, d.State.UIDNext, d.State.HighestModSeq)})
		seen += len(d.New)
	}
	if seen != n {
		t.Errorf("saw %d messages, want %d", seen, n)
	}
}
