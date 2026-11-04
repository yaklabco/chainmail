package query

import (
	"testing"
	"time"
)

func TestAgeStart(t *testing.T) {
	t.Parallel()
	// The largest ages the parser accepts do not overflow, and the day is
	// taken in now's own zone.
	now := time.Date(2026, 10, 29, 23, 30, 0, 0, time.FixedZone("EST", -5*3600))
	tests := map[string]time.Time{
		"1d":    time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		"9999d": time.Date(1999, 6, 14, 0, 0, 0, 0, time.UTC),
		"9999w": time.Date(1835, 3, 12, 0, 0, 0, 0, time.UTC),
		"9999m": time.Date(1193, 7, 29, 0, 0, 0, 0, time.UTC),
		"9999y": time.Date(-7973, 10, 29, 0, 0, 0, 0, time.UTC),
	}
	for v, want := range tests {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if got := AgeStart(now, v); !got.Equal(want) {
				t.Errorf("AgeStart(%s) = %v, want %v", v, got, want)
			}
		})
	}

	// Months and years land on the last day of a shorter target month
	// rather than spilling into the next one.
	ends := []struct {
		now  time.Time
		age  string
		want time.Time
	}{
		{time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC), "1m", time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC), "13m", time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC), "1M", time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, 3, 31, 9, 0, 0, 0, time.UTC), "1m", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC), "1y", time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC), "4y", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC), "1m", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range ends {
		t.Run(tt.now.Format(time.DateOnly)+"/"+tt.age, func(t *testing.T) {
			t.Parallel()
			if got := AgeStart(tt.now, tt.age); !got.Equal(tt.want) {
				t.Errorf("AgeStart(%s, %s) = %v, want %v", tt.now.Format(time.DateOnly), tt.age, got, tt.want)
			}
		})
	}
}

// TestAddMonths covers the clamp directly, including the kept time of day
// and location, which AgeStart's midnight UTC days do not show.
func TestAddMonths(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("EST", -5*3600)
	tests := []struct {
		name   string
		t      time.Time
		months int
		want   time.Time
	}{
		{"back to short month", time.Date(2026, 3, 31, 9, 15, 0, 0, zone), -1, time.Date(2026, 2, 28, 9, 15, 0, 0, zone)},
		{"forward to short month", time.Date(2026, 1, 31, 9, 15, 0, 0, zone), 1, time.Date(2026, 2, 28, 9, 15, 0, 0, zone)},
		{"leap day", time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), 1, time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"no clamp", time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), -13, time.Date(2025, 2, 15, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AddMonths(tt.t, tt.months); !got.Equal(tt.want) || got.Location() != tt.want.Location() {
				t.Errorf("AddMonths(%v, %d) = %v, want %v", tt.t, tt.months, got, tt.want)
			}
		})
	}
}
