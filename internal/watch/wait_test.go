package watch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Wait ignores a Notify with nothing new, and returns once an event exists.
func TestWaitWakesOnNotify(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { done <- l.Wait(ctx, 0) })
	defer wg.Wait()

	select {
	case err := <-done:
		t.Fatalf("Wait returned %v before any event", err)
	case <-time.After(20 * time.Millisecond):
	}
	l.Notify()
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v after a Notify with no event", err)
	case <-time.After(20 * time.Millisecond):
	}
	publish(t, l, newEvent("a", "INBOX", 1))
	if err := <-done; err != nil {
		t.Errorf("Wait = %v, want nil", err)
	}
}

func TestWaitReturnsWhenContextEnds(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { done <- l.Wait(ctx, 0) })
	cancel()
	wg.Wait()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
}

func TestWaitReturnsAtOnceWhenAnEventExists(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := l.Wait(ctx, 1); err != nil {
		t.Errorf("Wait(after 1) = %v, want nil at once", err)
	}
}

// One Publish wakes every blocked waiter, not just one.
func TestConcurrentWaitersAllWake(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const waiters = 16
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	for i := range waiters {
		wg.Go(func() { errs[i] = l.Wait(ctx, 0) })
	}
	publish(t, l, newEvent("a", "INBOX", 1))
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("waiter %d: %v", i, err)
		}
	}
}
