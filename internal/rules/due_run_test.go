package rules

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// startDueRun runs r's checker on r's clock until the test ends or stop is
// called. stop cancels and waits for Run to return.
func startDueRun(t *testing.T, r *dueRig) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.checker.Run(ctx, r.clock) })
	stop = func() {
		cancel()
		wg.Wait()
	}
	t.Cleanup(stop)
	return stop
}

func TestRunScansAtStartAndAtEachDueTime(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)
	r := newDueRig(t, start, dueSettings)
	it := r.create(t, r.plain, items.NewItem{Title: "file taxes", DueDate: "2026-11-10"})
	if got := r.checker.NextWake(); !got.IsZero() {
		t.Errorf("NextWake before Run = %v, want zero", got)
	}
	tail := r.tail(t)
	startDueRun(t, r)

	steps := []struct {
		at    time.Time
		wait  time.Duration
		stage watch.Kind
	}{
		{start, time.Hour, watch.KindItemDueSoon},
		{time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), 24 * time.Hour, watch.KindItemDue},
		{time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC), 24 * time.Hour, watch.KindItemOverdue},
	}
	var w dueWait
	for i, s := range steps {
		if i > 0 {
			r.clock.set(s.at)
			w.ch <- s.at
		}
		w = r.clock.next(t)
		if w.d != s.wait {
			t.Errorf("step %d: Run slept %v, want %v", i, w.d, s.wait)
		}
		if got, want := r.checker.NextWake(), s.at.Add(s.wait); !got.Equal(want) {
			t.Errorf("step %d: NextWake = %v, want %v", i, got, want)
		}
		wantDue(t, string(s.stage), r.lines(t, tail), dueLine(s.stage, it))
		tail = r.tail(t)
	}
}

// A failed scan is logged and tried again after the watch backoff, which
// grows while it keeps failing and resets once a scan works.
func TestRunRetriesAFailedScan(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)
	r := newDueRig(t, start, dueSettings)
	it := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE file taxes", DueDate: "2026-11-10"})
	var logs bytes.Buffer
	r.checker.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	r.checker.jitter = func() float64 { return 0.5 } // watch.Backoff then gives exactly 1s, 2s, ...
	r.exec(t, "ALTER TABLE item_due_sent RENAME TO item_due_sent_away")
	tail := r.tail(t)
	stop := startDueRun(t, r)

	w := r.clock.next(t)
	if w.d != time.Second {
		t.Errorf("first retry after %v, want 1s", w.d)
	}
	if got, want := r.checker.NextWake(), start.Add(time.Second); !got.Equal(want) {
		t.Errorf("NextWake = %v, want %v", got, want)
	}
	r.clock.set(start.Add(time.Second))
	w.ch <- start.Add(time.Second)
	w = r.clock.next(t)
	if w.d != 2*time.Second {
		t.Errorf("second retry after %v, want 2s", w.d)
	}
	wantDue(t, "events while failing", r.lines(t, tail))

	r.exec(t, "ALTER TABLE item_due_sent_away RENAME TO item_due_sent")
	r.clock.set(start.Add(3 * time.Second))
	w.ch <- start.Add(3 * time.Second)
	w = r.clock.next(t)
	if want := time.Hour - 3*time.Second; w.d != want {
		t.Errorf("after recovery Run slept %v, want %v until 08:00", w.d, want)
	}
	wantDue(t, "events after recovery", r.lines(t, tail), dueLine(watch.KindItemDueSoon, it))

	stop()
	out := logs.String()
	if got := strings.Count(out, "checking item due dates failed"); got != 2 {
		t.Errorf("logged %d failures, want 2:\n%s", got, out)
	}
	if strings.Contains(out, "SECRET-DUE") {
		t.Errorf("log holds item text:\n%s", out)
	}
}

// A retry never sleeps past the next due time.
func TestRunRetryStopsAtTheDueTime(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, time.Date(2026, 11, 10, 7, 59, 59, 500_000_000, time.UTC), dueSettings)
	r.create(t, r.plain, items.NewItem{Title: "file taxes", DueDate: "2026-11-10"})
	r.checker.jitter = func() float64 { return 0.5 }
	r.exec(t, "ALTER TABLE item_due_sent RENAME TO item_due_sent_away")
	startDueRun(t, r)
	if w := r.clock.next(t); w.d != 500*time.Millisecond {
		t.Errorf("Run slept %v, want 500ms until 08:00", w.d)
	}
}
