package caldav

import (
	"fmt"
	"strconv"
	"time"

	"github.com/emersion/go-ical"
)

// Bounds of VTIMEZONE generation.
const (
	// tzCheckYears is how many years past an event's last year a yearly
	// rule must hold before it is written as an RRULE.
	tzCheckYears = 10
	// tzMaxYears bounds the years one VTIMEZONE spells out.
	tzMaxYears = 100
	// tzMaxTransitions bounds the transitions read from Go's zone data.
	tzMaxTransitions = 2 * (tzMaxYears + tzCheckYears + 2)
)

// transition is one change of UTC offset.
type transition struct {
	at       time.Time // the instant of the change
	from, to int       // offsets in seconds east of UTC
	name     string    // the zone abbreviation after the change
	dst      bool      // whether the offset after the change is daylight time
}

// local is the wall time of the change in the offset before it, which is
// how a VTIMEZONE observance's DTSTART is written.
func (t transition) local() time.Time {
	return t.at.In(time.FixedZone("", t.from))
}

// transitions lists the offset changes of loc in [start, end).
func transitions(loc *time.Location, start, end time.Time) []transition {
	var out []transition
	t := start.In(loc)
	_, prev := t.Zone()
	for range tzMaxTransitions {
		_, next := t.ZoneBounds()
		if next.IsZero() || !next.Before(end) || !next.After(t) {
			break
		}
		nt := next.In(loc)
		name, off := nt.Zone()
		if off != prev {
			out = append(out, transition{at: next, from: prev, to: off, name: name, dst: nt.IsDST()})
		}
		prev, t = off, nt
	}
	return out
}

// yearRule is a yearly transition rule: the nth (or, for -1, the last)
// weekday of a month, at a wall time in the offset before the change.
type yearRule struct {
	month    time.Month
	weekday  time.Weekday
	nth      int
	clock    time.Duration // the wall time since midnight
	from, to int
}

// at is the instant the rule gives in year y.
func (r yearRule) at(y int) time.Time {
	zone := time.FixedZone("", r.from)
	var day int
	if r.nth > 0 {
		first := time.Date(y, r.month, 1, 0, 0, 0, 0, zone)
		day = 1 + (int(r.weekday)-int(first.Weekday())+7)%7 + (r.nth-1)*7
	} else {
		last := time.Date(y, r.month+1, 0, 0, 0, 0, 0, zone)
		day = last.Day() - (int(last.Weekday())-int(r.weekday)+7)%7
	}
	return time.Date(y, r.month, day, 0, 0, 0, 0, zone).Add(r.clock)
}

// rulesFor returns the candidate rules of a transition: the nth weekday,
// and also the last one when the day is in the month's last week.
func rulesFor(t transition) []yearRule {
	l := t.local()
	midnight := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location())
	base := yearRule{month: l.Month(), weekday: l.Weekday(), clock: l.Sub(midnight), from: t.from, to: t.to}
	daysIn := time.Date(l.Year(), l.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	nth := base
	nth.nth = (l.Day()-1)/7 + 1
	last := base
	last.nth = -1
	if l.Day()+7 > daysIn {
		return []yearRule{last, nth}
	}
	return []yearRule{nth}
}

// holds reports whether rules give exactly the transitions of year y.
func holds(rules []yearRule, y int, got []transition) bool {
	if len(rules) != len(got) {
		return false
	}
	for i, r := range rules {
		if !r.at(y).Equal(got[i].at) || r.from != got[i].from || r.to != got[i].to {
			return false
		}
	}
	return true
}

