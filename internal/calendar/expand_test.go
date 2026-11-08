package calendar

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func starts(insts []Instance) []time.Time {
	out := make([]time.Time, len(insts))
	for i, in := range insts {
		out[i] = in.Start
	}
	return out
}

func sameTimes(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d instants %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			t.Fatalf("instant %d = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
}

func mustExpand(t *testing.T, series []Event, from, to time.Time, limit int) ([]Instance, bool) {
	t.Helper()
	insts, truncated, err := Expand(series, from, to, limit)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	return insts, truncated
}

func master(rrule string, start time.Time, d time.Duration) Event {
	return Event{UID: "s", ProviderID: "p", RRule: rrule, Start: start, End: start.Add(d)}
}

func TestExpandRules(t *testing.T) {
	start := utc(2026, 11, 2, 9, 0, 0) // a Monday
	from, to := utc(2026, 11, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0)
	cases := []struct {
		name string
		ev   Event
		from time.Time
		to   time.Time
		want []time.Time
	}{
		{
			name: "daily count",
			ev:   master("FREQ=DAILY;COUNT=3", start, time.Hour),
			want: []time.Time{start, start.AddDate(0, 0, 1), start.AddDate(0, 0, 2)},
		},
		{
			name: "daily until",
			ev:   master("FREQ=DAILY;UNTIL=20261104T090000Z", start, time.Hour),
			want: []time.Time{start, start.AddDate(0, 0, 1), start.AddDate(0, 0, 2)},
		},
		{
			name: "weekly byday",
			ev:   master("FREQ=WEEKLY;BYDAY=MO,WE;COUNT=4", start, time.Hour),
			want: []time.Time{start, utc(2026, 11, 4, 9, 0, 0), utc(2026, 11, 9, 9, 0, 0), utc(2026, 11, 11, 9, 0, 0)},
		},
		{
			name: "monthly",
			ev:   master("FREQ=MONTHLY;BYMONTHDAY=2", start, time.Hour),
			from: from, to: utc(2027, 2, 1, 0, 0, 0),
			want: []time.Time{start, utc(2026, 12, 2, 9, 0, 0), utc(2027, 1, 2, 9, 0, 0)},
		},
		{
			name: "range cuts the series",
			ev:   master("FREQ=DAILY", start, time.Hour),
			from: utc(2026, 11, 20, 0, 0, 0), to: utc(2026, 11, 22, 0, 0, 0),
			want: []time.Time{utc(2026, 11, 20, 9, 0, 0), utc(2026, 11, 21, 9, 0, 0)},
		},
		{
			name: "an instance overlapping the start of the range is kept",
			ev:   master("FREQ=DAILY;COUNT=5", start, 3*time.Hour),
			from: utc(2026, 11, 3, 11, 0, 0), to: utc(2026, 11, 4, 0, 0, 0),
			want: []time.Time{utc(2026, 11, 3, 9, 0, 0)},
		},
		{
			name: "exdate",
			ev: func() Event {
				e := master("FREQ=DAILY;COUNT=4", start, time.Hour)
				e.ExDates = []time.Time{start.AddDate(0, 0, 1)}
				return e
			}(),
			want: []time.Time{start, start.AddDate(0, 0, 2), start.AddDate(0, 0, 3)},
		},
		{
			name: "rdate",
			ev: func() Event {
				e := master("FREQ=WEEKLY;COUNT=2", start, time.Hour)
				e.RDates = []time.Time{utc(2026, 11, 5, 14, 0, 0)}
				return e
			}(),
			want: []time.Time{start, utc(2026, 11, 5, 14, 0, 0), utc(2026, 11, 9, 9, 0, 0)},
		},
		{
			name: "rdate without a rule",
			ev:   Event{UID: "s", Start: start, End: start.Add(time.Hour), RDates: []time.Time{utc(2026, 11, 5, 14, 0, 0)}},
			want: []time.Time{start, utc(2026, 11, 5, 14, 0, 0)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, e := from, to
			if !tc.from.IsZero() {
				f, e = tc.from, tc.to
			}
			insts, truncated := mustExpand(t, []Event{tc.ev}, f, e, MaxInstances)
			if truncated {
				t.Fatal("truncated without reaching the cap")
			}
			sameTimes(t, starts(insts), tc.want)
			for _, in := range insts {
				if !in.RecurrenceID.Equal(in.Start) {
					t.Fatalf("instance at %v has recurrence ID %v", in.Start, in.RecurrenceID)
				}
				if in.End.Sub(in.Start) != tc.ev.End.Sub(tc.ev.Start) {
					t.Fatalf("instance at %v lasts %v", in.Start, in.End.Sub(in.Start))
				}
				if in.Start.Location() != time.UTC {
					t.Fatalf("instance start %v is not UTC", in.Start)
				}
			}
		})
	}
}

func TestExpandOverrides(t *testing.T) {
	_, events := mustParse(t, readFixture(t, "apple_series.ics"))
	insts, truncated := mustExpand(t, events, utc(2026, 11, 1, 0, 0, 0), utc(2027, 1, 1, 0, 0, 0), MaxInstances)
	if truncated {
		t.Fatal("truncated")
	}
	sameTimes(t, starts(insts), []time.Time{
		utc(2026, 11, 2, 10, 0, 0),
		utc(2026, 11, 4, 15, 0, 0),  // RDATE
		utc(2026, 11, 30, 11, 0, 0), // moved override; 11-09, 11-16, 11-23 are EXDATEs
		utc(2026, 12, 14, 10, 0, 0), // 12-07 is a CANCELLED override
		utc(2026, 12, 21, 10, 0, 0),
		utc(2026, 12, 28, 10, 0, 0),
	})
	moved := insts[2]
	if moved.Event.Summary != "Standup (moved)" || !moved.RecurrenceID.Equal(utc(2026, 11, 30, 10, 0, 0)) || !moved.End.Equal(utc(2026, 11, 30, 11, 30, 0)) {
		t.Fatalf("moved instance: %+v", moved)
	}
	if insts[0].Event.Summary != "Standup" || !insts[0].Event.IsMaster() {
		t.Fatal("a rule instance carries the master")
	}

	// An override moved into the range from outside it appears; the slot it
	// left does not.
	insts, _ = mustExpand(t, events, utc(2026, 11, 30, 10, 45, 0), utc(2026, 11, 30, 12, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{utc(2026, 11, 30, 11, 0, 0)})
	insts, _ = mustExpand(t, events, utc(2026, 11, 30, 9, 0, 0), utc(2026, 11, 30, 10, 45, 0), MaxInstances)
	if len(insts) != 0 {
		t.Fatalf("the slot a moved instance left still shows: %v", starts(insts))
	}

	// Overrides without their master (an invite to one instance) still expand.
	insts, _ = mustExpand(t, events[1:2], utc(2026, 11, 1, 0, 0, 0), utc(2027, 1, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{utc(2026, 11, 30, 11, 0, 0)})
}

func TestExpandKeepsLocalTimeAcrossDST(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// 09:00 PDT on Monday 2026-10-26; DST ends on 2026-11-01.
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, la).UTC()
	ev := master("FREQ=WEEKLY;COUNT=3", start, 30*time.Minute)
	ev.TZID = "America/Los_Angeles"
	insts, _ := mustExpand(t, []Event{ev}, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{
		utc(2026, 10, 26, 16, 0, 0), // PDT, UTC-7
		utc(2026, 11, 2, 17, 0, 0),  // PST, UTC-8
		utc(2026, 11, 9, 17, 0, 0),
	})
	for _, in := range insts {
		if h := in.Start.In(la).Hour(); h != 9 {
			t.Fatalf("instance %v is at %02d:00 local, want 09:00", in.Start, h)
		}
	}

	// The spring change too, with an EXDATE in UTC on the far side.
	start = time.Date(2027, 3, 8, 9, 0, 0, 0, la).UTC() // still PST; DST starts on 2027-03-14
	ev = master("FREQ=WEEKLY;COUNT=3", start.AddDate(0, 0, -7), 30*time.Minute)
	ev.TZID = "America/Los_Angeles"
	ev.ExDates = []time.Time{utc(2027, 3, 8, 17, 0, 0)}
	insts, _ = mustExpand(t, []Event{ev}, utc(2027, 2, 1, 0, 0, 0), utc(2027, 4, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{utc(2027, 3, 1, 17, 0, 0), utc(2027, 3, 15, 16, 0, 0)})
}

func TestExpandCap(t *testing.T) {
	start := utc(2026, 11, 2, 9, 0, 0)
	ev := master("FREQ=DAILY", start, time.Hour)
	insts, truncated := mustExpand(t, []Event{ev}, start, start.AddDate(0, 0, 10), 5)
	if !truncated || len(insts) != 5 {
		t.Fatalf("got %d instances, truncated %v; want 5, true", len(insts), truncated)
	}
	sameTimes(t, starts(insts)[:1], []time.Time{start})
	_, truncated = mustExpand(t, []Event{ev}, start, start.AddDate(0, 0, 5), 5)
	if truncated {
		t.Fatal("exactly max instances is not truncated")
	}
	insts, truncated = mustExpand(t, []Event{ev}, start, start.AddDate(4, 0, 0), 0)
	if !truncated || len(insts) != MaxInstances {
		t.Fatalf("max 0: got %d, truncated %v; want MaxInstances, true", len(insts), truncated)
	}
}

func TestExpandUnsafeRecurrence(t *testing.T) {
	start := utc(2026, 11, 2, 9, 0, 0)
	for _, rule := range []string{"FREQ=SECONDLY", "FREQ=MINUTELY;INTERVAL=5", "INTERVAL=2;FREQ=SECONDLY"} {
		_, _, err := Expand([]Event{master(rule, start, time.Minute)}, start, start.Add(time.Hour), MaxInstances)
		if !errors.Is(err, ErrUnsafeRecurrence) {
			t.Fatalf("%s: err = %v, want ErrUnsafeRecurrence", rule, err)
		}
	}
	_, _, err := Expand([]Event{master("FREQ=SOMETIMES", start, time.Minute)}, start, start.Add(time.Hour), MaxInstances)
	if !errors.Is(err, ErrInvalidICS) {
		t.Fatalf("bad rule: err = %v, want ErrInvalidICS", err)
	}

	// rrule-go iterates from DTSTART, so a rule with more than a million
	// periods before the end of the range is not expanded either.
	ancient := utc(1800, 1, 1, 9, 0, 0)
	_, _, err = Expand([]Event{master("FREQ=HOURLY", ancient, time.Minute)}, start, start.Add(time.Hour), MaxInstances)
	if !errors.Is(err, ErrUnsafeRecurrence) {
		t.Fatalf("hourly from 1800: err = %v, want ErrUnsafeRecurrence", err)
	}
	insts, _ := mustExpand(t, []Event{master("FREQ=DAILY", utc(1900, 1, 1, 9, 0, 0), time.Hour)}, start, start.AddDate(0, 0, 2), MaxInstances)
	if len(insts) != 2 {
		t.Fatalf("daily from 1900: %d instances, want 2", len(insts))
	}
	insts, _ = mustExpand(t, []Event{master("FREQ=YEARLY", utc(1, 11, 2, 9, 0, 0), time.Hour)}, start, start.AddDate(0, 0, 1), MaxInstances)
	if len(insts) != 1 {
		t.Fatalf("yearly from year 1: %d instances, want 1", len(insts))
	}
}

func TestExpandSingleEvents(t *testing.T) {
	ev := Event{UID: "one", Start: utc(2026, 11, 10, 9, 0, 0), End: utc(2026, 11, 10, 10, 0, 0)}
	insts, _ := mustExpand(t, []Event{ev}, utc(2026, 11, 10, 0, 0, 0), utc(2026, 11, 11, 0, 0, 0), MaxInstances)
	if len(insts) != 1 || !insts[0].RecurrenceID.IsZero() || !insts[0].Start.Equal(ev.Start) || !insts[0].End.Equal(ev.End) {
		t.Fatalf("single inside: %+v", insts)
	}
	for _, r := range [][2]time.Time{
		{utc(2026, 11, 10, 10, 0, 0), utc(2026, 11, 11, 0, 0, 0)}, // starts when the event ends
		{utc(2026, 11, 9, 0, 0, 0), utc(2026, 11, 10, 9, 0, 0)},   // ends when the event starts
		{utc(2026, 12, 1, 0, 0, 0), utc(2026, 12, 2, 0, 0, 0)},
	} {
		if insts, _ := mustExpand(t, []Event{ev}, r[0], r[1], MaxInstances); len(insts) != 0 {
			t.Fatalf("single outside [%v, %v): %+v", r[0], r[1], insts)
		}
	}

	// A zero-length event belongs to the range holding its instant.
	pt := Event{UID: "pt", Start: utc(2026, 11, 10, 9, 0, 0), End: utc(2026, 11, 10, 9, 0, 0)}
	if insts, _ := mustExpand(t, []Event{pt}, utc(2026, 11, 10, 9, 0, 0), utc(2026, 11, 10, 9, 1, 0), MaxInstances); len(insts) != 1 {
		t.Fatal("zero-length event at the range start is missing")
	}
	if insts, _ := mustExpand(t, []Event{pt}, utc(2026, 11, 10, 8, 0, 0), utc(2026, 11, 10, 9, 0, 0), MaxInstances); len(insts) != 0 {
		t.Fatal("zero-length event at the range end is included")
	}
}

func TestExpandAllDay(t *testing.T) {
	ev := Event{UID: "hol", AllDay: true, Start: utc(2026, 12, 24, 0, 0, 0), End: utc(2026, 12, 26, 0, 0, 0), RRule: "FREQ=YEARLY", ExDates: []time.Time{utc(2027, 12, 24, 0, 0, 0)}}
	insts, _ := mustExpand(t, []Event{ev}, utc(2026, 12, 25, 12, 0, 0), utc(2029, 1, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{utc(2026, 12, 24, 0, 0, 0), utc(2028, 12, 24, 0, 0, 0)})
	if !insts[1].End.Equal(utc(2028, 12, 26, 0, 0, 0)) {
		t.Fatalf("all-day instance end %v", insts[1].End)
	}

	single := Event{UID: "day", AllDay: true, Start: utc(2026, 11, 10, 0, 0, 0), End: utc(2026, 11, 11, 0, 0, 0)}
	if insts, _ := mustExpand(t, []Event{single}, utc(2026, 11, 10, 23, 0, 0), utc(2026, 11, 12, 0, 0, 0), MaxInstances); len(insts) != 1 {
		t.Fatal("all-day single overlapping the range is missing")
	}
}

// expandWithin runs Expand and fails the test if it has not returned within
// a generous bound, so a rule that would loop forever fails instead of
// hanging the run.
func expandWithin(t *testing.T, series []Event, from, to time.Time) ([]Instance, error) {
	t.Helper()
	type result struct {
		insts []Instance
		err   error
	}
	done := make(chan result, 1)
	go func() {
		insts, _, err := Expand(series, from, to, MaxInstances)
		done <- result{insts, err}
	}()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.insts, r.err
	case <-timer.C:
		t.Fatalf("Expand of %q did not return", series[0].RRule)
		return nil, nil
	}
}

func repeatList(n int, v string) string {
	return strings.TrimSuffix(strings.Repeat(v+",", n), ",")
}

// rrule-go allocates the product of the BYHOUR, BYMINUTE and BYSECOND lists
// without removing repeats, so repeated values are merged before it sees them.
func TestExpandDeduplicatesRuleLists(t *testing.T) {
	const n = 1000
	start := utc(2026, 11, 2, 0, 0, 0)
	rule := "FREQ=DAILY;BYHOUR=" + repeatList(n, "0") + ";BYMINUTE=" + repeatList(n, "0") + ";BYSECOND=" + repeatList(n, "0")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	insts, err := expandWithin(t, []Event{master(rule, start, time.Hour)}, start, start.AddDate(0, 0, 3))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	sameTimes(t, starts(insts), []time.Time{start, start.AddDate(0, 0, 1), start.AddDate(0, 0, 2)})
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("Expand allocated %d bytes for a rule with repeated values", grew)
	}
}

// A rule whose candidate instances (the product of its lists, times the
// periods to iterate) are too many is not expanded, even with every value
// distinct and in range.
func TestExpandBoundsCandidateProduct(t *testing.T) {
	from := utc(2026, 12, 1, 0, 0, 0)
	full := "FREQ=DAILY;BYHOUR=" + seq(0, 23) + ";BYMINUTE=" + seq(0, 59) + ";BYSECOND=" + seq(0, 59)
	for _, tc := range []struct {
		name, rule string
		start      time.Time
	}{
		{"every second of every day", full, from.AddDate(0, 0, -30)},
		// No February 30 exists, so rrule-go would search to year 9999,
		// building the minute list for every day on the way.
		{"never matches", "FREQ=HOURLY;BYMONTH=2;BYMONTHDAY=30;BYMINUTE=" + seq(0, 59), from},
	} {
		_, err := expandWithin(t, []Event{master(tc.rule, tc.start, time.Second)}, from, from.AddDate(0, 0, 1))
		if !errors.Is(err, ErrUnsafeRecurrence) {
			t.Errorf("%s: err = %v, want ErrUnsafeRecurrence", tc.name, err)
		}
	}
}

func seq(lo, hi int) string {
	var b strings.Builder
	for i := lo; i <= hi; i++ {
		if i > lo {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i))
	}
	return b.String()
}

// Values outside the RFC 5545 ranges are refused before rrule-go sees
// them; some of them index past its tables and panic.
func TestExpandRejectsOutOfRangeRuleValues(t *testing.T) {
	start := utc(2026, 1, 5, 9, 0, 0)
	for _, rule := range []string{
		"FREQ=DAILY;BYHOUR=24",
		"FREQ=DAILY;BYMINUTE=60",
		"FREQ=MONTHLY;BYMONTHDAY=0",
		"FREQ=MONTHLY;BYMONTHDAY=32",
		"FREQ=YEARLY;BYYEARDAY=367",
		"FREQ=YEARLY;BYWEEKNO=54",
		"FREQ=YEARLY;BYMONTH=13",
		"FREQ=MONTHLY;BYSETPOS=0;BYDAY=MO",
		"FREQ=MONTHLY;BYDAY=-53MO",
		"FREQ=MONTHLY;BYDAY=53MO",
		"FREQ=MONTHLY;BYDAY=6MO",
		"FREQ=YEARLY;BYMONTH=2;BYDAY=-6MO",
		"FREQ=YEARLY;BYDAY=54MO",
		"FREQ=YEARLY;BYEASTER=-200",
		"FREQ=YEARLY;BYEASTER=1",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic %v", rule, r)
				}
			}()
			_, _, err := Expand([]Event{master(rule, start, time.Hour)}, start, start.AddDate(1, 0, 0), MaxInstances)
			if !errors.Is(err, ErrInvalidICS) {
				t.Errorf("%s: err = %v, want ErrInvalidICS", rule, err)
			}
		}()
	}
	// The largest values in range still expand.
	for _, rule := range []string{"FREQ=MONTHLY;BYDAY=5MO,-5MO", "FREQ=YEARLY;BYDAY=53MO,-53MO", "FREQ=YEARLY;BYMONTH=2;BYDAY=5MO"} {
		if _, _, err := Expand([]Event{master(rule, start, time.Hour)}, start, start.AddDate(1, 0, 0), MaxInstances); err != nil {
			t.Errorf("%s: %v", rule, err)
		}
	}
}

