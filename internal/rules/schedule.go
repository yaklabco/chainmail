package rules

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
)

// Bounds of an every: schedule.
const (
	MinEvery = 15 * time.Minute
	MaxEvery = 30 * 24 * time.Hour
)

// day is one calendar day of 24 hours, for whole-day every: schedules.
const day = 24 * time.Hour

// scheduleHint is the hint of every schedule refusal.
const scheduleHint = "write every:<duration> from 15m to 30d, such as every:6h or every:7d, or at:HH:MM such as at:07:00"

// Refusals of a schedule or a clock time. The text is fixed.
var (
	errScheduleForm = &InvalidError{Msg: "the schedule is not every:<duration> or at:HH:MM", Hint: scheduleHint}
	errEveryForm    = &InvalidError{Msg: "the every: duration is not valid", Hint: scheduleHint}
	errEveryShort   = &InvalidError{Msg: "the every: duration is shorter than 15 minutes", Hint: scheduleHint}
	errEveryLong    = &InvalidError{Msg: "the every: duration is longer than 30 days", Hint: scheduleHint}
	errAtForm       = &InvalidError{Msg: "the at: time is not HH:MM", Hint: scheduleHint}
	errClockForm    = &InvalidError{Msg: "the time is not HH:MM", Hint: "write a 24-hour time from 00:00 to 23:59, such as 08:00"}
)

// ScheduleKind says how a rule is scheduled.
type ScheduleKind string

// The schedule kinds. ScheduleNone is a rule with no schedule.
const (
	ScheduleNone  ScheduleKind = ""
	ScheduleEvery ScheduleKind = "every"
	ScheduleAt    ScheduleKind = "at"
)

// ClockTime is a wall-clock time of day, to the minute.
type ClockTime struct{ Hour, Minute int }

// ParseClock parses "HH:MM", 00:00 to 23:59, with two digits each. It
// checks the round trip, so 7:00, +7:00 and 07:00:00 are refused.
func ParseClock(s string) (ClockTime, error) {
	if len(s) != 5 || s[2] != ':' {
		return ClockTime{}, errClockForm
	}
	h, herr := strconv.Atoi(s[:2])
	m, merr := strconv.Atoi(s[3:])
	c := ClockTime{Hour: h, Minute: m}
	if herr != nil || merr != nil || !c.valid() || c.String() != s {
		return ClockTime{}, errClockForm
	}
	return c, nil
}

// String renders c as HH:MM.
func (c ClockTime) String() string {
	return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute)
}

// valid reports whether c is 00:00 to 23:59.
func (c ClockTime) valid() bool {
	return c.Hour >= 0 && c.Hour < 24 && c.Minute >= 0 && c.Minute < 60
}

// Schedule is when a rule runs on its own. The zero Schedule is none.
type Schedule struct {
	Kind ScheduleKind
	// Every is the gap between runs, for ScheduleEvery: MinEvery to
	// MaxEvery.
	Every time.Duration
	// At is the time of day, for ScheduleAt.
	At ClockTime
}

// ParseSchedule parses "", every:<d> and at:HH:MM. The duration takes
// config.ParseCacheDuration's syntax: a whole number of days such as 7d, or
// a Go duration such as 6h. Errors are fixed text.
func ParseSchedule(s string) (Schedule, error) {
	if s == "" {
		return Schedule{}, nil
	}
	if rest, ok := strings.CutPrefix(s, "every:"); ok {
		d, err := config.ParseCacheDuration(rest)
		if err != nil {
			// The error quotes the text, which is rule text.
			return Schedule{}, errEveryForm
		}
		if err := checkEvery(d); err != nil {
			return Schedule{}, err
		}
		return Schedule{Kind: ScheduleEvery, Every: d}, nil
	}
	if rest, ok := strings.CutPrefix(s, "at:"); ok {
		c, err := ParseClock(rest)
		if err != nil {
			return Schedule{}, errAtForm
		}
		return Schedule{Kind: ScheduleAt, At: c}, nil
	}
	return Schedule{}, errScheduleForm
}

// checkEvery checks d is MinEvery to MaxEvery.
func checkEvery(d time.Duration) error {
	switch {
	case d < MinEvery:
		return errEveryShort
	case d > MaxEvery:
		return errEveryLong
	}
	return nil
}

