package rules

import (
	"testing"
	"time"

	// The zone tests name IANA zones; the embedded database makes them
	// pass on hosts without zoneinfo.
	_ "time/tzdata"
)

// zone loads an IANA zone and fails the test when it cannot.
func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// utc is a UTC instant to the minute, for expected times.
func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func TestParseClock(t *testing.T) {
	t.Parallel()
	good := []struct {
		in   string
		want ClockTime
	}{
		{"00:00", ClockTime{}},
		{"07:00", ClockTime{Hour: 7}},
		{"08:05", ClockTime{Hour: 8, Minute: 5}},
		{"23:59", ClockTime{Hour: 23, Minute: 59}},
	}
	for _, tt := range good {
		got, err := ParseClock(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseClock(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
		if got.String() != tt.in {
			t.Errorf("ParseClock(%q).String() = %q", tt.in, got.String())
		}
	}
	for _, in := range []string{"", "7:00", "07:0", "24:00", "23:60", "-1:00", "+7:00", "07:00:00", "07-00", "ab:cd", " 7:00", "07:+5"} {
		_, err := ParseClock(in)
		wantInvalid(t, "ParseClock("+in+")", err, "the time is not HH:MM")
	}
}

func TestParseSchedule(t *testing.T) {
	t.Parallel()
	good := []struct {
		in   string
		want Schedule
		str  string
	}{
		{"", Schedule{}, ""},
		{"every:15m", Schedule{Kind: ScheduleEvery, Every: 15 * time.Minute}, "every:15m"},
		{"every:6h", Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}, "every:6h"},
		{"every:1h30m", Schedule{Kind: ScheduleEvery, Every: 90 * time.Minute}, "every:1h30m"},
		{"every:90m", Schedule{Kind: ScheduleEvery, Every: 90 * time.Minute}, "every:1h30m"},
		{"every:24h", Schedule{Kind: ScheduleEvery, Every: 24 * time.Hour}, "every:1d"},
		{"every:7d", Schedule{Kind: ScheduleEvery, Every: 7 * 24 * time.Hour}, "every:7d"},
		{"every:30d", Schedule{Kind: ScheduleEvery, Every: 30 * 24 * time.Hour}, "every:30d"},
		{"every:720h", Schedule{Kind: ScheduleEvery, Every: 30 * 24 * time.Hour}, "every:30d"},
		{"every:15m30s", Schedule{Kind: ScheduleEvery, Every: 15*time.Minute + 30*time.Second}, "every:15m30s"},
		{"at:07:00", Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}, "at:07:00"},
		{"at:00:00", Schedule{Kind: ScheduleAt}, "at:00:00"},
		{"at:23:59", Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 23, Minute: 59}}, "at:23:59"},
	}
	for _, tt := range good {
		got, err := ParseSchedule(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseSchedule(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
			continue
		}
		if s := got.String(); s != tt.str {
			t.Errorf("ParseSchedule(%q).String() = %q, want %q", tt.in, s, tt.str)
		}
		if back, err := ParseSchedule(got.String()); err != nil || back != got {
			t.Errorf("round trip of %q = %+v, %v", tt.in, back, err)
		}
	}
	bad := []struct {
		in, msg string
	}{
		{"every:14m59s", "the every: duration is shorter than 15 minutes"},
		{"every:0d", "the every: duration is shorter than 15 minutes"},
		{"every:-1h", "the every: duration is shorter than 15 minutes"},
		{"every:30d1s", "the every: duration is not valid"},
		{"every:720h1s", "the every: duration is longer than 30 days"},
		{"every:31d", "the every: duration is longer than 30 days"},
		{"every:", "the every: duration is not valid"},
		{"every:" + ruleMarker, "the every: duration is not valid"},
		{"every:7D", "the every: duration is not valid"},
		{"at:24:00", "the at: time is not HH:MM"},
		{"at:7:00", "the at: time is not HH:MM"},
		{"at:", "the at: time is not HH:MM"},
		{"at:" + ruleMarker, "the at: time is not HH:MM"},
		{"daily", "the schedule is not every:<duration> or at:HH:MM"},
		{"Every:6h", "the schedule is not every:<duration> or at:HH:MM"},
		{" every:6h", "the schedule is not every:<duration> or at:HH:MM"},
		{ruleMarker, "the schedule is not every:<duration> or at:HH:MM"},
	}
	for _, tt := range bad {
		_, err := ParseSchedule(tt.in)
		wantInvalid(t, "ParseSchedule("+tt.in+")", err, tt.msg)
	}
}

func TestScheduleStringUnknownKind(t *testing.T) {
	t.Parallel()
	if s := (Schedule{Kind: "cron"}).String(); s != "" {
		t.Errorf("String of an unknown kind = %q, want empty", s)
	}
}

