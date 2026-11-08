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
// local wall time across DST, and reports truncated when there were more
// than limit instances.
//
// The zone a series recurs in is the VTIMEZONE its DTSTART names in the
// master's Raw, when Raw is an iCalendar object holding one (so a zone
// known only from its VTIMEZONE, which gives TZID "", still recurs on its
// own rules), else the IANA zone TZID names, else UTC. A UTC UNTIL and
// the end of the range are instants, so a repeated hour does not move
// them. An instance at a local time a DST change skips moves past the gap
// when the rule recurs once a day or less often, and is dropped, not
// counted toward COUNT, when it recurs more often (see seriesRule). Rules
// step in wall time, so the second pass of a repeated hour has no HOURLY
// instance of its own.
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

// occurrences expands the rule, RDATEs and EXDATEs of one master into the
// earliest instances overlapping [from, to), at most limit, skipping
// overridden ones. EXDATEs and overrides name instances by their instant.
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
	start := ev.Start.UTC()
	dur := max(ev.End.Sub(start), 0)

	rule := func() (w, t time.Time, ok bool) { return w, t, false }
	if ev.RRule != "" {
		next, err := seriesRule(ev.RRule, fr, start, to)
		if err != nil {
			return nil, err
		}
		if next != nil {
			rule = next
		}
	}
	// DTSTART is always the first instance (RFC 5545 3.8.5.3). It and the
	// RDATEs are instants already.
	dates := make([]time.Time, 0, len(ev.RDates)+1)
	dates = append(dates, start)
	for _, t := range ev.RDates {
		dates = append(dates, t.UTC())
	}
	slices.SortFunc(dates, time.Time.Compare)
	excluded := make(map[int64]bool, len(ev.ExDates))
	for _, t := range ev.ExDates {
		excluded[t.Unix()] = true
	}

	// Rule instances come in wall-time order. Their instants follow it,
	// except one moved past a gap, so the instants still to come are no
	// earlier than the next wall time less the zone's largest offset. The
	// rule itself ends past to (seriesRule bounds it), the dates are sorted.
	w, t, more := rule()
	seen := map[int64]bool{}
	// Once limit instances are kept, latest is the latest start among them,
	// and no rule wall time after stop has an instant before it.
	var latest, stop time.Time
	for more || len(dates) > 0 {
		if !more && !dates[0].Before(to) {
			break
		}
		if len(out) >= limit && (!more || !w.Before(stop)) && (len(dates) == 0 || !dates[0].Before(latest)) {
			break // nothing left starts before the instances kept
		}
		var at time.Time
		if more && (len(dates) == 0 || !dates[0].Before(t)) {
			at = t
			w, t, more = rule()
		} else {
			at, dates = dates[0], dates[1:]
		}
		key := at.Unix()
		if seen[key] || excluded[key] || overridden[key] || !overlaps(at, at.Add(dur), from, to) ||
			(len(out) >= limit && !at.Before(latest)) {
			continue
		}
		seen[key] = true
		out = append(out, Instance{Event: ev, Start: at, End: at.Add(dur), RecurrenceID: at})
		if len(out) == limit {
			for _, in := range out {
				if in.Start.After(latest) {
					latest = in.Start
				}
			}
			stop = fr.latestWall(latest)
		}
	}
	if len(out) > limit {
		slices.SortFunc(out, func(a, b Instance) int { return a.Start.Compare(b.Start) })
		out = out[:limit]
	}
	return out, nil
}