func TestExpandRulesThatRarelyOrNeverMatch(t *testing.T) {
	start := utc(2026, 1, 5, 0, 0, 0)
	// An HOURLY rule stepping 24 hours from midnight never reaches 01:00;
	// rrule-go would loop forever looking for it.
	insts, err := expandWithin(t, []Event{master("FREQ=HOURLY;INTERVAL=24;BYHOUR=1", start, time.Hour)}, start, start.AddDate(0, 0, 3))
	if err != nil {
		t.Fatalf("unreachable hour: %v", err)
	}
	sameTimes(t, starts(insts), []time.Time{start}) // DTSTART is always an instance
	insts, err = expandWithin(t, []Event{master("FREQ=HOURLY;INTERVAL=12;BYHOUR=12", start, time.Hour)}, start, start.AddDate(0, 0, 2))
	if err != nil {
		t.Fatalf("reachable hour: %v", err)
	}
	sameTimes(t, starts(insts), []time.Time{start, start.Add(12 * time.Hour), start.Add(36 * time.Hour)})

	// No February 30 exists: only DTSTART.
	insts, err = expandWithin(t, []Event{master("FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30", start, time.Hour)}, start, start.AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("never matches: %v", err)
	}
	sameTimes(t, starts(insts), []time.Time{start})

	// A fifth Monday in February comes once in 28 years.
	insts, err = expandWithin(t, []Event{master("FREQ=MONTHLY;BYMONTH=2;BYDAY=5MO", start, time.Hour)}, utc(2030, 1, 1, 0, 0, 0), utc(2050, 1, 1, 0, 0, 0))
	if err != nil {
		t.Fatalf("rare: %v", err)
	}
	sameTimes(t, starts(insts), []time.Time{utc(2044, 2, 29, 0, 0, 0)})

	// HOURLY with BYSETPOS can fail to match hour after hour, so it is not
	// expanded.
	if _, err := expandWithin(t, []Event{master("FREQ=HOURLY;BYSETPOS=2", start, time.Hour)}, start, start.AddDate(0, 0, 1)); !errors.Is(err, ErrUnsafeRecurrence) {
		t.Fatalf("hourly BYSETPOS: err = %v, want ErrUnsafeRecurrence", err)
	}
}

