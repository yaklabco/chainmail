package watch

import (
	"testing"
	"time"
)

// Backoff starts at 1s, doubles, caps at 5 minutes, then scales by
// 0.8 + 0.4*jitter.
func TestBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		n      int
		jitter float64
		want   time.Duration
	}{
		{name: "first attempt", n: 0, jitter: 0.5, want: time.Second},
		{name: "doubles", n: 1, jitter: 0.5, want: 2 * time.Second},
		{name: "doubles again", n: 2, jitter: 0.5, want: 4 * time.Second},
		{name: "last step below the cap", n: 8, jitter: 0.5, want: 256 * time.Second},
		{name: "capped", n: 9, jitter: 0.5, want: maxBackoff},
		{name: "stays capped", n: 1000, jitter: 0.5, want: maxBackoff},
		{name: "negative attempt counts as the first", n: -3, jitter: 0.5, want: time.Second},
		{name: "lowest jitter", n: 0, jitter: 0, want: 800 * time.Millisecond},
		{name: "lowest jitter at the cap", n: 20, jitter: 0, want: 4 * time.Minute},
		{name: "jitter below zero clamps to zero", n: 0, jitter: -1, want: 800 * time.Millisecond},
		{name: "jitter above one clamps to one", n: 0, jitter: 2, want: 1200 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Backoff(tt.n, tt.jitter); got != tt.want {
				t.Errorf("Backoff(%d, %v) = %v, want %v", tt.n, tt.jitter, got, tt.want)
			}
		})
	}
}

// Every jittered wait stays within 0.8 to 1.2 of its unjittered step, and
// the schedule never shrinks from one attempt to the next.
func TestBackoffJitterBounds(t *testing.T) {
	t.Parallel()
	for n := range 40 {
		base := min(time.Second<<min(n, 9), maxBackoff)
		lo, hi := base*8/10, base*12/10
		for _, j := range []float64{0, 0.1, 0.5, 0.9, 0.999999} {
			if d := Backoff(n, j); d < lo || d >= hi {
				t.Errorf("Backoff(%d, %v) = %v, want in [%v, %v)", n, j, d, lo, hi)
			}
		}
		if n > 0 && Backoff(n, 0.5) < Backoff(n-1, 0.5) {
			t.Errorf("Backoff(%d) = %v is below Backoff(%d) = %v", n, Backoff(n, 0.5), n-1, Backoff(n-1, 0.5))
		}
	}
}

func TestSystemClock(t *testing.T) {
	t.Parallel()
	before := time.Now()
	if now := realClock.Now(); now.Before(before) {
		t.Errorf("realClock.Now() = %v, before %v", now, before)
	}
	select {
	case <-realClock.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("realClock.After(1ms) never fired")
	}
}
