package imapstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// waiters returns how many foreground calls are blocked in p.get.
func waiters[C pooled](p *pool[C]) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiting
}

// idleCount returns how many connections sit idle in p.
func idleCount[C pooled](p *pool[C]) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// tryGet takes an idle connection, or dials while the pool has room, and
// never waits: at the cap, or while a foreground call waits, it fails at
// once with ErrBusy, leaving any idle connection to the waiter.
func TestPoolTryGet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		size int
		// borrowed and idle connections are set up first; waiting
		// foreground calls are then counted as if blocked in get.
		borrowed, idle, waiting int
		wantBusy                bool
		wantDials               int
		wantIdle                int // left idle after the call
	}{
		{name: "takes an idle connection", size: 2, idle: 1, wantDials: 1},
		{name: "dials when the pool has room", size: 2, wantDials: 1},
		{name: "dials beside a borrowed connection", size: 2, borrowed: 1, wantDials: 2},
		{name: "busy at the cap with none idle", size: 2, borrowed: 2, wantBusy: true, wantDials: 2},
		{name: "busy when a foreground call waits, though there is room", size: 2, borrowed: 1, waiting: 1, wantBusy: true, wantDials: 1},
		{name: "leaves an idle connection to a waiting foreground call", size: 2, borrowed: 1, idle: 1, waiting: 1, wantBusy: true, wantDials: 2, wantIdle: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &fakeDialer{}
			p := newTestPool(tt.size, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
			var out []*fakeConn
			for range tt.borrowed + tt.idle {
				c, err := p.get(t.Context(), "", false)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, c)
			}
			for _, c := range out[tt.borrowed:] {
				p.put(c)
			}
			p.mu.Lock()
			p.waiting = tt.waiting
			p.mu.Unlock()
			c, err := p.tryGet(t.Context(), "", false)
			switch {
			case tt.wantBusy && !errors.Is(err, mailstore.ErrBusy):
				t.Fatalf("tryGet = %v, %v; want ErrBusy", c, err)
			case tt.wantBusy && errors.Is(err, mailstore.ErrUnavailable):
				t.Errorf("tryGet = %v, which matches ErrUnavailable; ErrBusy must not", err)
			case !tt.wantBusy && err != nil:
				t.Fatalf("tryGet = %v, want a connection", err)
			}
			if n := d.count(); n != tt.wantDials {
				t.Errorf("dials = %d, want %d", n, tt.wantDials)
			}
			if n := idleCount(p); n != tt.wantIdle {
				t.Errorf("idle after tryGet = %d, want %d", n, tt.wantIdle)
			}
		})
	}
}

// A foreground call blocked in get counts as waiting until a connection
// comes back to it, and stops counting when it gives up. While it waits,
// background work gets nothing.
func TestPoolForegroundWaitIsCountedAndWoken(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	p := newTestPool(1, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	held, err := p.get(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *fakeConn, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		c, err := p.get(t.Context(), "", false)
		if err != nil {
			t.Error(err)
		}
		got <- c
	})
	eventually(t, func() bool { return waiters(p) == 1 })
	if _, err := p.tryGet(t.Context(), "", false); !errors.Is(err, mailstore.ErrBusy) {
		t.Errorf("tryGet while a foreground call waits = %v, want ErrBusy", err)
	}
	p.put(held)
	wg.Wait()
	if c := <-got; c != held {
		t.Errorf("waiter got conn %d, want the returned conn %d", c.id, held.id)
	}
	if n := waiters(p); n != 0 {
		t.Errorf("waiters after the wait = %d, want 0", n)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.get(ctx, "", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get at the cap = %v, want to wait until the deadline", err)
	}
	if n := waiters(p); n != 0 {
		t.Errorf("waiters after a get gave up = %d, want 0", n)
	}
	if n := d.count(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

func TestPoolTryGetAfterClose(t *testing.T) {
	t.Parallel()
	p := newTestPool(1, &fakeDialer{}, &testClock{t: time.Unix(0, 0)}, time.Hour)
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.tryGet(t.Context(), "", false); !errors.Is(err, errPoolClosed) {
		t.Errorf("tryGet after close = %v, want errPoolClosed", err)
	}
}

// An idle connection that fails its NOOP is dropped and a fresh one dialed,
// as get does.
func TestPoolTryGetReplacesAConnectionThatFailsNoop(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{}
	clock := &testClock{t: time.Unix(0, 0)}
	p := newTestPool(1, d, clock, time.Hour)
	c, err := p.get(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	c.noopErr = errors.New("connection reset")
	p.put(c)
	clock.advance(61 * time.Second)
	fresh, err := p.tryGet(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == c || !c.closed.Load() || d.count() != 2 {
		t.Errorf("got conn %d, old closed %t, dials %d; want a fresh conn after the failed NOOP", fresh.id, c.closed.Load(), d.count())
	}
}

// A failed dial is returned as it is and frees its slot.
func TestPoolTryGetDialError(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	d := &fakeDialer{failNext: refused}
	p := newTestPool(1, d, &testClock{t: time.Unix(0, 0)}, time.Hour)
	if _, err := p.tryGet(t.Context(), "", false); !errors.Is(err, refused) {
		t.Fatalf("tryGet = %v, want the dial's error", err)
	}
	if _, err := p.tryGet(t.Context(), "", false); err != nil {
		t.Errorf("tryGet after a failed dial = %v, want the slot free", err)
	}
}

// Foreground and background borrowers mixed never open more than the cap,
// and every foreground call completes. Run under -race.
func TestPoolMixedForegroundAndBackground(t *testing.T) {
	t.Parallel()
	p := newTestPool(2, &fakeDialer{}, &testClock{t: time.Unix(0, 0)}, time.Hour)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				c, err := p.get(t.Context(), "", false)
				if err != nil {
					t.Error(err)
					return
				}
				p.put(c)
			}
		})
		wg.Go(func() {
			for range 50 {
				c, err := p.tryGet(t.Context(), "", false)
				if errors.Is(err, mailstore.ErrBusy) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				p.put(c)
			}
		})
	}
	wg.Wait()
	if peak := p.connPeak(); peak > 2 {
		t.Errorf("peak = %d, want at most the cap of 2", peak)
	}
	if n := waiters(p); n != 0 {
		t.Errorf("waiters at the end = %d, want 0", n)
	}
}
