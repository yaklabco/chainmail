package calendar

import (
	"bytes"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// utcNames are zone names that mean UTC. An event in one of them has TZID "".
var utcNames = map[string]bool{
	"UTC": true, "Etc/UTC": true, "Etc/UCT": true, "UCT": true,
	"Universal": true, "Etc/Universal": true, "Zulu": true, "Etc/Zulu": true,
	"GMT": true, "Etc/GMT": true, "Etc/GMT0": true, "GMT0": true,
}

// windowsZones maps the Windows zone names Outlook and Exchange write to
// IANA zones, after CLDR's windowsZones table (territory 001).
var windowsZones = map[string]string{
	"Dateline Standard Time":          "Etc/GMT+12",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Alaskan Standard Time":           "America/Anchorage",
	"Pacific Standard Time":           "America/Los_Angeles",
	"Pacific Standard Time (Mexico)":  "America/Tijuana",
	"US Mountain Standard Time":       "America/Phoenix",
	"Mountain Standard Time":          "America/Denver",
	"Mountain Standard Time (Mexico)": "America/Mazatlan",
	"Central America Standard Time":   "America/Guatemala",
	"Central Standard Time":           "America/Chicago",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"SA Pacific Standard Time":        "America/Bogota",
	"Eastern Standard Time":           "America/New_York",
	"US Eastern Standard Time":        "America/Indianapolis",
	"Atlantic Standard Time":          "America/Halifax",
	"Newfoundland Standard Time":      "America/St_Johns",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"Argentina Standard Time":         "America/Buenos_Aires",
	"Pacific SA Standard Time":        "America/Santiago",
	"UTC":                             "UTC",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Romance Standard Time":           "Europe/Paris",
	"Central European Standard Time":  "Europe/Warsaw",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"GTB Standard Time":               "Europe/Bucharest",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"FLE Standard Time":               "Europe/Kiev",
	"Israel Standard Time":            "Asia/Jerusalem",
	"Egypt Standard Time":             "Africa/Cairo",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"Turkey Standard Time":            "Europe/Istanbul",
	"Russian Standard Time":           "Europe/Moscow",
	"Arabian Standard Time":           "Asia/Dubai",
	"India Standard Time":             "Asia/Calcutta",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"China Standard Time":             "Asia/Shanghai",
	"Singapore Standard Time":         "Asia/Singapore",
	"Taipei Standard Time":            "Asia/Taipei",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"Korea Standard Time":             "Asia/Seoul",
	"W. Australia Standard Time":      "Australia/Perth",
	"Cen. Australia Standard Time":    "Australia/Adelaide",
	"E. Australia Standard Time":      "Australia/Brisbane",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"New Zealand Standard Time":       "Pacific/Auckland",
}

// loadZone loads an IANA zone by name. A name meaning UTC returns time.UTC
// and the name "". It never returns the host's local zone.
func loadZone(name string) (loc *time.Location, canonical string, ok bool) {
	if name == "" || name == "Local" || hasControl(name) {
		return nil, "", false
	}
	if utcNames[name] {
		return time.UTC, "", true
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", false
	}
	return loc, loc.String(), true
}

// zones resolves the TZID parameters of one VCALENDAR.
type zones struct {
	comps map[string]*ical.Component // VTIMEZONE by TZID
	defs  map[string]*vtimezone      // comps read on first use; nil when unusable
	cache map[string]zone
	// budget is the observance rule work left for this calendar, in
	// maxRuleWork's units.
	budget int64
}

// zone is a resolved TZID: rules from a VTIMEZONE (vtz set), an IANA zone
// (loc set), or floating (neither). name is the IANA name the TZID stands
// for, "" for UTC and for zones without one.
type zone struct {
	loc  *time.Location
	name string
	vtz  *vtimezone
}

func newZones(cal *ical.Component) *zones {
	z := &zones{comps: map[string]*ical.Component{}, defs: map[string]*vtimezone{}, cache: map[string]zone{}, budget: maxRuleWork}
	for _, child := range cal.Children {
		if child.Name != ical.CompTimezone {
			continue
		}
		if id := child.Props.Get(ical.PropTimezoneID); id != nil {
			z.comps[strings.TrimSpace(id.Value)] = child
		}
	}
	return z
}

func (z *zones) resolve(tzid string) zone {
	tzid = strings.TrimSpace(tzid)
	if r, ok := z.cache[tzid]; ok {
		return r
	}
	r := z.lookup(tzid)
	z.cache[tzid] = r
	return r
}

// lookup prefers the calendar's own VTIMEZONE, which RFC 5545 makes the
// definition of its TZID, and falls back to the IANA zone the TZID names
// when the calendar has no usable VTIMEZONE for it, or one that is
// degraded. A degraded VTIMEZONE still beats floating time for a TZID that
// names no IANA zone.
func (z *zones) lookup(tzid string) zone {
	loc, name, ok := ianaZone(tzid)
	if v := z.def(tzid); v != nil && (!v.degraded || !ok) {
		return zone{name: name, vtz: v}
	}
	if ok {
		return zone{loc: loc, name: name}
	}
	return zone{}
}

// def returns the VTIMEZONE for tzid, or nil when there is none or it has
// no usable observance.
func (z *zones) def(tzid string) *vtimezone {
	if v, ok := z.defs[tzid]; ok {
		return v
	}
	var v *vtimezone
	if comp := z.comps[tzid]; comp != nil {
		if v = parseVTimezone(comp, &z.budget); len(v.obs) == 0 {
			v = nil
		}
	}
	z.defs[tzid] = v
	return v
}

// ianaZone resolves a TZID to an IANA zone: directly, through the Windows
// zone names Outlook writes, or through a prefixed IANA name.
func ianaZone(tzid string) (*time.Location, string, bool) {
	if loc, name, ok := loadZone(tzid); ok {
		return loc, name, true
	}
	if iana, ok := windowsZones[tzid]; ok {
		if loc, name, ok := loadZone(iana); ok {
			return loc, name, true
		}
	}
	// Some producers prefix the IANA name, as in
	// "/mozilla.org/20050126_1/America/New_York".
	if strings.HasPrefix(tzid, "/") {
		parts := strings.Split(tzid, "/")
		for k := 3; k >= 1; k-- {
			if len(parts) <= k {
				continue
			}
			if loc, name, ok := loadZone(strings.Join(parts[len(parts)-k:], "/")); ok {
				return loc, name, true
			}
		}
	}
	return nil, "", false
}

const (
	localLayout = "20060102T150405"
	dateLayout  = "20060102"
)

// parseTime reads one DATE or DATE-TIME value with its TZID and VALUE
// parameters. It returns the instant in UTC, the IANA zone name for a
// TZID-qualified time ("" otherwise) and whether the value is a DATE.
// Floating times and unknown zones read as UTC.
func (z *zones) parseTime(value string, params ical.Params) (t time.Time, tzid string, date bool, ok bool) {
	value = strings.TrimSpace(value)
	vt := strings.ToUpper(params.Get(ical.ParamValue))
	if vt == string(ical.ValueDate) || (vt == "" && len(value) == len(dateLayout)) {
		t, err := time.ParseInLocation(dateLayout, value, time.UTC)
		return t, "", true, err == nil
	}
	if len(value) == len(localLayout)+1 && (value[len(value)-1] == 'Z' || value[len(value)-1] == 'z') {
		t, err := time.ParseInLocation(localLayout, value[:len(localLayout)], time.UTC)
		return t, "", false, err == nil
	}
	if len(value) != len(localLayout) {
		return time.Time{}, "", false, false
	}
	naive, err := time.ParseInLocation(localLayout, value, time.UTC)
	if err != nil {
		return time.Time{}, "", false, false
	}
	id := params.Get(ical.ParamTimezoneID)
	if id == "" {
		return naive, "", false, true
	}
	r := z.resolve(id)
	switch {
	case r.vtz != nil:
		return naive.Add(-r.vtz.offset(naive)), r.name, false, true
	case r.loc != nil:
		local := time.Date(naive.Year(), naive.Month(), naive.Day(), naive.Hour(), naive.Minute(), naive.Second(), 0, r.loc)
		return local.UTC(), r.name, false, true
	default:
		return naive, "", false, true
	}
}

// vtimezone holds the observances of a VTIMEZONE.
type vtimezone struct {
	obs []observance
	// offs holds the distinct offsets of the observances, ascending.
	offs []time.Duration
	// degraded is set when part of the VTIMEZONE was ignored, so its rules
	// may be incomplete.
	degraded bool

	// mu guards years: a zone embeddedZone remembers is shared.
	mu sync.Mutex
	// years caches, by year, the onsets that can be in force during it,
	// sorted by time.
	years map[int][]transition
}

// transition is an onset: from wall time at, the offset is to.
type transition struct {
	at time.Time
	to time.Duration
}

// maxObservances bounds the STANDARD and DAYLIGHT blocks read from one
// VTIMEZONE. Real zones have a few; more is hostile input.
const maxObservances = 64

// minRuleYear and maxRuleYear bound the years VTIMEZONE rules are
// evaluated for. Times outside them use the rules of the nearest one.
const (
	minRuleYear = 1970
	maxRuleYear = 2100
)

// observance is one STANDARD or DAYLIGHT block. Its onset times are local
// wall times held as UTC values, so they compare with other wall times.
type observance struct {
	start    time.Time
	from, to time.Duration
	// onsets are the RRULE's onsets from about ten years before minRuleYear
	// to the end of maxRuleYear, sorted.
	onsets []time.Time
	rdates []time.Time // sorted
}

// parseVTimezone reads the observances of a VTIMEZONE. RRULE onsets are
// computed here, once, and charged to budget. A rule that does not parse,
// recurs more often than monthly (no real zone changes offset more often
// than twice a year), or does not fit the budget is ignored, and its
// observance counts only at its DTSTART and RDATEs. Such a zone, and one
// with an observance block that does not parse or past maxObservances, is
// marked degraded.
func parseVTimezone(comp *ical.Component, budget *int64) *vtimezone {
	v := &vtimezone{years: map[int][]transition{}}
	for _, child := range comp.Children {
		if child.Name != ical.CompTimezoneStandard && child.Name != ical.CompTimezoneDaylight {
			continue
		}
		if len(v.obs) == maxObservances {
			v.degraded = true
			break
		}
		var o observance
		var ok bool
		if p := child.Props.Get(ical.PropDateTimeStart); p != nil {
			o.start, ok = naiveTime(p.Value)
		}
		from, okFrom := parseOffset(child.Props.Get(ical.PropTimezoneOffsetFrom))
		to, okTo := parseOffset(child.Props.Get(ical.PropTimezoneOffsetTo))
		if !ok || !okFrom || !okTo {
			v.degraded = true
			continue
		}
		o.from, o.to = from, to
		if p := child.Props.Get(ical.PropRecurrenceRule); p != nil {
			usable := false
			if opt, err := parseRule(p.Value, time.UTC); err == nil && (opt.Freq == rrule.YEARLY || opt.Freq == rrule.MONTHLY) {
				// A UTC UNTIL is an instant; onsets are wall times in the
				// offset before them.
				if !opt.Until.IsZero() && untilIsUTC(p.Value) {
					opt.Until = opt.Until.Add(from)
				}
				o.onsets, usable = ruleOnsets(o.start, opt, budget)
			}
			if !usable {
				v.degraded = true
			}
		}
		for _, p := range child.Props.Values(ical.PropRecurrenceDates) {
			for _, s := range strings.Split(p.Value, ",") {
				if t, ok := naiveTime(s); ok {
					o.rdates = append(o.rdates, t)
				}
			}
		}
		slices.SortFunc(o.rdates, time.Time.Compare)
		v.obs = append(v.obs, o)
		v.offs = append(v.offs, o.from, o.to)
	}
	slices.Sort(v.offs)
	v.offs = slices.Compact(v.offs)
	return v
}

const (
	// onsetFromYear is the first year observance rules are expanded from,
	// so the last onset before minRuleYear is known too.
	onsetFromYear = minRuleYear - 10
	// shiftYears moves observance rules forward by whole Gregorian cycles,
	// which leaves their onsets the same but puts the end of maxRuleYear
	// within three centuries of rrule-go's year limit, which ends its
	// search for the onset after the last one wanted.
	shiftYears = cycleYears * ((rrule.MAXYEAR - maxRuleYear) / cycleYears)
)

// ruleOnsets returns the onsets of an observance rule from about
// onsetFromYear to the end of maxRuleYear, and false when the rule cannot
// be used: the work it takes does not fit budget, or rrule-go refuses or
// panics on it. A rule without COUNT is started at the last of its periods
// before onsetFromYear (or before its UNTIL) instead of at DTSTART, which
// yields the same onsets from then on; one with COUNT starts at DTSTART.
func ruleOnsets(start time.Time, opt *rrule.ROption, budget *int64) (out []time.Time, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	end := time.Date(maxRuleYear+1, 1, 1, 0, 0, 0, 0, time.UTC)
	until := end
	if !opt.Until.IsZero() && opt.Until.Before(until) {
		until = opt.Until
	}
	if !start.Before(end) || until.Before(start) {
		return nil, true // no onsets in the years zone rules are read for
	}
	fillDefaults(opt, start)
	dtstart := start
	if opt.Count == 0 {
		dtstart = anchor(opt, start, min(onsetFromYear, until.Year()-1))
	}
	opt.Dtstart = dtstart.AddDate(shiftYears, 0, 0)
	opt.Until = until.AddDate(shiftYears, 0, 0)
	limit := time.Date(rrule.MAXYEAR+1, 1, 1, 0, 0, 0, 0, time.UTC)
	c := costOf(opt)
	w := c.work(periodsBetween(opt, opt.Dtstart, opt.Until), periodsBetween(opt, opt.Until, limit)+1)
	if w > *budget {
		return nil, false
	}
	*budget -= w
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, false
	}
	next := r.Iterator()
	for t, more := next(); more; t, more = next() {
		out = append(out, t.AddDate(-shiftYears, 0, 0))
	}
	return out, true
}