// vtimezone builds a VTIMEZONE for the zone id from Go's zone data. It
// covers the years first to last (and every later year when open), spelling
// out each transition, and writes the final yearly pattern as RRULEs when
// it holds to tzCheckYears past last. A zone with no transitions gets one
// fixed observance.
func vtimezone(id string, loc *time.Location, first, last int, open bool) *ical.Component {
	first = max(first, 1800)
	last = min(max(last, first), first+tzMaxYears)
	checkEnd := last + tzCheckYears
	all := transitions(loc,
		time.Date(first, 1, 1, 0, 0, 0, 0, loc),
		time.Date(checkEnd+1, 1, 1, 0, 0, 0, 0, loc))
	byYear := map[int][]transition{}
	for _, t := range all {
		y := t.local().Year()
		byYear[y] = append(byYear[y], t)
	}

	tz := ical.NewComponent(ical.CompTimezone)
	setRaw(tz.Props, ical.PropTimezoneID, id)
	if len(all) == 0 {
		// The offset is sampled inside the span, since a zone with no
		// change there may have had another offset in 1970 (Kathmandu,
		// Singapore). DTSTART is backdated to 1970 or the span's start,
		// whichever is earlier, so earlier times still resolve.
		sample := time.Date(first, 1, 1, 0, 0, 0, 0, loc)
		name, off := sample.Zone()
		start := time.Date(min(first, 1970), 1, 1, 0, 0, 0, 0, time.UTC)
		tz.Children = append(tz.Children, observance(sample.IsDST(), localStamp(start), off, off, name, ""))
		return tz
	}

	// The rule of the last checked year, if it has transitions, and the
	// first year from which it holds without a break.
	var rules []yearRule
	ruleFrom := checkEnd + 1
	if final := byYear[checkEnd]; len(final) > 0 && len(final) <= 4 {
		cand := pickRules(final, byYear, checkEnd, first)
		for y := checkEnd; y >= first && holds(cand, y, byYear[y]); y-- {
			ruleFrom = y
		}
		if ruleFrom <= checkEnd && (open || ruleFrom <= last) {
			rules = cand
		}
	}

	// Transitions are spelled out through the event's last year, or every
	// checked year for an open series, and stop where the rules start.
	explicitEnd := last
	if open {
		explicitEnd = checkEnd
	}
	if rules != nil {
		explicitEnd = min(explicitEnd, ruleFrom-1)
	}
	for _, t := range all {
		if t.local().Year() > explicitEnd {
			break
		}
		tz.Children = append(tz.Children, observance(t.dst, localStamp(t.local()), t.from, t.to, t.name, ""))
	}
	for i, r := range rules {
		t := byYear[ruleFrom][i]
		rrule := fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%d%s", int(r.month), r.nth, weekdayCode(r.weekday))
		tz.Children = append(tz.Children, observance(t.dst, localStamp(t.local()), t.from, t.to, t.name, rrule))
	}
	if len(tz.Children) == 0 {
		// Every transition is outside the span: the offset at the start holds.
		start := time.Date(first, 1, 1, 0, 0, 0, 0, loc)
		name, off := start.Zone()
		tz.Children = append(tz.Children, observance(start.IsDST(), localStamp(start), off, off, name, ""))
	}
	return tz
}

// pickRules chooses, for each transition of the final year, the candidate
// rule that holds for the most years before it.
func pickRules(final []transition, byYear map[int][]transition, end, first int) []yearRule {
	rules := make([]yearRule, len(final))
	for i, t := range final {
		cands := rulesFor(t)
		best, bestYears := cands[0], -1
		for _, c := range cands {
			n := 0
			for y := end; y >= first; y-- {
				got := byYear[y]
				if i >= len(got) || !c.at(y).Equal(got[i].at) {
					break
				}
				n++
			}
			if n > bestYears {
				best, bestYears = c, n
			}
		}
		rules[i] = best
	}
	return rules
}

func observance(dst bool, start string, from, to int, name, rrule string) *ical.Component {
	kind := ical.CompTimezoneStandard
	if dst {
		kind = ical.CompTimezoneDaylight
	}
	c := ical.NewComponent(kind)
	setRaw(c.Props, ical.PropDateTimeStart, start)
	setRaw(c.Props, ical.PropTimezoneOffsetFrom, offsetString(from))
	setRaw(c.Props, ical.PropTimezoneOffsetTo, offsetString(to))
	if name != "" && tokenSafe(name) {
		setRaw(c.Props, ical.PropTimezoneName, name)
	}
	if rrule != "" {
		setRaw(c.Props, ical.PropRecurrenceRule, rrule)
	}
	return c
}

func localStamp(t time.Time) string { return t.Format("20060102T150405") }

// offsetString writes a UTC offset as RFC 5545 UTC-OFFSET: +hhmm or
// +hhmmss.
func offsetString(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	h, m, s := sec/3600, sec/60%60, sec%60
	out := sign + pad2(h) + pad2(m)
	if s != 0 {
		out += pad2(s)
	}
	return out
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func weekdayCode(d time.Weekday) string {
	return [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[d]
}

// tokenSafe reports whether a zone abbreviation can be written as is.
func tokenSafe(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e || r == ';' || r == ':' || r == ',' || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}
