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
// were more than limit instances.
//
// SECONDLY and MINUTELY rules return ErrUnsafeRecurrence, as do an INTERVAL
// over 10000, HOURLY with BYSETPOS, and a rule whose estimated work (the
// periods from DTSTART to to, times the days and candidate instances of
// each, plus the search for an instance past to) is too large. A rule that
// does not parse, or holds a value outside its RFC 5545 range, returns
// ErrInvalidICS. A rule with no instance at all adds nothing to DTSTART and
// the RDATEs.
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
func occurrences(ev Event, from, to time.Time, limit int, overridden map[int64]bool) (out []Instance, err error) {
	// rrule-go panics on input parseRule does not foresee. A panic here is
	// reported as a bad rule, since one hostile invite must not stop the
	// daemon; it also covers this function's own arithmetic.
	defer func() {
		if recover() != nil {
			out, err = nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
		}
	}()
	fr := frameOf(ev)
	dur := max(ev.End.Sub(ev.Start), 0)
	start := fr.wall(ev.Start)

	var set rrule.Set
	if ev.RRule != "" {
		r, err := seriesRule(ev.RRule, fr, start, to)
		if err != nil {
			return nil, err
		}
		if r != nil {
			set.RRule(r)
		}
	}
	set.DTStart(start)
	set.RDate(start) // DTSTART is always the first instance (RFC 5545 3.8.5.3)
	for _, t := range ev.RDates {
		set.RDate(fr.wall(t))
	}
	for _, t := range ev.ExDates {
		set.ExDate(fr.wall(t))
	}

	next := set.Iterator()
	for len(out) < limit {
		w, ok := next()
		if !ok {
			break
		}
		t := fr.instant(w)
		if !t.Before(to) {
			break
		}
		if overridden[t.Unix()] || !overlaps(t, t.Add(dur), from, to) {
			continue
		}
		out = append(out, Instance{Event: ev, Start: t, End: t.Add(dur), RecurrenceID: t})
	}
	return out, nil
}

// seriesRule builds the rule of a master starting at start, in the frame
// fr, ending at to. It returns nil when the rule adds no instance to
// DTSTART and the RDATEs, and ErrUnsafeRecurrence when its estimated work
// is over maxRuleWork.
func seriesRule(value string, fr frame, start, to time.Time) (*rrule.RRule, error) {
	opt, err := parseRule(value, fr.loc)
	if err != nil {
		return nil, err
	}
	opt.Dtstart = start
	// No instance at or after to is wanted, so the rule stops there.
	if end := fr.wall(to); opt.Until.IsZero() || opt.Until.After(end) {
		opt.Until = end
	}
	if !hourReachable(opt, start.Hour()) {
		return nil, nil
	}
	c := costOf(opt)
	if c.canMiss && patternYears(opt) > maxPatternYears {
		return nil, ErrUnsafeRecurrence
	}
	if c.work(periodsBetween(opt, start, opt.Until), searchSteps(opt, c)) > maxRuleWork {
		return nil, ErrUnsafeRecurrence
	}
	if c.canMiss && !matchesEver(opt, start) {
		return nil, nil
	}
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
	}
	return r, nil
}

// frame is the clock a series recurs on: rrule-go runs in its zone.
type frame struct {
	loc *time.Location
}

// frameOf returns the frame of a master: its TZID's IANA zone, else UTC.
func frameOf(ev Event) frame {
	if !ev.AllDay {
		if l, _, ok := loadZone(ev.TZID); ok {
			return frame{loc: l}
		}
	}
	return frame{loc: time.UTC}
}

// wall returns the instant u in the frame.
func (f frame) wall(u time.Time) time.Time {
	return u.In(f.loc)
}

// instant returns the UTC instant of a time in the frame.
func (f frame) instant(w time.Time) time.Time {
	return w.UTC()
}

// overlaps reports whether [s, e) overlaps [from, to). A zero-length
// interval overlaps the range holding its instant.
func overlaps(s, e, from, to time.Time) bool {
	if e.After(s) {
		return s.Before(to) && e.After(from)
	}
	return !s.Before(from) && s.Before(to)
}
