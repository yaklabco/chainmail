package imapstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConn struct {
	id         int
	folder     string
	writable   bool
	dead       bool
	noopErr    error
	closeDelay time.Duration
	noops      atomic.Int32
	closed     atomic.Bool
}

func (f *fakeConn) Selected() string { return f.folder }
func (f *fakeConn) Writable() bool   { return f.writable }
func (f *fakeConn) Dead() bool       { return f.dead }
func (f *fakeConn) Noop(context.Context) error {
	f.noops.Add(1)
	return f.noopErr
}

func (f *fakeConn) Close() error {
	time.Sleep(f.closeDelay)
	f.closed.Store(true)
	return nil
}

type fakeDialer struct {
	mu         sync.Mutex
	dials      int
	failNext   error
	closeDelay time.Duration
}

func (d *fakeDialer) dial(context.Context) (*fakeConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.failNext; err != nil {
		d.failNext = nil
		return nil, err
	}
	d.dials++
	return &fakeConn{id: d.dials, closeDelay: d.closeDelay}, nil
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestPool(size int, d *fakeDialer, clock *testClock, idleLogout time.Duration) *pool[*fakeConn] {
	return newPool(d.dial, poolOptions{size: size, noopAfter: time.Minute, idleLogout: idleLogout, now: clock.now})
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPoolReusesIdleConnection(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(3, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	c1, err := p.get(t.Context(), "INBOX", false)
	if err != nil {
		t.Fatal(err)
	}
	p.put(c1)
	c2, err := p.get(t.Context(), "INBOX", false)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 || d.count() != 1 {
		t.Errorf("got conn %d after %d dials, want conn %d after 1", c2.id, d.count(), c1.id)
	}
}

func TestPoolPrefersFolder(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(3, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	inbox, _ := p.get(t.Context(), "", false)
	sent, _ := p.get(t.Context(), "", false)
	inbox.folder, sent.folder = "INBOX", "Sent"
	p.put(inbox)
	p.put(sent)
	if got, _ := p.get(t.Context(), "INBOX", false); got != inbox {
		t.Errorf("get(INBOX) = conn %d, want the one on INBOX (%d)", got.id, inbox.id)
	}
	if got, _ := p.get(t.Context(), "Archive", false); got != sent {
		t.Errorf("get(Archive) = conn %d, want the remaining idle conn %d", got.id, sent.id)
	}
}

func TestPoolCapsAndWaits(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(2, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	a, _ := p.get(t.Context(), "", false)
	if _, err := p.get(t.Context(), "", false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := p.get(ctx, "", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third get = %v, want to wait until the deadline", err)
	}
	got := make(chan *fakeConn, 1)
	go func() {
		c, err := p.get(t.Context(), "", false)
		if err != nil {
			t.Error(err)
		}
		got <- c
	}()
	p.put(a)
	select {
	case c := <-got:
		if c != a {
			t.Errorf("waiter got conn %d, want the returned conn %d", c.id, a.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never got a connection")
	}
	if d.count() != 2 {
		t.Errorf("dials = %d, want 2", d.count())
	}
}

func TestPoolNoopAfterIdle(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	clock := &testClock{t: time.Unix(0, 0)}
	p := newTestPool(3, d, clock, time.Hour)
	c, _ := p.get(t.Context(), "", false)
	p.put(c)
	clock.advance(30 * time.Second)
	c, _ = p.get(t.Context(), "", false)
	if n := c.noops.Load(); n != 0 {
		t.Errorf("NOOPs after 30s idle = %d, want 0", n)
	}
	p.put(c)
	clock.advance(61 * time.Second)
	c, _ = p.get(t.Context(), "", false)
	if n := c.noops.Load(); n != 1 {
		t.Errorf("NOOPs after 61s idle = %d, want 1", n)
	}
	c.noopErr = errors.New("connection reset")
	p.put(c)
	clock.advance(61 * time.Second)
	fresh, err := p.get(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == c || !c.closed.Load() || d.count() != 2 {
		t.Errorf("a failed NOOP should discard the conn and dial anew: got %d, closed %t, dials %d", fresh.id, c.closed.Load(), d.count())
	}
}

func TestPoolLogsOutIdleConnections(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(1, d, &testClock{t: time.Unix(0, 0)}, 20*time.Millisecond)
	c, _ := p.get(t.Context(), "", false)
	p.put(c)
	eventually(t, c.closed.Load)
	again, err := p.get(t.Context(), "", false)
	if err != nil || again == c || d.count() != 2 {
		t.Errorf("after idle logout: conn %v, err %v, dials %d", again, err, d.count())
	}
}

func TestPoolShrink(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(3, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	a, _ := p.get(t.Context(), "", false)
	b, _ := p.get(t.Context(), "", false)
	c, _ := p.get(t.Context(), "", false)
	p.shrink()
	p.put(c)
	if !c.closed.Load() {
		t.Error("a connection over the shrunk cap should be logged out on return")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := p.get(ctx, "", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("get at the shrunk cap = %v, want to wait", err)
	}
	for range 5 {
		p.shrink()
	}
	// The cap is now 1 with two borrowed: the first one back is over the
	// cap and logged out, the second is kept, so the cap never drops below one.
	p.put(a)
	p.put(b)
	if !a.closed.Load() || b.closed.Load() {
		t.Errorf("after shrinking to one: a closed %t (want true), b closed %t (want false)", a.closed.Load(), b.closed.Load())
	}
}

func TestPoolDiscardAndDeadConnections(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(1, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	c, _ := p.get(t.Context(), "", false)
	c.dead = true
	p.put(c)
	if !c.closed.Load() {
		t.Error("put of a dead conn should close it")
	}
	c2, err := p.get(t.Context(), "", false)
	if err != nil || c2 == c {
		t.Fatalf("after a dead conn: %v, %v", c2, err)
	}
	p.discard(c2)
	if _, err := p.get(t.Context(), "", false); err != nil || d.count() != 3 {
		t.Errorf("after discard: err %v, dials %d", err, d.count())
	}
}

func TestPoolDialErrorFreesSlot(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{failNext: errors.New("refused")}
	p := newTestPool(1, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	if _, err := p.get(t.Context(), "", false); err == nil {
		t.Fatal("dial error not returned")
	}
	if _, err := p.get(t.Context(), "", false); err != nil {
		t.Errorf("slot not freed after a dial error: %v", err)
	}
}

func TestPoolCloseIsParallelAndFinal(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{closeDelay: 200 * time.Millisecond}
	p := newTestPool(3, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	var conns []*fakeConn
	for range 3 {
		c, _ := p.get(t.Context(), "", false)
		conns = append(conns, c)
	}
	for _, c := range conns {
		p.put(c)
	}
	start := time.Now()
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("close took %v; LOGOUTs should run in parallel", took)
	}
	for _, c := range conns {
		if !c.closed.Load() {
			t.Errorf("conn %d not closed", c.id)
		}
	}
	if _, err := p.get(t.Context(), "", false); !errors.Is(err, errPoolClosed) {
		t.Errorf("get after close = %v, want errPoolClosed", err)
	}
}

func TestPoolPrefersFolderInASufficientMode(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(3, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	ro, _ := p.get(t.Context(), "", false)
	rw, _ := p.get(t.Context(), "", false)
	other, _ := p.get(t.Context(), "", false)
	ro.folder = "INBOX"
	rw.folder, rw.writable = "INBOX", true
	other.folder = "Sent"
	// rw goes back first, so most-recent order alone would pick ro or other.
	p.put(rw)
	p.put(ro)
	p.put(other)
	got, _ := p.get(t.Context(), "INBOX", true)
	if got != rw {
		t.Errorf("get(INBOX, writable) = conn %d, want the read-write one (%d)", got.id, rw.id)
	}
	p.put(got)
	// A read accepts either mode.
	for range 2 {
		c, _ := p.get(t.Context(), "INBOX", false)
		if c != ro && c != rw {
			t.Errorf("get(INBOX, read) = conn %d, want one with INBOX open", c.id)
		}
		defer p.put(c)
	}
}

func TestPoolShrinkReportsTheNewCap(t *testing.T) {
	t.Parallel()
	p := newTestPool(3, &fakeDialer{}, &testClock{t: time.Unix(0, 0)}, time.Hour)
	for _, want := range []int{2, 1, 1} {
		if got := p.shrink(); got != want {
			t.Errorf("shrink() = %d, want %d", got, want)
		}
		if got := p.size(); got != want {
			t.Errorf("size() = %d, want %d", got, want)
		}
	}
}
