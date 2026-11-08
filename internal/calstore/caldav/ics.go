package caldav

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"

	"github.com/yaklabco/chainmail/internal/calendar"
)

const (
	prodID = "-//chainmail//EN"

	paramScheduleAgent = "SCHEDULE-AGENT"
	propComment        = "COMMENT"
)

// object is one stored calendar object: the decoded VCALENDAR and the
// events calendar.ParseICS reads from it, index for index with the VEVENT
// components.
type object struct {
	cal    *ical.Calendar
	comps  []*ical.Component // the VEVENT children, in order
	events []calendar.Event
}

// decodeObject parses data with calendar.ParseICS, which bounds its size
// and parameter lengths and refuses malformed input, and then with go-ical
// for patching. It is the only way server bytes reach go-ical. A stored
// object is UTF-8 (RFC 5545 section 3.1.4): one that is not is refused, so
// no octets that are not UTF-8 reach the encoder, and a patched object
// keeps its UID exactly.
func decodeObject(data []byte) (*object, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: not UTF-8", calendar.ErrInvalidICS)
	}
	_, events, err := calendar.ParseICS(data)
	if err != nil {
		return nil, err
	}
	cal, err := decodeAccepted(data)
	if err != nil {
		return nil, err
	}
	o := &object{cal: cal, events: events}
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			o.comps = append(o.comps, c)
		}
	}
	if len(o.comps) != len(o.events) {
		return nil, fmt.Errorf("%w: events do not match", calendar.ErrInvalidICS)
	}
	return o, nil
}

// decodeAccepted runs go-ical's decoder on data calendar.ParseICS has
// already accepted. decodeObject is its only caller. It recovers from a
// panic anyway, since the decoder panics on some malformed parameter lists.
func decodeAccepted(data []byte) (cal *ical.Calendar, err error) {
	defer func() {
		if recover() != nil {
			cal, err = nil, fmt.Errorf("%w: malformed content line", calendar.ErrInvalidICS)
		}
	}()
	cal, err = ical.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))).Decode()
	if err != nil {
		return nil, fmt.Errorf("%w: malformed calendar", calendar.ErrInvalidICS)
	}
	return cal, nil
}

// find returns the index of the VEVENT with the given recurrence ID, or -1.
func (o *object) find(rid time.Time) int {
	for i, ev := range o.events {
		if ev.RecurrenceID.Equal(rid) {
			return i
		}
	}
	return -1
}

// encodeObject writes the calendar, folded, with PRODID and VERSION set
// when missing and no METHOD, which calendar collections refuse.
func encodeObject(cal *ical.Calendar) ([]byte, error) {
	if cal.Props.Get(ical.PropProductID) == nil {
		cal.Props.SetText(ical.PropProductID, prodID)
	}
	if cal.Props.Get(ical.PropVersion) == nil {
		cal.Props.SetText(ical.PropVersion, "2.0")
	}
	cal.Props.Del(ical.PropMethod)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("%w: cannot encode", calendar.ErrInvalidICS)
	}
	return calendar.Fold(buf.Bytes(), calendar.MaxICSBytes)
}