func TestScheduleNextEvery(t *testing.T) {
	t.Parallel()
	after := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)
	s := Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	if got, want := s.Next(after, nil), after.Add(6*time.Hour); !got.Equal(want) {
		t.Errorf("every:6h Next = %v, want %v", got, want)
	}
	// every counts elapsed time, so a DST change does not move it.
	la := zone(t, "America/Los_Angeles")
	beforeGap := time.Date(2026, 3, 8, 0, 0, 0, 0, la)
	day := Schedule{Kind: ScheduleEvery, Every: 24 * time.Hour}
	if got, want := day.Next(beforeGap, la), beforeGap.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("every:1d across spring forward = %v, want %v", got, want)
	}
}

func TestScheduleNextAt(t *testing.T) {
	t.Parallel()
	la := zone(t, "America/Los_Angeles")
	london := zone(t, "Europe/London")
	at := func(h, m int) Schedule { return Schedule{Kind: ScheduleAt, At: ClockTime{Hour: h, Minute: m}} }
	tests := []struct {
		name  string
		s     Schedule
		after time.Time
		loc   *time.Location
		want  time.Time
	}{
		{"utc later today", at(7, 0), utc(2026, 11, 7, 6, 59), time.UTC, utc(2026, 11, 7, 7, 0)},
		{"utc exactly at the time runs tomorrow", at(7, 0), utc(2026, 11, 7, 7, 0), time.UTC, utc(2026, 11, 8, 7, 0)},
		{"utc a nanosecond before", at(7, 0), utc(2026, 11, 7, 7, 0).Add(-time.Nanosecond), time.UTC, utc(2026, 11, 7, 7, 0)},
		{"utc past the time", at(7, 0), utc(2026, 11, 7, 7, 1), time.UTC, utc(2026, 11, 8, 7, 0)},
		{"utc over the year end", at(0, 0), utc(2026, 12, 31, 23, 59), time.UTC, utc(2027, 1, 1, 0, 0)},
		{"la standard time", at(7, 0), utc(2026, 11, 7, 10, 0), la, utc(2026, 11, 7, 15, 0)},
		{"la daylight time", at(7, 0), utc(2026, 7, 1, 0, 0), la, utc(2026, 7, 1, 14, 0)},
		{"la next day crosses spring forward", at(7, 0), utc(2026, 3, 7, 15, 0), la, utc(2026, 3, 8, 14, 0)},
		{"la 02:30 does not exist on spring forward", at(2, 30), utc(2026, 3, 8, 8, 0), la, utc(2026, 3, 8, 10, 0)},
		{"la 02:30 the day after spring forward", at(2, 30), utc(2026, 3, 8, 10, 0), la, utc(2026, 3, 9, 9, 30)},
		{"la 01:30 twice on fall back runs at the first", at(1, 30), utc(2026, 11, 1, 7, 0), la, utc(2026, 11, 1, 8, 30)},
		{"la 01:30 after the first runs the next day", at(1, 30), utc(2026, 11, 1, 8, 30), la, utc(2026, 11, 2, 9, 30)},
		{"la 01:30 between the two runs the next day", at(1, 30), utc(2026, 11, 1, 9, 0), la, utc(2026, 11, 2, 9, 30)},
		{"london gmt", at(8, 0), utc(2026, 1, 15, 0, 0), london, utc(2026, 1, 15, 8, 0)},
		{"london bst", at(8, 0), utc(2026, 6, 15, 0, 0), london, utc(2026, 6, 15, 7, 0)},
		{"london 01:30 does not exist on spring forward", at(1, 30), utc(2026, 3, 29, 0, 0), london, utc(2026, 3, 29, 1, 0)},
		{"london 01:30 twice on fall back runs at the first", at(1, 30), utc(2026, 10, 24, 12, 0), london, utc(2026, 10, 25, 0, 30)},
		{"london the local date decides today", at(0, 30), utc(2026, 6, 15, 23, 15), london, utc(2026, 6, 15, 23, 30)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.s.Next(tt.after, tt.loc)
			if !got.Equal(tt.want) {
				t.Errorf("Next(%v) = %v (%v UTC), want %v", tt.after, got, got.UTC(), tt.want)
			}
			if got.Location() != tt.loc {
				t.Errorf("Next(%v) is in %v, want %v", tt.after, got.Location(), tt.loc)
			}
		})
	}
}

func TestScheduleNextNone(t *testing.T) {
	t.Parallel()
	after := utc(2026, 11, 7, 9, 0)
	for _, s := range []Schedule{{}, {Kind: "cron"}, {Kind: ScheduleEvery}, {Kind: ScheduleAt, At: ClockTime{Hour: 24}}} {
		if got := s.Next(after, time.UTC); !got.IsZero() {
			t.Errorf("%+v.Next = %v, want zero", s, got)
		}
	}
}

// A nil location is the host's zone, as the [rules] timezone default is.
func TestScheduleNextNilLocation(t *testing.T) {
	t.Parallel()
	after := utc(2026, 11, 7, 9, 0)
	s := Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}
	if got, want := s.Next(after, nil), s.Next(after, time.Local); !got.Equal(want) {
		t.Errorf("Next with nil location = %v, want %v", got, want)
	}
}
