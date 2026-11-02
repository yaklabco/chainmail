package watch

import (
	"fmt"
	"time"
)

// timeLayout stores times as fixed-width UTC with nine fractional digits, as
// the audit table does, so the at, created_at, last_seen_at and sync time
// columns sort and range-compare correctly as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t in timeLayout, in UTC. The zero time renders as "",
// which the sync_folders time columns use for never.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

// parseTime reads a time written by formatTime. "" reads as the zero time.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing time %q: %w", s, err)
	}
	return t.UTC(), nil
}