// newObject encodes one event as a new calendar object.
func newObject(ev calendar.Event) (*object, error) {
	data, err := calendar.EncodeICS("", []calendar.Event{ev})
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

// eventComponent encodes ev and returns its VEVENT.
func eventComponent(ev calendar.Event) (*ical.Component, error) {
	o, err := newObject(ev)
	if err != nil {
		return nil, err
	}
	if len(o.comps) != 1 {
		return nil, fmt.Errorf("%w: no event", calendar.ErrInvalidICS)
	}
	return o.comps[0], nil
}

// patch applies the fields of ev that differ from old to comp, the stored
// VEVENT old was read from. Properties chainmail does not model, and the
// parameters of unchanged properties, are kept. A change to the start, the
// end, the recurrence or the status raises SEQUENCE past the stored one
// unless ev already does. Times are written in the zones zs of the object
// comp belongs to.
func patch(comp *ical.Component, old, ev calendar.Event, now time.Time, zs *calendar.Zones) error {
	nc, err := eventComponent(ev)
	if err != nil {
		return err
	}
	replace := func(names ...string) {
		for _, n := range names {
			comp.Props.Del(n)
			if vs := nc.Props.Values(n); len(vs) > 0 {
				comp.Props[n] = slices.Clone(vs)
			}
		}
	}
	timeChanged := !old.Start.Equal(ev.Start) || old.TZID != ev.TZID || old.AllDay != ev.AllDay
	if timeChanged {
		replace(ical.PropDateTimeStart)
		rezone(comp.Props[ical.PropDateTimeStart], []time.Time{ev.Start}, zs)
	}
	endChanged := !old.End.Equal(ev.End)
	if timeChanged || endChanged {
		comp.Props.Del(ical.PropDuration)
		replace(ical.PropDateTimeEnd)
		rezone(comp.Props[ical.PropDateTimeEnd], []time.Time{ev.End}, zs)
	}
	if old.Summary != ev.Summary {
		replace(ical.PropSummary)
	}
	if old.Location != ev.Location {
		replace(ical.PropLocation)
	}
	if old.Description != ev.Description {
		replace(ical.PropDescription)
	}
	if old.Status != ev.Status {
		replace(ical.PropStatus)
	}
	if old.Transparent != ev.Transparent {
		replace(ical.PropTransparency)
	}
	if !samePerson(old.Organizer, ev.Organizer) {
		replace(ical.PropOrganizer)
	}
	if !sameAttendees(old.Attendees, ev.Attendees) {
		mergeAttendees(comp, nc, ev.Attendees, old.AttendeesTruncated)
	}
	recurChanged := old.RRule != ev.RRule
	if recurChanged {
		replace(ical.PropRecurrenceRule)
	}
	if !sameTimes(old.RDates, ev.RDates) || (timeChanged && len(ev.RDates) > 0) {
		recurChanged = true
		replace(ical.PropRecurrenceDates)
		rezone(comp.Props[ical.PropRecurrenceDates], ev.RDates, zs)
	}
	if !sameTimes(old.ExDates, ev.ExDates) || (timeChanged && len(ev.ExDates) > 0) {
		recurChanged = true
		replace(ical.PropExceptionDates)
		rezone(comp.Props[ical.PropExceptionDates], ev.ExDates, zs)
	}
	seq := ev.Sequence
	// Changes to DTSTART, DTEND, DURATION, the recurrence or STATUS need a
	// new SEQUENCE for attendees to accept them (RFC 5546 section 2.1.4,
	// RFC 5545 section 3.8.7.4).
	if (timeChanged || endChanged || recurChanged || old.Status != ev.Status) && seq <= old.Sequence {
		seq = old.Sequence + 1
	}
	if seq != old.Sequence || comp.Props.Get(ical.PropSequence) == nil {
		setRaw(comp.Props, ical.PropSequence, strconv.Itoa(seq))
	}
	stamp(comp, now)
	return nil
}

// stamp sets DTSTAMP and LAST-MODIFIED to now.
func stamp(comp *ical.Component, now time.Time) {
	comp.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())
	comp.Props.SetDateTime(ical.PropLastModified, now.UTC())
}

// mergeAttendees rebuilds the ATTENDEE list as want. An attendee who was
// already there keeps its stored property, with the parameters chainmail
// does not model, such as SCHEDULE-STATUS and CUTYPE, and gets want's CN,
// PARTSTAT, ROLE and RSVP. When the stored list was longer than
// calendar.MaxAttendees, the attendees past the limit, which want cannot
// name, are kept.
func mergeAttendees(comp, nc *ical.Component, want []calendar.Attendee, truncated bool) {
	stored := comp.Props.Values(ical.PropAttendee)
	byEmail := map[string]ical.Prop{}
	for i, p := range stored {
		if truncated && i >= calendar.MaxAttendees {
			break
		}
		if k := propEmail(&p); k != "" {
			if _, dup := byEmail[k]; !dup {
				byEmail[k] = p
			}
		}
	}
	fresh := map[string]ical.Prop{}
	for _, p := range nc.Props.Values(ical.PropAttendee) {
		fresh[propEmail(&p)] = p
	}
	var out []ical.Prop
	for _, a := range want {
		k := strings.ToLower(a.Email)
		p, ok := byEmail[k]
		if !ok {
			if p, ok = fresh[k]; ok {
				out = append(out, p)
			}
			continue
		}
		p = cloneProp(p)
		f := fresh[k]
		for _, name := range []string{ical.ParamCommonName, ical.ParamParticipationStatus, ical.ParamRole, ical.ParamRSVP} {
			if v := f.Params.Get(name); v != "" {
				p.Params.Set(name, v)
			} else {
				p.Params.Del(name)
			}
		}
		out = append(out, p)
	}
	if truncated && len(stored) > calendar.MaxAttendees {
		out = append(out, stored[calendar.MaxAttendees:]...)
	}
	comp.Props.Del(ical.PropAttendee)
	if len(out) > 0 {
		comp.Props[ical.PropAttendee] = out
	}
}

