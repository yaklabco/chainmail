package watch

import "time"

// Clock is the time source the supervisor reads and waits on, so tests can
// run its timers without waiting.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// RealClock is the system clock.
var RealClock Clock = systemClock{}

// systemClock is Clock over package time.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Timing constants, from spec section 5.
const (
	// idleRestart is how long one IDLE runs before the watcher ends it and
	// syncs, which doubles as a liveness check.
	idleRestart = 25 * time.Minute
	// maxBackoff caps the reconnect backoff before jitter.
	maxBackoff = 5 * time.Minute
	// ExpungeSweepFloor is the default shortest time from one sweep to the
	// next one a server EXPUNGE asks for (SupervisorOptions.SweepFloor).
	ExpungeSweepFloor = 5 * time.Minute
	// authRecheck is how often a parked loop asks whether the account is
	// authenticated again.
	authRecheck = 15 * time.Second
	// ShutdownLogout is the bound a WatchConn's Close puts on its logout.
	ShutdownLogout = 5 * time.Second
)

// backoffBase is the first reconnect wait, before jitter.
const backoffBase = time.Second

// backoff is the wait before reconnect attempt n (0-based): 1s doubling,
// capped at 5 minutes, scaled by 0.8 + 0.4*jitter. A negative n counts as
// 0, and jitter is clamped to [0, 1].
func backoff(n int, jitter float64) time.Duration {
	d := backoffBase
	for i := 0; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	d = min(d, maxBackoff)
	jitter = min(max(jitter, 0), 1)
	return time.Duration(float64(d) * (0.8 + 0.4*jitter))
}
