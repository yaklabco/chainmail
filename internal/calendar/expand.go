package calendar

import (
	"fmt"
	"slices"
	"time"

	"github.com/teambition/rrule-go"
)

// Expand returns the instances of one series (a master plus its overrides,
// or one single event) that overlap [from, to), sorted by Start, at most limit.
// It applies RDATE, EXDATE and overrides, drops CANCELLED overrides, keeps
// local wall time across DST using TZID, and reports truncated when there
// were more than limit instances. SECONDLY or MINUTELY rules return
// ErrUnsafeRecurrence, as does a rule with more than a million periods
// between DTSTART and to; a rule that does not parse returns ErrInvalidICS.
//
// limit <= 0 means MaxInstances. An event without RRULE but with RDATEs
// recurs on DTSTART and its RDATEs. An override is placed by its own Start
// and End and replaces the instance its RecurrenceID names, even when one
// is inside the range and the other is not. Overrides with no master in
// series, as in an invite to one instance, still expand. A cancelled master
// or single event is returned as it is; callers filter by Status.
func Expand(series []Event, from, to time.Time, limit int) (insts []Instance, truncated bool, err error) {
	if limit <= 0 {
		limit = MaxInstances
	}
	from, to = from.UTC(), to.UTC()

	overridden := map[int64]bool{}
	for _, ev := range series {
		if ev.RecurrenceID.IsZero() {
			continue
		}
		overridden[ev.RecurrenceID.Unix()] = true
		if ev.Status == StatusCancelled || !overlaps(ev.Start, ev.End, from, to) {
			continue
		}
		insts = append(insts, Instance{Event: ev, Start: ev.Start.UTC(), End: ev.End.UTC(), RecurrenceID: ev.RecurrenceID.UTC()})
	}
	for _, ev := range series {
		if !ev.RecurrenceID.IsZero() {
			continue
		}
		if ev.RRule == "" && len(ev.RDates) == 0 {
			if overlaps(ev.Start, ev.End, from, to) {
				insts = append(insts, Instance{Event: ev, Start: ev.Start.UTC(), End: ev.End.UTC()})
			}
			continue
		}
		// Enough rule instances to fill limit after overrides are merged in,
		// plus one to tell whether there were more.
		occ, err := occurrences(ev, from, to, limit+1, overridden)
		if err != nil {
			return nil, false, err
		}
		insts = append(insts, occ...)
	}
	slices.SortStableFunc(insts, func(a, b Instance) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return a.RecurrenceID.Compare(b.RecurrenceID)
	})
	if len(insts) > limit {
		return insts[:limit:limit], true, nil
	}
	return insts, false, nil
}

// occurrences expands the rule, RDATEs and EXDATEs of one master into at
// most limit instances overlapping [from, to), skipping overridden ones.
func occurrences(ev Event, from, to time.Time, limit int, overridden map[int64]bool) ([]Instance, error) {
	loc := time.UTC
	if !ev.AllDay {
		if l, _, ok := loadZone(ev.TZID); ok {
			loc = l
		}
	}
	dur := max(ev.End.Sub(ev.Start), 0)
	start := ev.Start.In(loc)

	var set rrule.Set
	if ev.RRule != "" {
		opt, err := rrule.StrToROptionInLocation(ev.RRule, loc)
		if err != nil {
			return nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
		}
		if opt.Freq == rrule.SECONDLY || opt.Freq == rrule.MINUTELY {
			return nil, ErrUnsafeRecurrence
		}
		opt.Dtstart = start
		// No instance at or after to is wanted, so the rule stops there.
		if opt.Until.IsZero() || opt.Until.After(to) {
			opt.Until = to
		}
		if periods(opt.Freq, opt.Interval, opt.Until.Sub(start)) > maxPeriods {
			return nil, ErrUnsafeRecurrence
		}
		r, err := rrule.NewRRule(*opt)
		if err != nil {
			return nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
		}
		set.RRule(r)
	}
	set.DTStart(start)
	set.RDate(start) // DTSTART is always the first instance (RFC 5545 3.8.5.3)
	for _, t := range ev.RDates {
		set.RDate(t.In(loc))
	}
	for _, t := range ev.ExDates {
		set.ExDate(t.In(loc))
	}

	var out []Instance
	next := set.Iterator()
	for len(out) < limit {
		t, ok := next()
		if !ok || !t.Before(to) {
			break
		}
		t = t.UTC()
		if overridden[t.Unix()] || !overlaps(t, t.Add(dur), from, to) {
			continue
		}
		out = append(out, Instance{Event: ev, Start: t, End: t.Add(dur), RecurrenceID: t})
	}
	return out, nil
}

// overlaps reports whether [s, e) overlaps [from, to). A zero-length
// interval overlaps the range holding its instant.
func overlaps(s, e, from, to time.Time) bool {
	if e.After(s) {
		return s.Before(to) && e.After(from)
	}
	return !s.Before(from) && s.Before(to)
}

// maxPeriods bounds the rule periods between DTSTART and the end of the
// range. rrule-go iterates every period from DTSTART, so an hourly rule
// starting centuries ago would take seconds to reach the range.
const maxPeriods = 1_000_000

// periods estimates how many periods of a rule fit in d.
func periods(freq rrule.Frequency, interval int, d time.Duration) int64 {
	var period time.Duration
	switch freq {
	case rrule.HOURLY:
		period = time.Hour
	case rrule.DAILY:
		period = 24 * time.Hour
	case rrule.WEEKLY:
		period = 7 * 24 * time.Hour
	case rrule.MONTHLY:
		period = 28 * 24 * time.Hour
	default:
		period = 365 * 24 * time.Hour
	}
	return int64(d / (period * time.Duration(max(interval, 1))))
}