func cloneProp(p ical.Prop) ical.Prop {
	params := make(ical.Params, len(p.Params))
	for k, v := range p.Params {
		params[k] = slices.Clone(v)
	}
	return ical.Prop{Name: p.Name, Params: params, Value: p.Value}
}

// propEmail is the lowercased address of an ORGANIZER or ATTENDEE, read as
// calendar.ParseICS reads it.
func propEmail(p *ical.Prop) string {
	v := strings.TrimSpace(p.Value)
	switch {
	case len(v) >= 7 && strings.EqualFold(v[:7], "mailto:"):
		v = v[7:]
	case p.Params.Get(ical.ParamEmail) != "":
		v = p.Params.Get(ical.ParamEmail)
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func samePerson(a, b *calendar.Person) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return strings.EqualFold(a.Email, b.Email) && a.Name == b.Name
}

func sameAttendees(a, b []calendar.Attendee) bool {
	return slices.EqualFunc(a, b, func(x, y calendar.Attendee) bool {
		return strings.EqualFold(x.Email, y.Email) && x.Name == y.Name &&
			x.PartStat == y.PartStat && x.Role == y.Role && x.RSVP == y.RSVP
	})
}

func sameTimes(a, b []time.Time) bool {
	return slices.EqualFunc(a, b, func(x, y time.Time) bool { return x.Equal(y) })
}

// scheduleAgentClient sets SCHEDULE-AGENT=CLIENT on every ATTENDEE of
// every event, so the server sends no scheduling messages for the object
// (RFC 6638 section 7.1).
func scheduleAgentClient(cal *ical.Calendar) {
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		atts := c.Props.Values(ical.PropAttendee)
		for i := range atts {
			if atts[i].Params == nil {
				atts[i].Params = ical.Params{}
			}
			atts[i].Params.Set(paramScheduleAgent, "CLIENT")
		}
	}
}

// serverScheduled reports whether the server may schedule any attendee of
// cal other than the addresses in self: one whose SCHEDULE-AGENT is not
// CLIENT or NONE. Absent means SERVER (RFC 6638 section 7.1), and an
// unknown value is taken as SERVER too, since refusing is the safe side.
func serverScheduled(cal *ical.Calendar, self []string) bool {
	isSelf := func(addr string) bool {
		return slices.ContainsFunc(self, func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), addr) })
	}
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		for _, a := range c.Props.Values(ical.PropAttendee) {
			if isSelf(propEmail(&a)) {
				continue
			}
			switch strings.ToUpper(strings.TrimSpace(a.Params.Get(paramScheduleAgent))) {
			case "CLIENT", "NONE":
			default:
				return true
			}
		}
	}
	return false
}

// setPartStat sets self's PARTSTAT on comp and reports whether self is an
// attendee there.
func setPartStat(comp *ical.Component, self string, ps calendar.PartStat) bool {
	self = strings.ToLower(strings.TrimSpace(self))
	atts := comp.Props.Values(ical.PropAttendee)
	found := false
	for i := range atts {
		if propEmail(&atts[i]) != self {
			continue
		}
		if atts[i].Params == nil {
			atts[i].Params = ical.Params{}
		}
		atts[i].Params.Set(ical.ParamParticipationStatus, string(ps))
		found = true
	}
	return found
}

// addComment adds a COMMENT with text, cleaned and escaped.
func addComment(comp *ical.Component, text string) {
	text = cleanText(text)
	if text == "" {
		return
	}
	p := ical.NewProp(propComment)
	p.SetText(text)
	comp.Props.Add(p)
}

