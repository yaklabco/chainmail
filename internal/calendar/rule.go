package calendar

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// Recurrence rules come from whoever sent the invite, and rrule-go trusts
// them. It builds the product of the BYHOUR, BYMINUTE and BYSECOND lists
// without removing repeats, checks every BY list for each day it visits,
// iterates every period from DTSTART, searches up to year 9999 for an
// instance that never comes, loops forever on an HOURLY rule that cannot
// reach its BYHOUR, and indexes past its tables with some values the RFC
// syntax allows. parseRule validates a rule, and ruleCost estimates its
// work, before rrule-go builds anything.

const (
	// maxInterval bounds INTERVAL, which keeps period arithmetic small.
	maxInterval = 10_000
	// maxRuleWork bounds the estimated work of one rule, in units of about
	// one day checked against one BY value: a few nanoseconds each, so the
	// bound is about a tenth of a second.
	maxRuleWork = 1 << 25
	// stepWork is the fixed work of one rrule-go period step.
	stepWork = 8
	// instanceWork is the work of one instance rrule-go builds and the
	// caller consumes.
	instanceWork = 64
	// cycleYears is the Gregorian cycle: dates fall on the same weekdays
	// every 400 years, so a rule's instances moved by a multiple of it are
	// the rule's instances from a DTSTART moved by as much.
	cycleYears = 400
	// maxPatternYears bounds how long the instances of a rule that can miss
	// take to repeat, so a probe for one fits before rrule-go's year limit.
	maxPatternYears = 4000
	secondsPerYear  = 31_556_952 // 365.2425 days
)

// parseRule parses an RRULE value, reading a local UNTIL in loc, and checks
// it. FREQ=SECONDLY and MINUTELY, an INTERVAL over maxInterval, and HOURLY
// with BYSETPOS give ErrUnsafeRecurrence. A negative INTERVAL or COUNT, an
// explicit INTERVAL=0, BYEASTER (an rrule-go extension), a BY value outside
// its RFC 5545 range, and an ordinal BYDAY beyond the fifth week of a month
// give ErrInvalidICS. Repeated BY values are merged and the lists sorted,
// which rrule-go does not depend on.
func parseRule(value string, loc *time.Location) (*rrule.ROption, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "RRULE:")
	bad := fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
	opt, err := rrule.StrToROptionInLocation(value, loc)
	if err != nil {
		return nil, bad
	}
	_, hasInterval := ruleValue(value, "INTERVAL")
	switch {
	case opt.Freq == rrule.SECONDLY || opt.Freq == rrule.MINUTELY:
		return nil, ErrUnsafeRecurrence
	case opt.Interval < 0 || opt.Count < 0 || (opt.Interval == 0 && hasInterval) || len(opt.Byeaster) > 0:
		return nil, bad
	case opt.Interval > maxInterval:
		return nil, ErrUnsafeRecurrence
	case opt.Freq == rrule.HOURLY && len(opt.Bysetpos) > 0:
		// A period with days but no position steps an hour at a time, so
		// a rule that never matches would be searched hour by hour.
		return nil, ErrUnsafeRecurrence
	}
	for _, l := range []struct {
		v      *[]int
		lo, hi int
		signed bool
	}{
		{&opt.Bysecond, 0, 59, false}, // RFC 5545 allows 60, a leap second; rrule-go does not
		{&opt.Byminute, 0, 59, false},
		{&opt.Byhour, 0, 23, false},
		{&opt.Bymonthday, 1, 31, true},
		{&opt.Byyearday, 1, 366, true},
		{&opt.Byweekno, 1, 53, true},
		{&opt.Bymonth, 1, 12, false},
		{&opt.Bysetpos, 1, 366, true},
	} {
		for _, n := range *l.v {
			if l.signed && n < 0 {
				n = -n
			}
			if n < l.lo || n > l.hi {
				return nil, bad
			}
		}
		slices.Sort(*l.v)
		*l.v = slices.Compact(*l.v)
	}
	// An ordinal BYDAY counts weeks of the month in a MONTHLY rule, and in
	// a YEARLY rule with BYMONTH, and weeks of the year in other YEARLY
	// rules. rrule-go ignores it for other frequencies.
	maxWeek := 53
	if opt.Freq == rrule.MONTHLY || (opt.Freq == rrule.YEARLY && len(opt.Bymonth) > 0) {
		maxWeek = 5
	}
	for i := range opt.Byweekday {
		if n := opt.Byweekday[i].N(); n < -maxWeek || n > maxWeek {
			return nil, bad
		}
	}
	slices.SortFunc(opt.Byweekday, func(a, b rrule.Weekday) int {
		return cmp.Or(cmp.Compare(a.Day(), b.Day()), cmp.Compare(a.N(), b.N()))
	})
	opt.Byweekday = slices.Compact(opt.Byweekday)
	return opt, nil
}