// INTERVAL is a positive integer, bounded so period arithmetic cannot
// overflow.
func TestExpandValidatesInterval(t *testing.T) {
	start := utc(2026, 11, 2, 9, 0, 0)
	for _, tc := range []struct {
		rule string
		want error
	}{
		{"FREQ=HOURLY;INTERVAL=2251799813685248", ErrUnsafeRecurrence},
		{"FREQ=DAILY;INTERVAL=10001", ErrUnsafeRecurrence},
		{"FREQ=DAILY;INTERVAL=0", ErrInvalidICS},
		{"FREQ=DAILY;INTERVAL=-1", ErrInvalidICS},
		{"FREQ=DAILY;COUNT=-1", ErrInvalidICS},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic %v", tc.rule, r)
				}
			}()
			_, _, err := Expand([]Event{master(tc.rule, start, time.Hour)}, start, start.AddDate(0, 0, 3), MaxInstances)
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.rule, err, tc.want)
			}
		}()
	}
	insts, _ := mustExpand(t, []Event{master("FREQ=DAILY;INTERVAL=10000", start, time.Hour)}, start, start.AddDate(0, 0, 3), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{start})
}

// A rule's first instance can come centuries after DTSTART: every hundred
// years from 2100, the first February 29 is in 2400.
func TestExpandFirstInstanceCenturiesAway(t *testing.T) {
	start := utc(2100, 1, 1, 9, 0, 0)
	insts, err := expandWithin(t, []Event{master("FREQ=YEARLY;INTERVAL=100;BYMONTH=2;BYMONTHDAY=29", start, time.Hour)}, utc(2390, 1, 1, 0, 0, 0), utc(2410, 1, 1, 0, 0, 0))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	sameTimes(t, starts(insts), []time.Time{utc(2400, 2, 29, 9, 0, 0)})
}

