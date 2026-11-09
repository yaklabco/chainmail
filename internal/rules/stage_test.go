package rules

import (
	"testing"
	"time"
)

func TestDueStage(t *testing.T) {
	t.Parallel()
	la := zone(t, "America/Los_Angeles")
	eight := ClockTime{Hour: 8}
	utcTwo := DueSettings{Location: time.UTC, At: eight, SoonDays: 2}
	utcOff := DueSettings{Location: time.UTC, At: eight}
	laTwo := DueSettings{Location: la, At: eight, SoonDays: 2}
	apiaEight := DueSettings{Location: zone(t, "Pacific/Apia"), At: eight}
	tests := []struct {
		name   string
		due    string
		now    time.Time
		s      DueSettings
		want   string
		wantOK bool
	}{
		{"long before", "2026-11-20", utc(2026, 11, 7, 9, 0), utcTwo, "", false},
		{"just before soon", "2026-11-20", utc(2026, 11, 18, 8, 0).Add(-time.Nanosecond), utcTwo, "", false},
		{"at soon", "2026-11-20", utc(2026, 11, 18, 8, 0), utcTwo, "item_due_soon", true},
		{"the day before due is soon", "2026-11-20", utc(2026, 11, 19, 23, 0), utcTwo, "item_due_soon", true},
		{"just before due", "2026-11-20", utc(2026, 11, 20, 7, 59), utcTwo, "item_due_soon", true},
		{"at due", "2026-11-20", utc(2026, 11, 20, 8, 0), utcTwo, "item_due", true},
		{"due all day", "2026-11-20", utc(2026, 11, 21, 7, 59), utcTwo, "item_due", true},
		{"at overdue", "2026-11-20", utc(2026, 11, 21, 8, 0), utcTwo, "item_overdue", true},
		{"long overdue skips the earlier stages", "2026-01-01", utc(2026, 11, 7, 9, 0), utcTwo, "item_overdue", true},
		{"soon off: no stage before due", "2026-11-20", utc(2026, 11, 19, 9, 0), utcOff, "", false},
		{"soon off: due still fires", "2026-11-20", utc(2026, 11, 20, 8, 0), utcOff, "item_due", true},
		{"negative soon days count as off", "2026-11-20", utc(2026, 11, 19, 9, 0), DueSettings{Location: time.UTC, At: eight, SoonDays: -3}, "", false},
		{"soon across a month end", "2026-12-01", utc(2026, 11, 29, 8, 0), utcTwo, "item_due_soon", true},
		{"due at midnight", "2026-11-20", utc(2026, 11, 20, 0, 0), DueSettings{Location: time.UTC}, "item_due", true},
		{"la wall clock: before 08:00 PST", "2026-11-20", utc(2026, 11, 20, 15, 59), laTwo, "item_due_soon", true},
		{"la wall clock: 08:00 PST", "2026-11-20", utc(2026, 11, 20, 16, 0), laTwo, "item_due", true},
		{"la due on spring forward", "2026-03-08", utc(2026, 3, 8, 15, 0), laTwo, "item_due", true},
		{"la just before 08:00 PDT on spring forward", "2026-03-08", utc(2026, 3, 8, 15, 0).Add(-time.Nanosecond), laTwo, "item_due_soon", true},
		{"la overdue the day after fall back", "2026-10-31", utc(2026, 11, 1, 16, 0), laTwo, "item_overdue", true},
		{"la not overdue an hour earlier", "2026-10-31", utc(2026, 11, 1, 15, 0), laTwo, "item_due", true},
		{"la due at a time in the spring gap", "2026-03-08", utc(2026, 3, 8, 10, 0), DueSettings{Location: la, At: ClockTime{Hour: 2, Minute: 30}}, "item_due", true},
		{"la before the spring gap", "2026-03-08", utc(2026, 3, 8, 10, 0).Add(-time.Nanosecond), DueSettings{Location: la, At: ClockTime{Hour: 2, Minute: 30}}, "", false},
		// Samoa skipped all of 2011-12-30; its due instant is when the clocks
		// land at 12-31 00:00 local, 2011-12-30 10:00 UTC.
		{"apia skipped due date not due at noon the day before", "2011-12-30", utc(2011, 12, 29, 22, 0), apiaEight, "", false},
		{"apia skipped due date just before the jump", "2011-12-30", utc(2011, 12, 30, 10, 0).Add(-time.Nanosecond), apiaEight, "", false},
		{"apia skipped due date due when the clocks land", "2011-12-30", utc(2011, 12, 30, 10, 0), apiaEight, "item_due", true},
		{"bad date", "2026-02-30", utc(2026, 11, 7, 9, 0), utcTwo, "", false},
		{"not a date", "soon", utc(2026, 11, 7, 9, 0), utcTwo, "", false},
		{"empty date", "", utc(2026, 11, 7, 9, 0), utcTwo, "", false},
		{"date with a time", "2026-11-20T00:00:00Z", utc(2026, 12, 7, 9, 0), utcTwo, "", false},
		{"bad clock", "2026-11-20", utc(2026, 12, 7, 9, 0), DueSettings{Location: time.UTC, At: ClockTime{Hour: 25}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DueStage(tt.due, tt.now, tt.s)
			if string(got) != tt.want || ok != tt.wantOK {
				t.Errorf("DueStage(%q, %v) = %q, %v; want %q, %v", tt.due, tt.now, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// The stages are the event kinds T1 adds to internal/watch.
func TestStageValues(t *testing.T) {
	t.Parallel()
	if StageDueSoon != "item_due_soon" || StageDue != "item_due" || StageOverdue != "item_overdue" {
		t.Errorf("stages = %q, %q, %q", StageDueSoon, StageDue, StageOverdue)
	}
}

// A nil location is the host's zone.
func TestDueStageNilLocation(t *testing.T) {
	t.Parallel()
	now := utc(2026, 11, 20, 12, 0)
	got, ok := DueStage("2026-11-10", now, DueSettings{At: ClockTime{Hour: 8}})
	if got != StageOverdue || !ok {
		t.Errorf("DueStage with nil location = %q, %v; want item_overdue", got, ok)
	}
}

func TestNextDueWake(t *testing.T) {
	t.Parallel()
	la := zone(t, "America/Los_Angeles")
	tests := []struct {
		name string
		now  time.Time
		s    DueSettings
		want time.Time
	}{
		{"before the time", utc(2026, 11, 7, 7, 0), DueSettings{Location: time.UTC, At: ClockTime{Hour: 8}}, utc(2026, 11, 7, 8, 0)},
		{"at the time is tomorrow", utc(2026, 11, 7, 8, 0), DueSettings{Location: time.UTC, At: ClockTime{Hour: 8}}, utc(2026, 11, 8, 8, 0)},
		{"after the time", utc(2026, 11, 7, 9, 0), DueSettings{Location: time.UTC, At: ClockTime{Hour: 8}}, utc(2026, 11, 8, 8, 0)},
		{"la across spring forward", utc(2026, 3, 7, 16, 0), DueSettings{Location: la, At: ClockTime{Hour: 8}}, utc(2026, 3, 8, 15, 0)},
		{"la across fall back", utc(2026, 10, 31, 15, 0), DueSettings{Location: la, At: ClockTime{Hour: 8}}, utc(2026, 11, 1, 16, 0)},
		{"la gap time", utc(2026, 3, 8, 9, 0), DueSettings{Location: la, At: ClockTime{Hour: 2, Minute: 30}}, utc(2026, 3, 8, 10, 0)},
		{"apia skipped day", utc(2011, 12, 29, 19, 0), DueSettings{Location: zone(t, "Pacific/Apia"), At: ClockTime{Hour: 8}}, utc(2011, 12, 30, 10, 0)},
	}
	for _, tt := range tests {
		if got := NextDueWake(tt.now, tt.s); !got.Equal(tt.want) {
			t.Errorf("%s: NextDueWake(%v) = %v, want %v", tt.name, tt.now, got.UTC(), tt.want)
		}
	}
}