// fillDefaults writes out the day and time a YEARLY or MONTHLY rule takes
// from DTSTART, as rrule-go would, so DTSTART can move.
func fillDefaults(opt *rrule.ROption, start time.Time) {
	if len(opt.Byweekno)+len(opt.Byyearday)+len(opt.Bymonthday)+len(opt.Byweekday) == 0 {
		if opt.Freq == rrule.YEARLY && len(opt.Bymonth) == 0 {
			opt.Bymonth = []int{int(start.Month())}
		}
		opt.Bymonthday = []int{start.Day()}
	}
	if len(opt.Byhour) == 0 {
		opt.Byhour = []int{start.Hour()}
	}
	if len(opt.Byminute) == 0 {
		opt.Byminute = []int{start.Minute()}
	}
	if len(opt.Bysecond) == 0 {
		opt.Bysecond = []int{start.Second()}
	}
}

// anchor returns the start of the rule's last period that begins in a year
// before target, or start when that is start's own period.
func anchor(opt *rrule.ROption, start time.Time, target int) time.Time {
	k := max(opt.Interval, 1)
	switch opt.Freq {
	case rrule.YEARLY:
		if n := (target - start.Year()) / k; n > 0 {
			return time.Date(start.Year()+n*k, 1, 1, 0, 0, 0, 0, time.UTC)
		}
	case rrule.MONTHLY:
		m0 := start.Year()*12 + int(start.Month()) - 1
		if n := (target*12 - m0) / k; n > 0 {
			m := m0 + n*k
			return time.Date(m/12, time.Month(m%12+1), 1, 0, 0, 0, 0, time.UTC)
		}
	}
	return start
}