// dstZones parses an event body whose TZID-qualified times are written
// "TZ:", once in a zone known only from its VTIMEZONE (US Pacific rules)
// and once in the IANA zone America/Los_Angeles, which have the same
// rules.
func dstZones(t *testing.T, body string) map[string][]Event {
	t.Helper()
	iana := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:x\n" + strings.ReplaceAll(body, "TZ:", "America/Los_Angeles:") + "END:VCALENDAR\n"
	_, custom := mustParse(t, zoneCalendar("Office Time", pacificRules, body))
	_, tzdata := mustParse(t, []byte(iana))
	return map[string][]Event{"vtimezone": custom, "iana": tzdata}
}

// A UTC UNTIL is an instant, even when it falls in the hour the autumn
// change repeats: an instance in the first pass of that hour is before it.
// The end of the range is an instant in the same way.
func TestExpandUTCUntilInRepeatedHour(t *testing.T) {
	const ev = "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20261031T%s\nDURATION:PT30M\nRRULE:FREQ=DAILY%s\nEND:VEVENT\n"
	oct31, nov1 := utc(2026, 10, 31, 8, 30, 0), utc(2026, 11, 1, 8, 30, 0) // 01:30 PDT both days
	cases := []struct {
		name, start, until string
		from, to           time.Time
		want               []time.Time
	}{
		// 09:15Z is 01:15 in the second pass of the repeated hour, after
		// the first 01:30 (08:30Z).
		{"until in the second pass", "013000", ";UNTIL=20261101T091500Z", utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), []time.Time{oct31, nov1}},
		{"until before the first pass", "013000", ";UNTIL=20261101T082000Z", utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), []time.Time{oct31}},
		{"range ends in the second pass", "013000", "", utc(2026, 10, 1, 0, 0, 0), utc(2026, 11, 1, 9, 15, 0), []time.Time{oct31, nov1}},
		// 02:00 on 11-01 is PST, 10:00Z, after an UNTIL of 09:15Z although
		// its wall time is before 02:15, the wall time 09:15Z has in PDT.
		{"until before the hour after", "020000", ";UNTIL=20261101T091500Z", utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), []time.Time{utc(2026, 10, 31, 9, 0, 0)}},
	}
	for _, c := range cases {
		for zone, events := range dstZones(t, fmt.Sprintf(ev, c.start, c.until)) {
			t.Run(c.name+"/"+zone, func(t *testing.T) {
				insts, _ := mustExpand(t, events, c.from, c.to, MaxInstances)
				sameTimes(t, starts(insts), c.want)
			})
		}
	}
}

