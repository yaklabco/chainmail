package calendar

import (
	"errors"
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