// offset returns the UTC offset in force at a local wall time: TZOFFSETTO
// of the observance with the latest onset at or before it, or TZOFFSETFROM
// of the earliest observance when the time precedes every onset.
func (v *vtimezone) offset(local time.Time) time.Duration {
	// A time outside the years zone rules mean anything is read with the
	// rules of the nearest such year, which also bounds the years cached.
	if y := local.Year(); y < minRuleYear {
		local = local.AddDate(minRuleYear-y, 0, 0)
	} else if y > maxRuleYear {
		local = local.AddDate(maxRuleYear-y, 0, 0)
	}
	ts := v.transitions(local.Year())
	if i := sort.Search(len(ts), func(i int) bool { return ts[i].at.After(local) }) - 1; i >= 0 {
		// Of onsets at the same time, the first observance's wins.
		for i > 0 && ts[i-1].at.Equal(ts[i].at) {
			i--
		}
		return ts[i].to
	}
	var first *observance
	for i := range v.obs {
		if first == nil || v.obs[i].start.Before(first.start) {
			first = &v.obs[i]
		}
	}
	if first == nil {
		return 0
	}
	return first.from
}

// wallOf returns the local wall time, held as a UTC value, whose instant
// is u: the earliest wall time w with w - offset(w) = u. offset reads a
// wall time in the hour a DST change repeats with the earlier offset, so
// an instant in the repeat's second pass has no such w, and maps to
// u + offset(u).
func (v *vtimezone) wallOf(u time.Time) time.Time {
	for _, off := range v.offs {
		if w := u.Add(off); v.offset(w) == off {
			return w
		}
	}
	return u.Add(v.offset(u))
}