// String is the inverse of ParseSchedule. An every: duration that is a
// whole number of days is written in days, such as every:7d; any other is
// written as a Go duration without zero units, such as every:6h or
// every:1h30m. An unknown kind renders as "".
func (s Schedule) String() string {
	switch s.Kind {
	case ScheduleEvery:
		return "every:" + formatEvery(s.Every)
	case ScheduleAt:
		return "at:" + s.At.String()
	default:
		return ""
	}
}

// formatEvery writes d in days when it is whole days, else as
// time.Duration.String with trailing zero minutes and seconds dropped.
func formatEvery(d time.Duration) string {
	if d > 0 && d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}
	out := d.String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}

// Next is the first run strictly after after: after+Every for every; the
// next instant at At on the wall clock of loc for at. A wall time that does
// not exist (DST gap) runs at the first instant after the gap; a wall time
// that occurs twice runs once, at the first. Zero Schedule returns zero. A
// nil loc means time.Local.
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	switch s.Kind {
	case ScheduleEvery:
		if s.Every <= 0 {
			return time.Time{}
		}
		return after.Add(s.Every)
	case ScheduleAt:
		if !s.At.valid() {
			return time.Time{}
		}
		if loc == nil {
			loc = time.Local
		}
		y, m, d := after.In(loc).Date()
		// Today's time may have passed; tomorrow's never has. The third day
		// covers a zone whose clocks jump past midnight.
		for i := range 3 {
			if t := wallInstant(y, m, d+i, s.At, loc); t.After(after) {
				return t
			}
		}
		return time.Time{}
	default:
		return time.Time{}
	}
}

// wallInstant is the first instant on the date (y, m, d), normalized, at
// which the wall clock of loc reads c. When c falls in a gap on that date,
// it is the first instant after the gap.
func wallInstant(y int, m time.Month, d int, c ClockTime, loc *time.Location) time.Time {
	y, m, d = time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Date()
	guess := time.Date(y, m, d, c.Hour, c.Minute, 0, 0, loc)
	// wall is the wall time read as if it were UTC. Subtracting a zone's
	// offset from it gives the instant that wall time is in that zone.
	wall := time.Date(y, m, d, c.Hour, c.Minute, 0, 0, time.UTC)
	var best time.Time
	for _, probe := range []time.Time{guess.Add(-12 * time.Hour), guess, guess.Add(12 * time.Hour)} {
		_, offset := probe.Zone()
		t := wall.Add(-time.Duration(offset) * time.Second).In(loc)
		ty, tm, td := t.Date()
		if ty == y && tm == m && td == d && t.Hour() == c.Hour && t.Minute() == c.Minute && t.Second() == 0 &&
			(best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	if !best.IsZero() {
		return best
	}
	// No zone around the date reads c: the clocks jumped over it. The
	// answer is the transition whose jump spans that wall time.
	if t, ok := gapEnd(wall, guess, loc); ok {
		return t
	}
	return guess
}

// gapWindow is how far either side of a skipped wall time gapEnd looks
// for the transition that skipped it. A zone has jumped a whole day
// (Pacific/Apia, 2011-12-30), so the window is wider than that.
const gapWindow = 48 * time.Hour

// gapEnd returns the transition of loc near guess whose jump skips the
// wall time wall (a wall clock reading expressed in UTC): the wall clock
// reads before wall an instant before it and after wall at it. It walks
// the zone periods from gapWindow before guess to gapWindow after.
func gapEnd(wall, guess time.Time, loc *time.Location) (time.Time, bool) {
	limit := guess.Add(gapWindow)
	for t := guess.Add(-gapWindow); t.Before(limit); {
		_, end := t.In(loc).ZoneBounds()
		if end.IsZero() || !end.Before(limit) {
			return time.Time{}, false
		}
		if wallClock(end.Add(-time.Nanosecond), loc).Before(wall) && wallClock(end, loc).After(wall) {
			return end.In(loc), true
		}
		t = end
	}
	return time.Time{}, false
}

// wallClock is the wall clock reading of t in loc, expressed in UTC so
// readings from different offsets compare directly.
func wallClock(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}
