package calsync_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/calsync"
)

// startRun runs the syncer until the test ends or stop is called.
func startRun(t *testing.T, r *rig) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.syncer.Run(ctx) })
	stop = sync.OnceFunc(func() { cancel(); wg.Wait() })
	t.Cleanup(stop)
	return stop
}

// TestRunPolls checks that Run syncs at once, then every Poll.
func TestRunPolls(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	startRun(t, r)
	w := r.clock.next(t)
	if w.d != poll {
		t.Fatalf("first sleep %v, want %v", w.d, poll)
	}
	if n := r.store.Count(fake.MethodListCalendars); n != 1 {
		t.Errorf("ListCalendars called %d times before the first sleep, want 1", n)
	}
	r.clock.set(t0.Add(poll))
	w.ch <- t0.Add(poll)
	if w := r.clock.next(t); w.d != poll {
		t.Errorf("second sleep %v, want %v", w.d, poll)
	}
	if n := r.store.Count(fake.MethodListCalendars); n != 2 {
		t.Errorf("ListCalendars called %d times, want 2", n)
	}
}

// TestRunBacksOff checks the failure backoff: Poll doubling to 30 minutes,
// then back to Poll after a success. Waits longer than five minutes come
// in five minute steps.
func TestRunBacksOff(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Fail(fake.MethodListCalendars, fmt.Errorf("%w: status 503", calstore.ErrUnavailable))
	startRun(t, r)
	now := t0
	waits := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, want := range waits {
		for left := want; left > 0; {
			w := r.clock.next(t)
			if step := min(left, 5*time.Minute); w.d != step {
				t.Fatalf("wait %d: step %v, want %v", i, w.d, step)
			}
			left -= w.d
			now = now.Add(w.d)
			r.clock.set(now)
			if i == len(waits)-1 && left == 0 {
				r.store.Fail(fake.MethodListCalendars, nil)
			}
			w.ch <- now
		}
		if got := r.store.Count(fake.MethodListCalendars); got < i+1 {
			t.Fatalf("after wait %d ListCalendars called %d times", i, got)
		}
	}
	if w := r.clock.next(t); w.d != poll {
		t.Errorf("sleep after recovery %v, want %v", w.d, poll)
	}
	if n := r.store.Count(fake.MethodListCalendars); n != len(waits)+1 {
		t.Errorf("ListCalendars called %d times, want %d", n, len(waits)+1)
	}
	if st := r.syncer.Status(); st.State != calsync.StateReady {
		t.Errorf("Status = %+v, want ready", st)
	}
}

// TestRunAuthWaitsPoll checks that auth_required retries every Poll
// without backing off.
func TestRunAuthWaitsPoll(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Fail(fake.MethodListCalendars, fmt.Errorf("%w: status 401", calstore.ErrAuth))
	startRun(t, r)
	now := t0
	for range 3 {
		w := r.clock.next(t)
		if w.d != poll {
			t.Fatalf("sleep %v, want %v", w.d, poll)
		}
		now = now.Add(w.d)
		r.clock.set(now)
		w.ch <- now
	}
	if st := r.syncer.Status(); st.State != calsync.StateAuthRequired {
		t.Errorf("Status = %+v, want auth_required", st)
	}
}

// TestRunLongSleepsInSteps checks that a sleep longer than five minutes is
// taken in steps that re-read the clock, and that a clock jump (a resume
// from suspend) ends it early.
func TestRunLongSleepsInSteps(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *calsync.Options) { o.Poll = time.Hour })
	startRun(t, r)
	w := r.clock.next(t)
	if w.d != 5*time.Minute {
		t.Fatalf("first step %v, want 5m", w.d)
	}
	r.clock.set(t0.Add(5 * time.Minute))
	w.ch <- t0.Add(5 * time.Minute)
	w = r.clock.next(t)
	if w.d != 5*time.Minute {
		t.Fatalf("second step %v, want 5m", w.d)
	}
	r.clock.set(t0.Add(2 * time.Hour)) // resumed after a long suspend
	w.ch <- t0.Add(2 * time.Hour)
	r.clock.next(t)
	if n := r.store.Count(fake.MethodListCalendars); n != 2 {
		t.Errorf("ListCalendars called %d times, want 2", n)
	}
}

// TestRunPoke checks that a poke runs a cycle before the sleep ends.
func TestRunPoke(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	startRun(t, r)
	r.clock.next(t) // asleep after the first cycle
	r.syncer.Poke()
	r.clock.next(t) // asleep after the poked cycle
	if n := r.store.Count(fake.MethodListCalendars); n != 2 {
		t.Errorf("ListCalendars called %d times, want 2", n)
	}
}

// TestRunStops checks that Run returns when its context ends, mid-sleep.
func TestRunStops(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	stop := startRun(t, r)
	r.clock.next(t)
	stop()
	if st := r.syncer.Status(); st.State != calsync.StateReady {
		t.Errorf("Status = %+v, want ready", st)
	}
}