// lastOnset returns the observance's last onset at or before local, and
// false when it starts after local.
func (o observance) lastOnset(local time.Time) (time.Time, bool) {
	if o.start.After(local) {
		return time.Time{}, false
	}
	last := o.start
	for _, ts := range [][]time.Time{o.onsets, o.rdates} {
		if i := sort.Search(len(ts), func(i int) bool { return ts[i].After(local) }) - 1; i >= 0 && ts[i].After(last) {
			last = ts[i]
		}
	}
	return last, true
}

// within returns the times of sorted strictly between from and to.
func within(sorted []time.Time, from, to time.Time) []time.Time {
	lo := sort.Search(len(sorted), func(i int) bool { return sorted[i].After(from) })
	hi := sort.Search(len(sorted), func(i int) bool { return !sorted[i].Before(to) })
	if lo >= hi {
		return nil
	}
	return sorted[lo:hi]
}

// transitions returns the onsets that can be in force during year y, sorted
// by time: each observance's last onset at or before the start of y, and
// its onsets within y. They are computed once per year.
func (v *vtimezone) transitions(y int) []transition {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ts, ok := v.years[y]; ok {
		return ts
	}
	from := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(1, 0, 0)
	var ts []transition
	for _, o := range v.obs {
		if t, ok := o.lastOnset(from); ok {
			ts = append(ts, transition{at: t, to: o.to})
		}
		if o.start.After(from) && o.start.Before(to) {
			ts = append(ts, transition{at: o.start, to: o.to})
		}
		for _, list := range [][]time.Time{o.onsets, o.rdates} {
			for _, t := range within(list, from, to) {
				ts = append(ts, transition{at: t, to: o.to})
			}
		}
	}
	slices.SortStableFunc(ts, func(a, b transition) int { return a.at.Compare(b.at) })
	v.years[y] = ts
	return ts
}

