package limits

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jamesainslie/chainmail/internal/db"
)

var errRefused = errors.New("too many connections")

// fakeConn records whether it was closed.
type fakeConn struct {
	mu     sync.Mutex
	closed int
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *fakeConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// fakeDialer accepts cap connections and refuses every one after that.
type fakeDialer struct {
	cap int
	// after, when set, runs after each successful dial with its 1-based count.
	after func(n int)

	mu    sync.Mutex
	conns []*fakeConn
	dials []time.Time
}

func (d *fakeDialer) dial(ctx context.Context) (io.Closer, error) {
	d.mu.Lock()
	d.dials = append(d.dials, time.Now())
	if err := ctx.Err(); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	if len(d.conns) >= d.cap {
		d.mu.Unlock()
		return nil, errRefused
	}
	c := &fakeConn{}
	d.conns = append(d.conns, c)
	n := len(d.conns)
	d.mu.Unlock()
	if d.after != nil {
		d.after(n)
	}
	return c, nil
}

func (d *fakeDialer) opened() []*fakeConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*fakeConn(nil), d.conns...)
}

func (d *fakeDialer) dialTimes() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.dials...)
}

func assertAllClosedOnce(t *testing.T, conns []*fakeConn) {
	t.Helper()
	for i, c := range conns {
		if got := c.closeCount(); got != 1 {
			t.Errorf("connection %d closed %d times, want 1", i+1, got)
		}
	}
}

func TestProbeCountsUntilRefusal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDialer{cap: 5}
		got, err := Probe(t.Context(), d.dial, ProbeOptions{})
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if got != 5 {
			t.Errorf("cap = %d, want 5", got)
		}
		if n := len(d.dialTimes()); n != 6 {
			t.Errorf("dials = %d, want 6 (5 accepted, 1 refused)", n)
		}
		conns := d.opened()
		if len(conns) != 5 {
			t.Fatalf("opened %d, want 5", len(conns))
		}
		assertAllClosedOnce(t, conns)
	})
}

func TestProbeStopsAtCeiling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ceiling int
		want    int
	}{
		{name: "default ceiling is 16", ceiling: 0, want: 16},
		{name: "explicit ceiling", ceiling: 4, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				d := &fakeDialer{cap: 100}
				got, err := Probe(t.Context(), d.dial, ProbeOptions{Ceiling: tt.ceiling})
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				if got != tt.want {
					t.Errorf("cap = %d, want %d", got, tt.want)
				}
				if n := len(d.dialTimes()); n != tt.want {
					t.Errorf("dials = %d, want %d (no dial past the ceiling)", n, tt.want)
				}
				assertAllClosedOnce(t, d.opened())
			})
		})
	}
}

func TestProbeFirstFailureIsError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDialer{cap: 0}
		got, err := Probe(t.Context(), d.dial, ProbeOptions{})
		if !errors.Is(err, errRefused) {
			t.Fatalf("err = %v, want it to wrap %v", err, errRefused)
		}
		if got != 0 {
			t.Errorf("cap = %d, want 0", got)
		}
		if n := len(d.dialTimes()); n != 1 {
			t.Errorf("dials = %d, want 1", n)
		}
	})
}

func TestProbeCancelClosesOpened(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		d := &fakeDialer{cap: 100, after: func(n int) {
			if n == 3 {
				cancel()
			}
		}}
		got, err := Probe(ctx, d.dial, ProbeOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if got != 0 {
			t.Errorf("cap = %d, want 0 for an interrupted probe", got)
		}
		conns := d.opened()
		if len(conns) != 3 {
			t.Fatalf("opened %d, want 3", len(conns))
		}
		assertAllClosedOnce(t, conns)
	})
}

func TestProbeCancelDuringDialIsError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		d := &fakeDialer{cap: 100}
		// The third dial sees a canceled context and fails with it. That failure
		// is the cancellation, not a connection limit, so it is no cap.
		dial := func(ctx context.Context) (io.Closer, error) {
			if len(d.dialTimes()) == 2 {
				cancel()
			}
			return d.dial(ctx)
		}
		got, err := Probe(ctx, dial, ProbeOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if got != 0 {
			t.Errorf("cap = %d, want 0", got)
		}
		assertAllClosedOnce(t, d.opened())
	})
}

func TestProbeHonoursInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "default is 250ms", interval: 0, want: 250 * time.Millisecond},
		{name: "explicit", interval: time.Second, want: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				d := &fakeDialer{cap: 3}
				start := time.Now()
				if _, err := Probe(t.Context(), d.dial, ProbeOptions{Interval: tt.interval}); err != nil {
					t.Fatalf("Probe: %v", err)
				}
				times := d.dialTimes()
				if len(times) != 4 {
					t.Fatalf("dials = %d, want 4", len(times))
				}
				if !times[0].Equal(start) {
					t.Errorf("first dial at +%v, want immediately", times[0].Sub(start))
				}
				for i := 1; i < len(times); i++ {
					if gap := times[i].Sub(times[i-1]); gap != tt.want {
						t.Errorf("gap before dial %d = %v, want %v", i+1, gap, tt.want)
					}
				}
			})
		})
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewStore(openDB(t), nil)
	if _, ok, err := s.Get(t.Context(), "work"); err != nil || ok {
		t.Fatalf("Get before Put = ok %v, err %v; want not found", ok, err)
	}
	at := time.Date(2026, 10, 30, 9, 15, 0, 123456789, time.FixedZone("x", 3600))
	if err := s.Put(t.Context(), Limit{Account: "work", IMAPCap: 15, ProbedAt: at}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := s.Get(t.Context(), "work")
	if err != nil || !ok {
		t.Fatalf("Get = ok %v, err %v", ok, err)
	}
	if got.Account != "work" || got.IMAPCap != 15 || !got.ProbedAt.Equal(at) {
		t.Errorf("Get = %+v, want work cap 15 at %v", got, at)
	}
	if got.ProbedAt.Location() != time.UTC {
		t.Errorf("ProbedAt location = %v, want UTC", got.ProbedAt.Location())
	}
	if _, ok, err := s.Get(t.Context(), "home"); err != nil || ok {
		t.Errorf("Get other account = ok %v, err %v; want not found", ok, err)
	}
}

func TestStorePutUpserts(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	s := NewStore(conn, nil)
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	if err := s.Put(t.Context(), Limit{Account: "work", IMAPCap: 15, ProbedAt: first}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), Limit{Account: "work", IMAPCap: 8, ProbedAt: second}); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	got, ok, err := s.Get(t.Context(), "work")
	if err != nil || !ok {
		t.Fatalf("Get = ok %v, err %v", ok, err)
	}
	if got.IMAPCap != 8 || !got.ProbedAt.Equal(second) {
		t.Errorf("Get = %+v, want cap 8 at %v", got, second)
	}
	var rows int
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM account_limits").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1", rows)
	}
}

func TestStorePutDefaultsProbedAtToClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := NewStore(openDB(t), func() time.Time { return fixed })
	if err := s.Put(t.Context(), Limit{Account: "work", IMAPCap: 4}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(t.Context(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ProbedAt.Equal(fixed) {
		t.Errorf("ProbedAt = %v, want %v", got.ProbedAt, fixed)
	}
}

func TestStorePutRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := NewStore(openDB(t), nil)
	for _, l := range []Limit{
		{Account: "", IMAPCap: 3},
		{Account: "work", IMAPCap: 0},
		{Account: "work", IMAPCap: -1},
	} {
		if err := s.Put(t.Context(), l); err == nil {
			t.Errorf("Put(%+v) = nil, want error", l)
		}
	}
}

func TestPoolSize(t *testing.T) {
	t.Parallel()
	limit := func(c int) *Limit { return &Limit{Account: "a", IMAPCap: c} }
	tests := []struct {
		name          string
		configPool    int
		limit         *Limit
		presetDefault int
		want          int
	}{
		{name: "config wins over limit", configPool: 4, limit: limit(20), presetDefault: 6, want: 4},
		{name: "config wins over default", configPool: 9, limit: nil, presetDefault: 3, want: 9},
		{name: "cap 1 clamps to 1", limit: limit(1), presetDefault: 6, want: 1},
		{name: "cap 2 clamps to 1", limit: limit(2), presetDefault: 6, want: 1},
		{name: "cap 3 gives 1", limit: limit(3), presetDefault: 6, want: 1},
		{name: "cap 10 gives 8", limit: limit(10), presetDefault: 6, want: 8},
		{name: "cap 14 gives 12", limit: limit(14), presetDefault: 6, want: 12},
		{name: "cap 20 clamps to 12", limit: limit(20), presetDefault: 6, want: 12},
		{name: "no limit gives google default", limit: nil, presetDefault: 6, want: 6},
		{name: "no limit gives other default", limit: nil, presetDefault: 3, want: 3},
		{name: "negative config is unset", configPool: -1, limit: nil, presetDefault: 3, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := PoolSize(tt.configPool, tt.limit, tt.presetDefault); got != tt.want {
				t.Errorf("PoolSize(%d, %v, %d) = %d, want %d", tt.configPool, tt.limit, tt.presetDefault, got, tt.want)
			}
		})
	}
}

func TestStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{name: "fresh", age: 0, want: false},
		{name: "29 days", age: 29 * day, want: false},
		{name: "just under 30 days", age: 30*day - time.Nanosecond, want: false},
		{name: "30 days", age: 30 * day, want: true},
		{name: "31 days", age: 31 * day, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := Limit{Account: "a", IMAPCap: 5, ProbedAt: now.Add(-tt.age)}
			if got := Stale(l, now); got != tt.want {
				t.Errorf("Stale(age %v) = %v, want %v", tt.age, got, tt.want)
			}
		})
	}
	if !Stale(Limit{Account: "a", IMAPCap: 5}, now) {
		t.Error("Stale(zero ProbedAt) = false, want true")
	}
}