// seriesRule returns the instances of a master's rule, in the frame fr,
// from DTSTART start to past to: their wall times in rule order, with their
// instants. It returns nil when the rule adds no instance to DTSTART and
// the RDATEs, and ErrUnsafeRecurrence when its estimated work is over
// maxRuleWork.
//
// A rule wall time that a DST change skips is handled after RFC 5545. A
// rule that recurs at most once a day at one time of day (FREQ=DAILY or
// coarser, with at most one BYHOUR, BYMINUTE and BYSECOND value) names
// that time of day on each of its days, so, as 3.3.5 reads a DATE-TIME
// such as DTSTART, its instance on the day of the change is read with the
// offset before the gap and lands as far past it. That is what calendar
// clients show. Any other rule (FREQ=HOURLY, or several times a day) loses
// the instances in the gap, which 3.3.10 says are ignored and not counted;
// moving them could land them on another instance. COUNT is therefore
// applied here, not by rrule-go. A wall time a change repeats is its first
// occurrence, and the wall time of DTSTART is DTSTART itself.
func seriesRule(value string, fr frame, start, to time.Time) (next func() (w, t time.Time, ok bool), err error) {
	// The frame's wall times are UTC values, so a local UNTIL reads as one.
	opt, err := parseRule(value, time.UTC)
	if err != nil {
		return nil, err
	}
	// A UTC UNTIL is an instant. The rule runs to the last wall time that
	// can be at or before it, and instances after it are dropped.
	var until time.Time
	if !opt.Until.IsZero() && untilIsUTC(value) {
		until = opt.Until
		opt.Until = fr.latestWall(until)
	}
	startWall := fr.wall(start)
	opt.Dtstart = startWall
	// No instance at or after to is wanted, so the rule stops at the last
	// wall time before it.
	if end := fr.latestWall(to); opt.Until.IsZero() || opt.Until.After(end) {
		opt.Until = end
	}
	count := opt.Count
	opt.Count = 0
	if !hourReachable(opt, startWall.Hour()) {
		return nil, nil
	}
	c := costOf(opt)
	if c.canMiss && patternYears(opt) > maxPatternYears {
		return nil, ErrUnsafeRecurrence
	}
	if c.work(periodsBetween(opt, startWall, opt.Until), searchSteps(opt, c)) > maxRuleWork {
		return nil, ErrUnsafeRecurrence
	}
	if c.canMiss && !matchesEver(opt, startWall) {
		return nil, nil
	}
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
	}
	skipGaps := opt.Freq > rrule.DAILY || len(opt.Byhour) > 1 || len(opt.Byminute) > 1 || len(opt.Bysecond) > 1
	it := r.Iterator()
	n := 0
	// moved holds the instants of instances moved past a gap. Wall times
	// that exist have instants of their own, so only a moved instance can
	// fall on another (a day moved past a gap of a day or more falls on the
	// next one). They are the same instance, counted once.
	moved := map[int64]bool{}
	return func() (time.Time, time.Time, bool) {
		for count == 0 || n < count {
			w, ok := it()
			if !ok {
				break
			}
			t, exists := fr.instant(w)
			if w.Equal(startWall) {
				t, exists = start, true
			}
			if (!exists && skipGaps) || moved[t.Unix()] {
				continue
			}
			if !exists {
				moved[t.Unix()] = true
			}
			n++
			if !until.IsZero() && t.After(until) {
				continue
			}
			return w, t, true
		}
		return time.Time{}, time.Time{}, false
	}, nil
}

// frame is the clock a series recurs on. rrule-go runs on its wall times,
// held as UTC values, which are converted with the series' VTIMEZONE as
// ParseICS converts them, or with its IANA zone, or are UTC.
type frame struct {
	loc *time.Location // the IANA zone or UTC, when vtz is nil
	vtz *vtimezone
}

// frameOf returns the frame of a master: the VTIMEZONE its DTSTART names
// in Raw, when Raw is an iCalendar object holding one, else its TZID's
// IANA zone, else UTC. Raw survives the calendar cache, so a series read
// back from it recurs on its own zone's rules too.
func frameOf(ev Event) frame {
	if !ev.AllDay {
		if v := embeddedZone(ev); v != nil {
			return frame{loc: time.UTC, vtz: v}
		}
		if l, _, ok := loadZone(ev.TZID); ok {
			return frame{loc: l}
		}
	}
	return frame{loc: time.UTC}
}

// wall returns the wall time of the instant u in the frame.
func (f frame) wall(u time.Time) time.Time {
	if f.vtz != nil {
		return f.vtz.wallOf(u.UTC())
	}
	return naiveOf(u.In(f.loc))
}

// instant returns the UTC instant of a wall time in the frame, and whether
// the wall time exists, read as RFC 5545 3.3.5 reads a DATE-TIME.
func (f frame) instant(w time.Time) (time.Time, bool) {
	switch {
	case f.vtz != nil:
		return f.vtz.instant(w)
	case f.loc == time.UTC:
		return w, true
	default:
		return localInstant(w, f.loc)
	}
}

// maxOffset returns an offset no smaller than any the frame has in force
// within days of the time around, a wall time or an instant.
func (f frame) maxOffset(around time.Time) time.Duration {
	switch {
	case f.vtz != nil:
		return f.vtz.maxOffset()
	case f.loc == time.UTC:
		return 0
	default:
		return maxLocalOffset(f.loc, around.AddDate(0, 0, -3), around.AddDate(0, 0, 3))
	}
}

// latestWall returns a wall time no earlier than any whose instant is at or
// before u: no wall time after it is at or before u.
func (f frame) latestWall(u time.Time) time.Time {
	return u.UTC().Add(f.maxOffset(u))
}

// overlaps reports whether [s, e) overlaps [from, to). A zero-length
// interval overlaps the range holding its instant.
func overlaps(s, e, from, to time.Time) bool {
	if e.After(s) {
		return s.Before(to) && e.After(from)
	}
	return !s.Before(from) && s.Before(to)
}