// naiveTime reads a local DATE-TIME as a UTC value holding the wall time.
func naiveTime(s string) (time.Time, bool) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "Z")
	t, err := time.ParseInLocation(localLayout, s, time.UTC)
	return t, err == nil
}

// parseOffset reads a UTC-OFFSET value: +hhmm, -hhmm, +hhmmss or -hhmmss.
func parseOffset(p *ical.Prop) (time.Duration, bool) {
	if p == nil {
		return 0, false
	}
	s := strings.TrimSpace(p.Value)
	if len(s) != 5 && len(s) != 7 {
		return 0, false
	}
	sign := time.Duration(1)
	switch s[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, false
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil {
		return 0, false
	}
	secs := 0
	if len(s) == 7 {
		secs, n = n%100, n/100
	}
	hours, mins := n/100, n%100
	if mins > 59 || secs > 59 {
		return 0, false
	}
	return sign * (time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute + time.Duration(secs)*time.Second), true
}

// hasControl reports whether s holds a control character.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return true
		}
	}
	return false
}

// maxRawICS bounds the iCalendar data embeddedZone reads: ParseInvite
// parses up to three times MaxICSBytes after charset conversion.
const maxRawICS = 3 * MaxICSBytes

// embeddedZone returns the VTIMEZONE the TZID-qualified DTSTART of master
// ev names in ev.Raw, or nil when Raw is not an iCalendar object, holds no
// VTIMEZONE, or the master's DTSTART names none of them. The last few
// results are remembered by Raw's backing array, so the series of one
// parsed object, which share Raw, decode it once.
func embeddedZone(ev Event) *vtimezone {
	raw := ev.Raw
	if len(raw) == 0 || len(raw) > maxRawICS {
		return nil
	}
	if v, ok := zoneMemo.get(raw, ev.UID); ok {
		return v
	}
	v := readEmbeddedZone(raw, ev.UID)
	zoneMemo.put(raw, ev.UID, v)
	return v
}