// An instance whose wall time is DTSTART's is DTSTART, even when DTSTART,
// as a calendar API can give it, is in the second pass of a repeated hour.
func TestExpandStartInSecondPass(t *testing.T) {
	start := utc(2026, 11, 1, 9, 30, 0) // 01:30 PST, the second 01:30
	ev := master("FREQ=DAILY;COUNT=2", start, 30*time.Minute)
	ev.TZID = "America/Los_Angeles"
	insts, _ := mustExpand(t, []Event{ev}, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{start, utc(2026, 11, 2, 9, 30, 0)})
}

// A rule instance at a local time the spring change skips is ignored and
// does not count toward COUNT (RFC 5545 3.3.10) when the rule recurs more
// than once a day. A rule recurring once a day at one time of day keeps
// its instance on the day of the change, read with the offset before the
// gap (RFC 5545 3.3.5), as DTSTART itself is.
func TestExpandSpringGap(t *testing.T) {
	const ev = "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:%s\nDURATION:PT30M\nRRULE:%s\nEND:VEVENT\n"
	cases := []struct {
		name, start, rule string
		want              []time.Time
	}{
		{
			// 00:30 PST, 01:30 PST, (02:30 does not exist), 03:30 PDT, 04:30, 05:30.
			name: "hourly", start: "20260308T003000", rule: "FREQ=HOURLY;COUNT=5",
			want: []time.Time{utc(2026, 3, 8, 8, 30, 0), utc(2026, 3, 8, 9, 30, 0), utc(2026, 3, 8, 10, 30, 0), utc(2026, 3, 8, 11, 30, 0), utc(2026, 3, 8, 12, 30, 0)},
		},
		{
			// 00:30 PST, (02:30 does not exist), 04:30 PDT, 06:30 PDT: not
			// 03:30 PDT, where 02:30 read before the gap would land.
			name: "every two hours", start: "20260308T003000", rule: "FREQ=HOURLY;INTERVAL=2;COUNT=3",
			want: []time.Time{utc(2026, 3, 8, 8, 30, 0), utc(2026, 3, 8, 11, 30, 0), utc(2026, 3, 8, 13, 30, 0)},
		},
		{
			name: "several times a day", start: "20260307T013000", rule: "FREQ=DAILY;BYHOUR=1,2,3;BYMINUTE=30;COUNT=6",
			want: []time.Time{
				utc(2026, 3, 7, 9, 30, 0), utc(2026, 3, 7, 10, 30, 0), utc(2026, 3, 7, 11, 30, 0), // PST
				utc(2026, 3, 8, 9, 30, 0), utc(2026, 3, 8, 10, 30, 0), // 01:30 PST, 03:30 PDT
				utc(2026, 3, 9, 8, 30, 0), // 01:30 PDT
			},
		},
		{
			// 02:30 on 03-08 is read as 03:30 PDT (10:30Z).
			name: "once a day", start: "20260307T023000", rule: "FREQ=DAILY;COUNT=3",
			want: []time.Time{utc(2026, 3, 7, 10, 30, 0), utc(2026, 3, 8, 10, 30, 0), utc(2026, 3, 9, 9, 30, 0)},
		},
		{
			// DTSTART in the gap is read with the offset before it.
			name: "start in the gap", start: "20260308T023000", rule: "FREQ=DAILY;COUNT=1",
			want: []time.Time{utc(2026, 3, 8, 10, 30, 0)},
		},
	}
	for _, c := range cases {
		for zone, events := range dstZones(t, fmt.Sprintf(ev, c.start, c.rule)) {
			t.Run(c.name+"/"+zone, func(t *testing.T) {
				insts, _ := mustExpand(t, events, utc(2026, 3, 1, 0, 0, 0), utc(2026, 4, 1, 0, 0, 0), MaxInstances)
				sameTimes(t, starts(insts), c.want)
				uniqueInstances(t, insts)
			})
		}
	}
}

