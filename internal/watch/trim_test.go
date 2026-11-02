package watch

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const week = 168 * time.Hour

func TestIDsAscendAndAreNeverReused(t *testing.T) {
	t.Parallel()
	l, conn, clock := newLog(t)
	first := publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2), newEvent("a", "INBOX", 3))
	if got := ids(first); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("IDs = %v, want [1 2 3]", got)
	}
	clock.Set(t0.Add(week + time.Hour))
	if n, err := l.Trim(t.Context(), week, 1000); err != nil || n != 3 {
		t.Fatalf("Trim = %d, %v, want 3 deleted", n, err)
	}
	if n := countEvents(t, conn); n != 0 {
		t.Fatalf("%d events left after trimming all", n)
	}
	next := publish(t, l, newEvent("a", "INBOX", 4))
	if next[0].ID != 4 {
		t.Errorf("ID after trimming everything = %d, want 4", next[0].ID)
	}
	tail, err := l.Tail(t.Context())
	if err != nil || tail != 4 {
		t.Errorf("Tail = %d, %v, want 4", tail, err)
	}
}

func TestReadTrimmedGap(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	for uid := range uint32(6) {
		publish(t, l, newEvent("a", "INBOX", uid+1))
	}
	if n, err := l.Trim(t.Context(), week, 3); err != nil || n != 3 {
		t.Fatalf("Trim = %d, %v, want 3", n, err)
	}
	clock.Set(t0.Add(time.Minute))
	wantGap := Event{At: t0.Add(time.Minute), Kind: KindGap, Reason: GapTrimmed, NextID: 4}

	page, err := l.Read(t.Context(), Query{After: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 4 || !reflect.DeepEqual(page.Events[0], wantGap) {
		t.Fatalf("Read(after 0) = %+v, want the trimmed gap then 3 events", page.Events)
	}
	if got := ids(page.Events[1:]); !slices.Equal(got, []int64{4, 5, 6}) || page.Next != 6 {
		t.Errorf("after the gap: %v next %d, want [4 5 6] next 6", got, page.Next)
	}

	tests := []struct {
		name     string
		q        Query
		wantGap  bool
		want     []int64
		wantNext int64
	}{
		{name: "after below oldest", q: Query{After: 2}, wantGap: true, want: []int64{4, 5, 6}, wantNext: 6},
		{name: "after just below oldest", q: Query{After: 3}, want: []int64{4, 5, 6}, wantNext: 6},
		{name: "after at tail", q: Query{After: 6}, want: []int64{}, wantNext: 6},
		{name: "after above tail", q: Query{After: 8}, want: []int64{}, wantNext: 8},
		{name: "gap counts toward the limit", q: Query{After: 0, Limit: 1}, wantGap: true, want: []int64{}, wantNext: 3},
		{name: "gap filtered out", q: Query{After: 0, Filter: Filter{Kinds: []Kind{KindNew, KindStatus}}}, want: []int64{4, 5, 6}, wantNext: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := l.Read(t.Context(), tt.q)
			if err != nil {
				t.Fatal(err)
			}
			events := page.Events
			gotGap := len(events) > 0 && events[0].Kind == KindGap
			if gotGap {
				if !reflect.DeepEqual(events[0], wantGap) {
					t.Errorf("gap = %+v, want %+v", events[0], wantGap)
				}
				events = events[1:]
			}
			if gotGap != tt.wantGap || !slices.Equal(ids(events), tt.want) || page.Next != tt.wantNext {
				t.Errorf("Read(%+v) = gap %v, %v next %d, want gap %v, %v next %d",
					tt.q, gotGap, ids(events), page.Next, tt.wantGap, tt.want, tt.wantNext)
			}
		})
	}
}

// With every event trimmed, the gap points past the tail and Next moves to
// the tail, so the following read neither repeats the gap nor skips anything.
func TestReadGapWhenEverythingTrimmed(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2), newEvent("a", "INBOX", 3))
	clock.Set(t0.Add(week + time.Hour))
	if _, err := l.Trim(t.Context(), week, 1000); err != nil {
		t.Fatal(err)
	}
	page, err := l.Read(t.Context(), Query{After: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{{At: t0.Add(week + time.Hour), Kind: KindGap, Reason: GapTrimmed, NextID: 4}}
	if !reflect.DeepEqual(page.Events, want) || page.Next != 3 {
		t.Fatalf("Read = %+v next %d, want %+v next 3", page.Events, page.Next, want)
	}
	got, next := readIDs(t, l, Query{After: page.Next})
	if len(got) != 0 || next != 3 {
		t.Errorf("Read(after 3) = %v next %d, want nothing next 3", got, next)
	}
	publish(t, l, newEvent("a", "INBOX", 4))
	if got, _ := readIDs(t, l, Query{After: 3}); !slices.Equal(got, []int64{4}) {
		t.Errorf("Read(after 3) after a new event = %v, want [4]", got)
	}
}

func TestTrimByAge(t *testing.T) {
	t.Parallel()
	l, conn, clock := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2))
	clock.Set(t0.Add(100 * time.Hour))
	publish(t, l, newEvent("a", "INBOX", 3), newEvent("a", "INBOX", 4))
	clock.Set(t0.Add(week + time.Hour))
	n, err := l.Trim(t.Context(), week, 1000)
	if err != nil || n != 2 {
		t.Fatalf("Trim = %d, %v, want 2", n, err)
	}
	if got, _ := readIDs(t, l, Query{After: 2}); !slices.Equal(got, []int64{3, 4}) {
		t.Errorf("left %v, want [3 4]", got)
	}
	if c := countEvents(t, conn); c != 2 {
		t.Errorf("%d events left, want 2", c)
	}
}

