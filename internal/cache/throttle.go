package cache

import (
	"sync"
	"time"
)

// throttle is an account's pause after the server refused background work
// for a quota or bandwidth limit (mailstore.ErrThrottled, spec section 8):
// until the next 00:00 UTC, the backfill and the prefetcher wait as for a
// spent budget, and status reports budget_exhausted. The builder and
// foreground reads do not wait. It is kept in memory, so a restart forgets
// it; the server refuses again if its limit still holds. A nil throttle is
// never set. Its methods are safe for concurrent use.
type throttle struct {
	mu    sync.Mutex
	until time.Time
}

// set records a refusal at now, pausing until the next 00:00 UTC. It
// reports whether that starts a pause, false when one was already running.
func (t *throttle) set(now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	started := !now.Before(t.until)
	t.until = now.Add(untilUTCMidnight(now))
	return started
}

// wait is how long from now the pause lasts, zero when none is running.
func (t *throttle) wait(now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(0, t.until.Sub(now))
}