func readEmbeddedZone(raw []byte, uid string) *vtimezone {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !hasPrefixFold(bytes.TrimLeft(raw, " \t\r\n"), "BEGIN:VCALENDAR") ||
		!bytes.Contains(bytes.ToUpper(raw), []byte("BEGIN:VTIMEZONE")) {
		return nil
	}
	cal, err := decodeCalendar(raw)
	if err != nil {
		return nil
	}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent || child.Props.Get(ical.PropRecurrenceID) != nil ||
			strings.TrimSpace(textProp(child.Props, ical.PropUID)) != uid {
			continue
		}
		p := child.Props.Get(ical.PropDateTimeStart)
		if p == nil {
			return nil
		}
		// parseTime ignores TZID on a UTC time, and DATE values are
		// all-day.
		value := strings.TrimSpace(p.Value)
		id := p.Params.Get(ical.ParamTimezoneID)
		if id == "" || len(value) != len(localLayout) || strings.EqualFold(p.Params.Get(ical.ParamValue), string(ical.ValueDate)) {
			return nil
		}
		return newZones(cal.Component).resolve(id).vtz
	}
	return nil
}

func hasPrefixFold(s []byte, prefix string) bool {
	return len(s) >= len(prefix) && bytes.EqualFold(s[:len(prefix)], []byte(prefix))
}

// zoneMemo remembers the last few embeddedZone results, keyed by the
// backing array of Raw and the UID. Holding the slice keeps the array
// alive, so its address cannot be reused by other data while remembered.
// Raw is never changed in place.
var zoneMemo memo

type memo struct {
	mu      sync.Mutex
	entries [8]memoEntry
	next    int
}

type memoEntry struct {
	raw []byte
	uid string
	vtz *vtimezone
}

func (m *memo) get(raw []byte, uid string) (*vtimezone, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if len(e.raw) == len(raw) && len(raw) > 0 && &e.raw[0] == &raw[0] && e.uid == uid {
			return e.vtz, true
		}
	}
	return nil, false
}

func (m *memo) put(raw []byte, uid string, v *vtimezone) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[m.next] = memoEntry{raw: raw, uid: uid, vtz: v}
	m.next = (m.next + 1) % len(m.entries)
}