func TestTrimByMaxEvents(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	events := make([]Event, 10)
	for i := range events {
		events[i] = newEvent("a", "INBOX", uint32(i+1))
	}
	publish(t, l, events...)
	for _, want := range []int64{6, 0} {
		n, err := l.Trim(t.Context(), week, 4)
		if err != nil || n != want {
			t.Errorf("Trim = %d, %v, want %d", n, err, want)
		}
	}
	if got, _ := readIDs(t, l, Query{After: 6}); !slices.Equal(got, []int64{7, 8, 9, 10}) {
		t.Errorf("left %v, want [7 8 9 10]", got)
	}
}

func TestTrimRejectsBadLimits(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	for _, tt := range []struct {
		retention time.Duration
		max       int
	}{{0, 1000}, {-time.Hour, 1000}, {week, 0}} {
		if _, err := l.Trim(t.Context(), tt.retention, tt.max); err == nil {
			t.Errorf("Trim(%s, %d) accepted bad limits", tt.retention, tt.max)
		}
	}
}

// Run trims on entry and returns when its context ends.
func TestRunTrimsUntilCancelled(t *testing.T) {
	t.Parallel()
	l, conn, clock := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2))
	clock.Set(t0.Add(week + time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { l.Run(ctx, time.Hour, week, 1000) })
	deadline := time.Now().Add(5 * time.Second)
	for countEvents(t, conn) != 0 {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatal("Run did not trim within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
}

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A trim that fails while ctx is live is logged at error level.
func TestRunLogsTrimFailure(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	var logs syncBuffer
	l.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { l.Run(ctx, time.Hour, 0, 1000) }) // a zero retention fails every trim
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "trimming the event log failed") {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatalf("no trim failure logged; log = %q", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("log = %q, want an error-level record", logs.String())
	}
}

// A trim cut short by shutdown is expected and is not reported, and a nil
// Logger is safe.
func TestRunDoesNotLogCancellation(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	var logs syncBuffer
	l.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	l.Run(ctx, time.Hour, 0, 1000)
	if got := logs.String(); got != "" {
		t.Errorf("log = %q, want nothing on cancellation", got)
	}
	l.Logger = nil
	l.Run(ctx, time.Hour, 0, 1000)
}

// The age trim removes only a prefix of IDs, so a reader whose start is
// still retained never loses an event without a gap. An event older than
// retention that sits after a retained one stays until everything before it
// is trimmed.
func TestTrimByAgeKeepsAPrefix(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	clock.Set(t0.Add(2 * time.Hour))
	first := newEvent("a", "INBOX", 1)
	backdated := newEvent("a", "INBOX", 2)
	backdated.At = t0 // supplied by the caller, or the clock stepped back
	last := newEvent("a", "INBOX", 3)
	publish(t, l, first, backdated, last)
	clock.Set(t0.Add(week + time.Hour))
	if n, err := l.Trim(t.Context(), week, 1000); err != nil || n != 0 {
		t.Fatalf("Trim = %d, %v, want nothing deleted", n, err)
	}
	if got, _ := readIDs(t, l, Query{After: 1}); !slices.Equal(got, []int64{2, 3}) {
		t.Errorf("Read(after 1) = %v, want [2 3]", got)
	}
	clock.Set(t0.Add(week + 3*time.Hour))
	if n, err := l.Trim(t.Context(), week, 1000); err != nil || n != 3 {
		t.Errorf("Trim once all are old = %d, %v, want 3", n, err)
	}
}

// A trim larger than one batch deletes in batches, each committed on its
// own, so it never holds the only connection for the whole trim. The age
// trim still removes only a prefix: a backdated event after a retained one
// stays.
func TestTrimDeletesInBatches(t *testing.T) {
	t.Parallel()
	l, conn, clock := newLog(t)
	l.trimBatch = 4
	var left []int
	l.afterTrimBatch = func() { left = append(left, countEvents(t, conn)) }
	old := make([]Event, 10)
	for i := range old {
		old[i] = newEvent("a", "INBOX", uint32(i+1))
	}
	publish(t, l, old...) // 1 to 10
	clock.Set(t0.Add(100 * time.Hour))
	backdated := newEvent("a", "INBOX", 14)
	backdated.At = t0
	publish(t, l, newEvent("a", "INBOX", 11), newEvent("a", "INBOX", 12), newEvent("a", "INBOX", 13),
		backdated, newEvent("a", "INBOX", 15))
	clock.Set(t0.Add(week + time.Hour))
	n, err := l.Trim(t.Context(), week, 3)
	if err != nil || n != 12 {
		t.Fatalf("Trim = %d, %v, want 12", n, err)
	}
	// Age: 1 to 4, 5 to 8, 9 and 10. Count: 11 and 12.
	if want := []int{11, 7, 5, 3}; !slices.Equal(left, want) {
		t.Errorf("events left after each batch = %v, want %v", left, want)
	}
	if got, _ := readIDs(t, l, Query{After: 12}); !slices.Equal(got, []int64{13, 14, 15}) {
		t.Errorf("left %v, want [13 14 15]", got)
	}
}