// uniqueInstances fails when two instances share a start or a
// recurrence ID.
func uniqueInstances(t *testing.T, insts []Instance) {
	t.Helper()
	starts, ids := map[time.Time]bool{}, map[time.Time]bool{}
	for _, in := range insts {
		if starts[in.Start] || ids[in.RecurrenceID] {
			t.Fatalf("instance %v (recurrence ID %v) is repeated", in.Start, in.RecurrenceID)
		}
		starts[in.Start], ids[in.RecurrenceID] = true, true
	}
}

// An hourly series has one instance per hour that exists, with its own
// recurrence ID, across both DST changes: 23 on the spring day and 24 on
// the autumn one, whose repeated hour recurs once, in its first pass.
func TestExpandHourlyAcrossDSTUnique(t *testing.T) {
	const ev = "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260301T000000\nDURATION:PT15M\n" +
		"RRULE:FREQ=HOURLY;BYMONTH=3,11;BYMONTHDAY=1,8\nEND:VEVENT\n"
	for zone, events := range dstZones(t, ev) {
		t.Run(zone, func(t *testing.T) {
			insts, truncated := mustExpand(t, events, utc(2026, 1, 1, 0, 0, 0), utc(2027, 1, 1, 0, 0, 0), MaxInstances)
			uniqueInstances(t, insts)
			if len(insts) != 24+23+24+24 || truncated {
				t.Fatalf("got %d instances (truncated %v), want 95", len(insts), truncated)
			}
			if got := insts[46].Start; !got.Equal(utc(2026, 3, 8, 23+7, 0, 0)) {
				t.Fatalf("last instance on 03-08 = %v, want 23:00 PDT", got)
			}
		})
	}
}

// Samoa skipped 2011-12-30, moving from UTC-10 to UTC+14. A daily
// instance on the skipped day, read with the offset before the gap, is
// the next day's instance, and is one instance, counted once.
func TestExpandSkippedDay(t *testing.T) {
	apia, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Skip("no tzdata for Pacific/Apia")
	}
	ev := master("FREQ=DAILY;COUNT=4", time.Date(2011, 12, 29, 9, 0, 0, 0, apia).UTC(), time.Hour)
	ev.TZID = "Pacific/Apia"
	insts, _ := mustExpand(t, []Event{ev}, utc(2011, 12, 1, 0, 0, 0), utc(2012, 2, 1, 0, 0, 0), MaxInstances)
	uniqueInstances(t, insts)
	sameTimes(t, starts(insts), []time.Time{
		utc(2011, 12, 29, 19, 0, 0), // 09:00 UTC-10
		utc(2011, 12, 30, 19, 0, 0), // the skipped 30th, read at UTC-10, is 09:00 on the 31st
		utc(2011, 12, 31, 19, 0, 0), // 2012-01-01 09:00 UTC+14
		utc(2012, 1, 1, 19, 0, 0),
	})
}