// ruleValue returns the value of key in an RRULE value: the last one, as
// rrule-go reads a repeated key.
func ruleValue(rule, key string) (value string, found bool) {
	rule = strings.TrimPrefix(strings.TrimSpace(rule), "RRULE:")
	for _, part := range strings.Split(rule, ";") {
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

// untilIsUTC reports whether an RRULE value's UNTIL is a UTC time, which
// rrule-go reads as an instant rather than a wall time.
func untilIsUTC(rule string) bool {
	v, _ := ruleValue(rule, "UNTIL")
	return strings.HasSuffix(strings.ToUpper(v), "Z")
}

// ruleCost is the work of one period of a checked rule, in maxRuleWork's
// units.
type ruleCost struct {
	step int64 // visiting one period: each of its days against every BY list
	inst int64 // the instances one period can produce, at most
	// canMiss is set when a period can have no instance, so rrule-go can
	// search far past the end of the range for the next one.
	canMiss bool
}

func costOf(opt *rrule.ROption) ruleCost {
	var days int64
	switch opt.Freq {
	case rrule.YEARLY:
		days = 366
	case rrule.MONTHLY:
		days = 31
	case rrule.WEEKLY:
		days = 7
	default:
		days = 1
	}
	n := func(l int) int64 { return int64(max(l, 1)) }
	dayRules := len(opt.Byweekno) + len(opt.Byyearday) + len(opt.Bymonthday) + len(opt.Byweekday)
	lists := int64(dayRules + len(opt.Bymonth) + len(opt.Bysetpos))
	times := n(len(opt.Byminute)) * n(len(opt.Bysecond))
	c := ruleCost{step: stepWork + days*(1+lists), canMiss: lists > 0}
	if opt.Freq == rrule.HOURLY {
		// Every hour step rebuilds the minute and second list.
		c.step += int64(len(opt.Byhour)) + times*instanceWork/8
	} else {
		times *= n(len(opt.Byhour))
	}

	// The days of a period that can hold instances, at most.
	matched := days
	tighten := func(k int64) { matched = min(matched, k) }
	if dayRules == 0 {
		// rrule-go adds DTSTART's day of the month (YEARLY, MONTHLY) or
		// weekday (WEEKLY).
		matched = 1
		if opt.Freq == rrule.YEARLY {
			matched = n(len(opt.Bymonth))
		}
	} else {
		perYear := int64(1)
		if opt.Freq == rrule.YEARLY {
			perYear = 12
			if len(opt.Bymonth) > 0 {
				perYear = int64(len(opt.Bymonth))
				tighten(perYear * 31)
			}
		}
		if len(opt.Bymonthday) > 0 {
			tighten(int64(len(opt.Bymonthday)) * perYear)
		}
		if len(opt.Byyearday) > 0 {
			tighten(int64(len(opt.Byyearday)))
		}
		if len(opt.Byweekno) > 0 {
			tighten(int64(len(opt.Byweekno)) * 7)
		}
		if len(opt.Byweekday) > 0 && opt.Freq <= rrule.MONTHLY && allOrdinal(opt.Byweekday) {
			k := int64(1)
			if opt.Freq == rrule.YEARLY && len(opt.Bymonth) > 0 {
				k = int64(len(opt.Bymonth))
			}
			tighten(int64(len(opt.Byweekday)) * k)
		}
	}
	c.inst = satMul(matched, times)
	if len(opt.Bysetpos) > 0 {
		c.inst = min(c.inst, int64(len(opt.Bysetpos)))
	}
	return c
}

func allOrdinal(days []rrule.Weekday) bool {
	for i := range days {
		if days[i].N() == 0 {
			return false
		}
	}
	return true
}

// work estimates expanding p periods, plus the period in progress and the
// one whose instance ends the iteration, then searching search more periods
// for an instance.
func (c ruleCost) work(p, search int64) int64 {
	return satAdd(satMul(satAdd(p, 2), satAdd(c.step, satMul(instanceWork, c.inst))), satMul(search, c.step))
}

// periodSeconds is the shortest length of one period of freq.
func periodSeconds(f rrule.Frequency) int64 {
	switch f {
	case rrule.HOURLY:
		return 3600
	case rrule.DAILY:
		return 86400
	case rrule.WEEKLY:
		return 7 * 86400
	case rrule.MONTHLY:
		return 28 * 86400
	default:
		return 365 * 86400
	}
}

// periodsIn returns how many periods of the rule fit in secs, at most. The
// divisor is at most a year times maxInterval, so it cannot overflow.
func periodsIn(opt *rrule.ROption, secs int64) int64 {
	if secs <= 0 {
		return 0
	}
	return secs / (periodSeconds(opt.Freq) * int64(max(opt.Interval, 1)))
}

// periodsBetween returns how many periods of the rule fit between two
// times of the same frame, at most.
func periodsBetween(opt *rrule.ROption, from, to time.Time) int64 {
	return periodsIn(opt, to.Unix()-from.Unix())
}

// unitsPerCycle is how many periods of each frequency a Gregorian cycle
// holds.
var unitsPerCycle = map[rrule.Frequency]int64{
	rrule.YEARLY:  cycleYears,
	rrule.MONTHLY: cycleYears * 12,
	rrule.WEEKLY:  20_871,
	rrule.DAILY:   146_097,
	rrule.HOURLY:  146_097 * 24,
}

// patternYears is how many years the instances of a rule take to repeat:
// a whole number of cycles that is also a whole number of its periods.
func patternYears(opt *rrule.ROption) int64 {
	k := int64(max(opt.Interval, 1))
	return cycleYears * (k / gcd(k, unitsPerCycle[opt.Freq]))
}

// searchSteps bounds the periods rrule-go visits past the last one wanted.
// The next instance comes within one pattern of the rule, since its
// instances repeat with it. A rule that can miss also gets a probe of up to
// two patterns. Other DAILY, WEEKLY and HOURLY rules match within a day.
// An HOURLY period with no instance skips the rest of its day.
func searchSteps(opt *rrule.ROption, c ruleCost) int64 {
	years := patternYears(opt)
	if c.canMiss {
		years *= 3
	} else if opt.Freq != rrule.YEARLY && opt.Freq != rrule.MONTHLY {
		return 25
	}
	steps := periodsIn(opt, satMul(years, secondsPerYear)) + 1
	if opt.Freq == rrule.HOURLY {
		steps = min(steps, years*366+1)
	}
	return steps
}

// matchesEver reports whether a rule that can miss has an instance at all.
// rrule-go works on wall-clock dates, so the rule is tried from DTSTART
// moved by whole patterns to just under two patterns before year 10000:
// there it has an instance within one pattern if it has any, and rrule-go's
// own year limit ends a fruitless search.
func matchesEver(opt *rrule.ROption, start time.Time) bool {
	p := int(patternYears(opt))
	base := rrule.MAXYEAR + 1 - 2*p
	probe := *opt
	probe.Dtstart = time.Date(base+floorMod(start.Year()-base, p), start.Month(), start.Day(),
		start.Hour(), start.Minute(), start.Second(), 0, time.UTC)
	// Without UNTIL, rrule-go stops 292 years after DTSTART, inside the
	// pattern.
	probe.Until = time.Date(rrule.MAXYEAR+1, 1, 1, 0, 0, 0, 0, time.UTC)
	probe.Count = 1
	r, err := rrule.NewRRule(probe)
	if err != nil {
		return false
	}
	_, ok := r.Iterator()()
	return ok
}

// hourReachable reports whether an HOURLY rule stepping from hour h can
// land on one of its BYHOUR values. rrule-go loops forever when it cannot.
func hourReachable(opt *rrule.ROption, h int) bool {
	if opt.Freq != rrule.HOURLY || len(opt.Byhour) == 0 {
		return true
	}
	g := int(gcd(int64(max(opt.Interval, 1)), 24))
	for _, bh := range opt.Byhour {
		if floorMod(bh-h, g) == 0 {
			return true
		}
	}
	return false
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func floorMod(a, m int) int {
	return ((a % m) + m) % m
}

// satAdd adds non-negative values, saturating.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// satMul multiplies non-negative values, saturating.
func satMul(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}
