package cache

import (
	"testing"
	"time"
)

// Budget days are UTC dates, whatever zone the clock reports in, and the
// wait for the next day ends at 00:00 UTC.
func TestBudgetDays(t *testing.T) {
	t.Parallel()
	est := time.FixedZone("EST", -5*3600)
	tests := []struct {
		name     string
		now      time.Time
		wantDay  string
		wantWait time.Duration
	}{
		{name: "morning", now: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), wantDay: "2026-11-03", wantWait: 14 * time.Hour},
		{name: "one second before midnight", now: time.Date(2026, 11, 3, 23, 59, 59, 0, time.UTC), wantDay: "2026-11-03", wantWait: time.Second},
		{name: "midnight starts the day", now: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), wantDay: "2026-11-04", wantWait: 24 * time.Hour},
		{name: "another zone counts in UTC", now: time.Date(2026, 11, 3, 21, 0, 0, 0, est), wantDay: "2026-11-04", wantWait: 22 * time.Hour},
		{name: "month end", now: time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC), wantDay: "2026-11-30", wantWait: 12 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := utcDay(tt.now); got != tt.wantDay {
				t.Errorf("utcDay = %q, want %q", got, tt.wantDay)
			}
			if got := untilUTCMidnight(tt.now); got != tt.wantWait {
				t.Errorf("untilUTCMidnight = %v, want %v", got, tt.wantWait)
			}
		})
	}
}