// overrideFrom builds the override of master for the instance at rid: a
// copy of master without its recurrence, starting at rid with master's
// duration, with RECURRENCE-ID and DTSTART in master's DTSTART form,
// read in the object's zones zs.
func overrideFrom(master *ical.Component, mev calendar.Event, rid time.Time, zs *calendar.Zones) *ical.Component {
	o := ical.NewComponent(ical.CompEvent)
	for name, props := range master.Props {
		switch name {
		case ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates,
			"EXRULE", ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropDuration, ical.PropRecurrenceID:
			continue
		}
		cp := make([]ical.Prop, len(props))
		for i, p := range props {
			cp[i] = cloneProp(p)
		}
		o.Props[name] = cp
	}
	for _, child := range master.Children {
		o.Children = append(o.Children, cloneComponent(child))
	}
	like := master.Props.Get(ical.PropDateTimeStart)
	o.Props.Set(timeLike(ical.PropRecurrenceID, like, rid, mev.TZID, zs))
	o.Props.Set(timeLike(ical.PropDateTimeStart, like, rid, mev.TZID, zs))
	end := rid.Add(mev.End.Sub(mev.Start))
	endLike := master.Props.Get(ical.PropDateTimeEnd)
	if endLike == nil {
		endLike = like
	}
	o.Props.Set(timeLike(ical.PropDateTimeEnd, endLike, end, mev.TZID, zs))
	return o
}

func cloneComponent(c *ical.Component) *ical.Component {
	out := ical.NewComponent(c.Name)
	for name, props := range c.Props {
		cp := make([]ical.Prop, len(props))
		for i, p := range props {
			cp[i] = cloneProp(p)
		}
		out.Props[name] = cp
	}
	for _, child := range c.Children {
		out.Children = append(out.Children, cloneComponent(child))
	}
	return out
}

// timeLike writes t in the form of like: a DATE, a TZID-qualified local
// time, UTC or floating. A TZID-qualified time is written under like's
// TZID in the zone zs reads that TZID with (the object's own VTIMEZONE, or
// the IANA zone it names), so it reads back as t. A TZID zs cannot read is
// evaluated in fallback, the IANA zone calendar.ParseICS resolved it to;
// with neither the time is written in UTC.
func timeLike(name string, like *ical.Prop, t time.Time, fallback string, zs *calendar.Zones) *ical.Prop {
	p := ical.NewProp(name)
	if like == nil {
		p.Value = t.UTC().Format("20060102T150405Z")
		return p
	}
	v := strings.TrimSpace(like.Value)
	switch {
	case strings.EqualFold(like.Params.Get(ical.ParamValue), "DATE") || (len(v) == 8 && !strings.Contains(v, "T")):
		p.Params.Set(ical.ParamValue, "DATE")
		p.Value = t.UTC().Format("20060102")
	case like.Params.Get(ical.ParamTimezoneID) != "":
		tzid := like.Params.Get(ical.ParamTimezoneID)
		if w, ok := zs.Wall(tzid, t); ok {
			p.Params.Set(ical.ParamTimezoneID, tzid)
			p.Value = w.Format("20060102T150405")
			return p
		}
		if !loadable(fallback) {
			p.Value = t.UTC().Format("20060102T150405Z")
			return p
		}
		loc, err := time.LoadLocation(fallback)
		if err != nil {
			p.Value = t.UTC().Format("20060102T150405Z")
			return p
		}
		p.Params.Set(ical.ParamTimezoneID, tzid)
		p.Value = t.In(loc).Format("20060102T150405")
	case strings.HasSuffix(strings.ToUpper(v), "Z"):
		p.Value = t.UTC().Format("20060102T150405Z")
	default:
		p.Value = t.UTC().Format("20060102T150405")
	}
	return p
}

// rezone rewrites the TZID-qualified local times of props, the values of
// one property for the instants ts in order, in the zone zs reads their
// TZID with. EncodeICS writes them in Go's zone data for the TZID; the
// object may define that TZID otherwise in its own VTIMEZONE, which is
// what the server and every reader use.
func rezone(props []ical.Prop, ts []time.Time, zs *calendar.Zones) {
	for i := range props {
		p := &props[i]
		tzid := p.Params.Get(ical.ParamTimezoneID)
		if i >= len(ts) || tzid == "" || strings.EqualFold(p.Params.Get(ical.ParamValue), "DATE") {
			continue
		}
		if w, ok := zs.Wall(tzid, ts[i]); ok {
			p.Value = w.Format("20060102T150405")
		}
	}
}

