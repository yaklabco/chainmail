package calendar

import (
	"strconv"
	"strings"
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
	defs  map[string]*vtimezone // VTIMEZONE by TZID
	cache map[string]zone
}

// zone is a resolved TZID: an IANA zone (loc set, name its name or "" for
// UTC), rules from a VTIMEZONE (vtz set), or floating (neither).
type zone struct {
	loc  *time.Location
	name string
	vtz  *vtimezone
}

func newZones(cal *ical.Component) *zones {
	z := &zones{defs: map[string]*vtimezone{}, cache: map[string]zone{}}
	for _, child := range cal.Children {
		if child.Name != ical.CompTimezone {
			continue
		}
		if id := child.Props.Get(ical.PropTimezoneID); id != nil {
			z.defs[strings.TrimSpace(id.Value)] = parseVTimezone(child)
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

func (z *zones) lookup(tzid string) zone {
	if loc, name, ok := loadZone(tzid); ok {
		return zone{loc: loc, name: name}
	}
	if iana, ok := windowsZones[tzid]; ok {
		if loc, name, ok := loadZone(iana); ok {
			return zone{loc: loc, name: name}
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
				return zone{loc: loc, name: name}
			}
		}
	}
	if v := z.defs[tzid]; v != nil {
		return zone{vtz: v}
	}
	return zone{}
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
	case r.loc != nil:
		local := time.Date(naive.Year(), naive.Month(), naive.Day(), naive.Hour(), naive.Minute(), naive.Second(), 0, r.loc)
		return local.UTC(), r.name, false, true
	case r.vtz != nil:
		return naive.Add(-r.vtz.offset(naive)), "", false, true
	default:
		return naive, "", false, true
	}
}

// vtimezone holds the observances of a VTIMEZONE, for zones that are not
// IANA names.
type vtimezone struct {
	obs []observance
	// years caches, by year, the onsets that can be in force during it.
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
// evaluated for.
const (
	minRuleYear = 1970
	maxRuleYear = 2100
)

// observance is one STANDARD or DAYLIGHT block. Its onset times are local
// wall times held as UTC values, so they compare with other wall times.
type observance struct {
	start    time.Time
	from, to time.Duration
	rule     *rrule.ROption // DTSTART unset; ruleFrom sets it
	rdates   []time.Time
}

func parseVTimezone(comp *ical.Component) *vtimezone {
	v := &vtimezone{years: map[int][]transition{}}
	for _, child := range comp.Children {
		if child.Name != ical.CompTimezoneStandard && child.Name != ical.CompTimezoneDaylight {
			continue
		}
		if len(v.obs) == maxObservances {
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
			continue
		}
		o.from, o.to = from, to
		if p := child.Props.Get(ical.PropRecurrenceRule); p != nil {
			if opt, err := rrule.StrToROptionInLocation(p.Value, time.UTC); err == nil {
				// Without UNTIL, rrule-go stops 292 years after DTSTART, and
				// Windows zones start their rules in 1601.
				if opt.Until.IsZero() {
					opt.Until = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
				}
				o.rule = opt
			}
		}
		for _, p := range child.Props.Values(ical.PropRecurrenceDates) {
			for _, s := range strings.Split(p.Value, ",") {
				if t, ok := naiveTime(s); ok {
					o.rdates = append(o.rdates, t)
				}
			}
		}
		v.obs = append(v.obs, o)
	}
	return v
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
	var best time.Time
	var off time.Duration
	found := false
	for _, tr := range v.transitions(local.Year()) {
		if !tr.at.After(local) && (!found || tr.at.After(best)) {
			best, off, found = tr.at, tr.to, true
		}
	}
	if found {
		return off
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

// lastOnset returns the observance's last onset at or before local, and
// false when it starts after local.
func (o observance) lastOnset(local time.Time) (time.Time, bool) {
	if o.start.After(local) {
		return time.Time{}, false
	}
	last := o.start
	if t := o.ruleOnset(local); t.After(last) {
		last = t
	}
	for _, t := range o.rdates {
		if !t.After(local) && t.After(last) {
			last = t
		}
	}
	return last, true
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

// ruleOnset returns the last onset of the observance's RRULE at or before
// local, or the zero time.
func (o observance) ruleOnset(local time.Time) time.Time {
	r := o.ruleFrom(local.Year())
	if r == nil {
		return time.Time{}
	}
	return r.Before(local, true)
}

// ruleFrom returns the observance's RRULE for use in year y, or nil. A
// plain yearly rule, which is what VTIMEZONEs hold, is started two years
// before y instead of at DTSTART: the onsets it yields from then on are the
// same, and iterating from 1601 would be slow.
func (o observance) ruleFrom(y int) *rrule.RRule {
	if o.rule == nil {
		return nil
	}
	opt := *o.rule
	opt.Dtstart = o.start
	if opt.Freq == rrule.YEARLY && opt.Interval <= 1 && opt.Count == 0 && y-2 > o.start.Year() {
		s := o.start
		opt.Dtstart = time.Date(y-2, s.Month(), s.Day(), s.Hour(), s.Minute(), s.Second(), 0, time.UTC)
	}
	r, err := rrule.NewRRule(opt)
	if err != nil {
		return nil
	}
	return r
}

// transitions returns the onsets that can be in force during year y: each
// observance's last onset at or before the start of y, and its onsets
// within y. They are computed once per year, so a calendar with many times
// in one zone expands each rule once.
func (v *vtimezone) transitions(y int) []transition {
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
		if r := o.ruleFrom(y); r != nil {
			for _, t := range r.Between(from, to, false) {
				ts = append(ts, transition{at: t, to: o.to})
			}
		}
		for _, t := range o.rdates {
			if t.After(from) && t.Before(to) {
				ts = append(ts, transition{at: t, to: o.to})
			}
		}
	}
	v.years[y] = ts
	return ts
}