// rezoneEvent rezones the date and time properties of comp, a VEVENT
// eventComponent encoded from ev.
func rezoneEvent(comp *ical.Component, ev calendar.Event, zs *calendar.Zones) {
	rezone(comp.Props[ical.PropDateTimeStart], []time.Time{ev.Start}, zs)
	rezone(comp.Props[ical.PropDateTimeEnd], []time.Time{ev.End}, zs)
	rezone(comp.Props[ical.PropRecurrenceID], []time.Time{ev.RecurrenceID}, zs)
	rezone(comp.Props[ical.PropRecurrenceDates], ev.RDates, zs)
	rezone(comp.Props[ical.PropExceptionDates], ev.ExDates, zs)
}

func setRaw(props ical.Props, name, value string) {
	p := ical.NewProp(name)
	p.Value = value
	props.Set(p)
}

// isInstance reports whether rid is an instance of the series in events
// (its master and overrides).
func isInstance(events []calendar.Event, rid time.Time) bool {
	insts, _, err := calendar.Expand(events, rid, rid.Add(time.Second), 1)
	if err != nil {
		return false
	}
	for _, in := range insts {
		if in.RecurrenceID.Equal(rid) || (in.RecurrenceID.IsZero() && in.Start.Equal(rid)) {
			return true
		}
	}
	// A moved override is found by its RecurrenceID, not its start.
	for _, ev := range events {
		if ev.RecurrenceID.Equal(rid) {
			return true
		}
	}
	return false
}

// span returns the first and last years the events of cal reach, for
// VTIMEZONE generation. open is true when a recurrence has no UNTIL.
func span(cal *ical.Calendar) (first, last int, open bool) {
	first, last = 0, 0
	note := func(v string) {
		for _, part := range strings.Split(v, ",") {
			part, _, _ = strings.Cut(strings.TrimSpace(part), "/")
			if len(part) < 4 {
				continue
			}
			y, err := strconv.Atoi(part[:4])
			if err != nil {
				continue
			}
			if first == 0 || y < first {
				first = y
			}
			if y > last {
				last = y
			}
		}
	}
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		for _, name := range []string{
			ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID,
			ical.PropRecurrenceDates, ical.PropExceptionDates,
		} {
			for _, p := range c.Props.Values(name) {
				note(p.Value)
			}
		}
		for _, p := range c.Props.Values(ical.PropRecurrenceRule) {
			opt, err := rrule.StrToROption(p.Value)
			switch {
			case err != nil:
				open = true
			case !opt.Until.IsZero():
				note(opt.Until.UTC().Format("20060102"))
			default:
				open = true
			}
		}
	}
	return first, last, open
}

// addTimezones adds a VTIMEZONE, generated from Go's zone data, for every
// TZID the events use that the calendar does not define and Go can load.
// VTIMEZONEs already in the calendar are kept as they are.
func addTimezones(cal *ical.Calendar, now time.Time) {
	defined := map[string]bool{}
	for _, c := range cal.Children {
		if c.Name == ical.CompTimezone {
			if p := c.Props.Get(ical.PropTimezoneID); p != nil {
				defined[strings.TrimSpace(p.Value)] = true
			}
		}
	}
	var need []string
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		for _, props := range c.Props {
			for _, p := range props {
				id := p.Params.Get(ical.ParamTimezoneID)
				if id != "" && !defined[id] && !slices.Contains(need, id) && loadable(id) {
					need = append(need, id)
				}
			}
		}
	}
	if len(need) == 0 {
		return
	}
	first, last, open := span(cal)
	if first == 0 {
		first = now.Year()
		last = first
	}
	var zones []*ical.Component
	for _, id := range need {
		loc, err := time.LoadLocation(id)
		if err != nil {
			continue
		}
		zones = append(zones, vtimezone(id, loc, first, last, open))
	}
	cal.Children = append(zones, cal.Children...)
}

// cleanText makes untrusted text valid UTF-8 without control characters
// other than newline and tab.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// cleanLine is cleanText for one-line values: line breaks and tabs become
// spaces, and surrounding space is trimmed.
func cleanLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, cleanText(s)))
}
