package calendar

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

const (
	prodID = "-//chainmail//EN"
	// foldWidth is the longest content line RFC 5545 allows, in octets.
	foldWidth = 75
	// maxEncodedICS bounds what EncodeICS writes, in octets. Events parsed
	// from an invite can come from up to three times MaxICSBytes of text.
	maxEncodedICS = 3 * MaxICSBytes
)

// tokenRE matches parameter values chainmail writes unquoted, such as
// PARTSTAT and ROLE, and METHOD values.
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// parsedEvent is an Event plus the properties only mail invites need.
type parsedEvent struct {
	Event
	dtstamp time.Time
}

// ParseICS parses one VCALENDAR: its METHOD (may be "") and every VEVENT,
// with VTIMEZONE-aware times. Over MaxICSBytes, or more than
// MaxObjectEvents VEVENTs: ErrTooLarge. More than
// MaxAttendees attendees: the rest are dropped and AttendeesTruncated set.
// Each Event.Raw is the whole input.
//
// A TZID the calendar defines in a VTIMEZONE is read with those rules,
// which RFC 5545 makes authoritative. Event.TZID is still the IANA zone
// the TZID names (directly, through the Windows zone names Outlook writes,
// or through a prefixed IANA name), or "" when it names none. A TZID with
// no usable VTIMEZONE is read from that IANA zone. Floating times and
// zones with no definition read as UTC.
func ParseICS(data []byte) (method string, events []Event, err error) {
	method, parsed, err := parseICS(data, MaxICSBytes)
	if err != nil {
		return "", nil, err
	}
	events = make([]Event, len(parsed))
	for i := range parsed {
		events[i] = parsed[i].Event
	}
	return method, events, nil
}

func parseICS(data []byte, limit int) (string, []parsedEvent, error) {
	if len(data) > limit {
		return "", nil, ErrTooLarge
	}
	cal, err := decodeCalendar(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if err != nil {
		return "", nil, err
	}
	var method string
	if p := cal.Props.Get(ical.PropMethod); p != nil {
		method = strings.ToUpper(strings.TrimSpace(p.Value))
	}
	n := 0
	for _, child := range cal.Children {
		if child.Name == ical.CompEvent {
			n++
		}
	}
	if n > MaxObjectEvents {
		return "", nil, ErrTooLarge
	}
	z := newZones(cal.Component)
	raw := bytes.Clone(data)
	var events []parsedEvent
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		pe, err := parseEvent(child, z)
		if err != nil {
			return "", nil, err
		}
		pe.Raw = raw
		events = append(events, pe)
	}
	return method, events, nil
}

// maxParamValue bounds one parameter value, such as a CN, in octets.
// go-ical builds parameter values a byte at a time, in quadratic time, so
// a longer one is refused before decoding.
const maxParamValue = 2048

// paramState is where checkParams is in a content line.
type paramState int

const (
	inName      paramState = iota // the property name
	inParamName                   // a parameter name, up to its '='
	atValue                       // the first byte of a parameter value
	inQuoted                      // a quoted parameter value
	inUnquoted                    // an unquoted parameter value
	afterQuote                    // just after a quoted value's closing quote
	inValue                       // the property value
)

// checkParams refuses a content line with a parameter value longer than
// maxParamValue. It follows go-ical's decoder: lines are unfolded, the
// property name ends at the first semicolon or colon, a parameter name at
// the next equals sign, a quoted value at its closing quote, and an
// unquoted value at a semicolon, comma or colon. An equals sign inside a
// value is part of the value. A comma starts the next value of the same
// parameter, a semicolon the next parameter, and a colon the property
// value, which is not bounded.
func checkParams(data []byte) error {
	state, n := inName, 0
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			continue // the line break is handled at its '\n'
		}
		if c == '\n' {
			if i+1 < len(data) && (data[i+1] == ' ' || data[i+1] == '\t') {
				i++ // a folded line continues the logical line
				continue
			}
			state, n = inName, 0
			continue
		}
		switch state {
		case inName:
			switch c {
			case ';':
				state = inParamName
			case ':':
				state = inValue
			}
		case inParamName:
			if c == '=' {
				state, n = atValue, 0
			}
		case atValue, inUnquoted, afterQuote:
			switch {
			case c == ',':
				state, n = atValue, 0
			case c == ';':
				state = inParamName
			case c == ':':
				state = inValue
			case state == atValue && c == '"':
				state = inQuoted
			case state == afterQuote:
				state = inValue // the decoder refuses the line
			default:
				state = inUnquoted
				n++
			}
		case inQuoted:
			if c == '"' {
				state = afterQuote
			} else {
				n++
			}
		}
		if n > maxParamValue {
			return fmt.Errorf("%w: parameter value too long", ErrInvalidICS)
		}
	}
	return nil
}

// decodeCalendar runs go-ical's decoder, which panics on some malformed
// parameter lists, and reports every failure as ErrInvalidICS without its
// text, which can quote the input.
func decodeCalendar(data []byte) (cal *ical.Calendar, err error) {
	if err := checkParams(data); err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			cal, err = nil, fmt.Errorf("%w: malformed content line", ErrInvalidICS)
		}
	}()
	cal, err = ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("%w: malformed calendar", ErrInvalidICS)
	}
	return cal, nil
}

func parseEvent(comp *ical.Component, z *zones) (parsedEvent, error) {
	var pe parsedEvent
	ev := &pe.Event
	props := comp.Props

	ev.UID = strings.TrimSpace(textProp(props, ical.PropUID))
	ev.Summary = textProp(props, ical.PropSummary)
	ev.Location = textProp(props, ical.PropLocation)
	ev.Description = textProp(props, ical.PropDescription)

	var startDate bool
	if p := props.Get(ical.PropDateTimeStart); p != nil {
		t, tzid, date, ok := z.parseTime(p.Value, p.Params)
		if !ok {
			return pe, fmt.Errorf("%w: bad DTSTART", ErrInvalidICS)
		}
		ev.Start, ev.TZID, ev.AllDay, startDate = t, tzid, date, date
	}
	switch {
	case props.Get(ical.PropDateTimeEnd) != nil:
		p := props.Get(ical.PropDateTimeEnd)
		t, _, _, ok := z.parseTime(p.Value, p.Params)
		if !ok {
			return pe, fmt.Errorf("%w: bad DTEND", ErrInvalidICS)
		}
		ev.End = t
	case props.Get(ical.PropDuration) != nil:
		d, err := props.Get(ical.PropDuration).Duration()
		if err != nil || d < 0 {
			return pe, fmt.Errorf("%w: bad DURATION", ErrInvalidICS)
		}
		ev.End = ev.Start.Add(d)
	case startDate:
		ev.End = ev.Start.AddDate(0, 0, 1)
	default:
		ev.End = ev.Start
	}
	if p := props.Get(ical.PropRecurrenceID); p != nil {
		t, _, _, ok := z.parseTime(p.Value, p.Params)
		if !ok {
			return pe, fmt.Errorf("%w: bad RECURRENCE-ID", ErrInvalidICS)
		}
		ev.RecurrenceID = t
	}

	if p := props.Get(ical.PropStatus); p != nil {
		switch s := Status(strings.ToUpper(strings.TrimSpace(p.Value))); s {
		case StatusConfirmed, StatusTentative, StatusCancelled:
			ev.Status = s
		}
	}
	if p := props.Get(ical.PropSequence); p != nil {
		n, err := strconv.Atoi(strings.TrimSpace(p.Value))
		if err != nil || n < 0 {
			return pe, fmt.Errorf("%w: bad SEQUENCE", ErrInvalidICS)
		}
		ev.Sequence = n
	}
	if p := props.Get(ical.PropTransparency); p != nil {
		ev.Transparent = strings.EqualFold(strings.TrimSpace(p.Value), "TRANSPARENT")
	}
	if p := props.Get(ical.PropOrganizer); p != nil {
		org := parsePerson(p)
		ev.Organizer = &org
	}
	for _, p := range props.Values(ical.PropAttendee) {
		if len(ev.Attendees) == MaxAttendees {
			ev.AttendeesTruncated = true
			break
		}
		a := Attendee{
			Person:   parsePerson(&p),
			PartStat: PartStat(strings.ToUpper(strings.TrimSpace(p.Params.Get(ical.ParamParticipationStatus)))),
			Role:     strings.ToUpper(strings.TrimSpace(p.Params.Get(ical.ParamRole))),
			RSVP:     strings.EqualFold(strings.TrimSpace(p.Params.Get(ical.ParamRSVP)), "TRUE"),
		}
		if a.PartStat == "" || !tokenRE.MatchString(string(a.PartStat)) {
			a.PartStat = PartStatNeedsAction
		}
		if !tokenRE.MatchString(a.Role) {
			a.Role = ""
		}
		ev.Attendees = append(ev.Attendees, a)
	}
	if p := props.Get(ical.PropRecurrenceRule); p != nil {
		ev.RRule = strings.TrimPrefix(strings.TrimSpace(p.Value), "RRULE:")
	}
	var err error
	if ev.RDates, err = dateList(props.Values(ical.PropRecurrenceDates), z); err != nil {
		return pe, fmt.Errorf("%w: bad RDATE", ErrInvalidICS)
	}
	if ev.ExDates, err = dateList(props.Values(ical.PropExceptionDates), z); err != nil {
		return pe, fmt.Errorf("%w: bad EXDATE", ErrInvalidICS)
	}
	if p := props.Get(ical.PropDateTimeStamp); p != nil {
		if t, _, _, ok := z.parseTime(p.Value, p.Params); ok {
			pe.dtstamp = t
		}
	}
	ev.Updated = pe.dtstamp
	if p := props.Get(ical.PropLastModified); p != nil {
		if t, _, _, ok := z.parseTime(p.Value, p.Params); ok {
			ev.Updated = t
		}
	}
	return pe, nil
}

// dateList reads RDATE or EXDATE properties, each of which may hold a
// comma-separated list. A PERIOD contributes its start.
func dateList(props []ical.Prop, z *zones) ([]time.Time, error) {
	var out []time.Time
	for _, p := range props {
		params := p.Params
		if strings.EqualFold(params.Get(ical.ParamValue), string(ical.ValuePeriod)) {
			params = ical.Params{}
			for k, v := range p.Params {
				if k != ical.ParamValue {
					params[k] = v
				}
			}
		}
		for _, v := range strings.Split(p.Value, ",") {
			v, _, _ = strings.Cut(v, "/")
			t, _, _, ok := z.parseTime(v, params)
			if !ok {
				return nil, ErrInvalidICS
			}
			out = append(out, t)
		}
	}
	return out, nil
}

func parsePerson(p *ical.Prop) Person {
	v := strings.TrimSpace(p.Value)
	var email string
	switch {
	case len(v) >= 7 && strings.EqualFold(v[:7], "mailto:"):
		email = v[7:]
	case p.Params.Get(ical.ParamEmail) != "":
		email = p.Params.Get(ical.ParamEmail)
	default:
		email = v
	}
	return Person{
		Email: cleanLine(strings.TrimSpace(email)),
		Name:  cleanLine(strings.TrimSpace(p.Params.Get(ical.ParamCommonName))),
	}
}

func textProp(props ical.Props, name string) string {
	p := props.Get(name)
	if p == nil {
		return ""
	}
	return cleanText(unescapeText(p.Value))
}

// unescapeText decodes an RFC 5545 TEXT value. Unknown escapes keep the
// escaped character, and unescaped commas are kept, since real producers
// emit both.
func unescapeText(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i == len(v) {
			break
		}
		switch v[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// cleanText makes untrusted multi-line text safe to store and render:
// valid UTF-8, no control characters other than newline and tab.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if !hasControl(s) {
		return s
	}
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

// cleanLine is cleanText for single-line values such as names: line breaks
// and tabs become spaces.
func cleanLine(s string) string {
	s = cleanText(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// EncodeICS writes one VCALENDAR (PRODID "-//chainmail//EN", VERSION 2.0,
// METHOD when non-empty) with the given events. A timed event whose TZID is
// a loadable IANA zone is written with TZID-qualified local times and no
// VTIMEZONE, which Google, Apple and CalDAV servers accept for IANA names;
// any other timed event is written in UTC. All-day events are written as
// DATE values. Lines are folded at 75 octets.
//
// DTSTAMP is Updated, or the Unix epoch when Updated is zero, so the output
// depends only on the events. iTIP orders messages of one SEQUENCE by
// DTSTAMP, so a caller sending a REQUEST, UPDATE or CANCEL sets Updated or
// uses EncodeICSAt with the current time.
//
// Text is escaped and stripped of control characters; values that cannot
// be escaped (UID, RRULE, addresses, PARTSTAT, ROLE) are refused with
// ErrInvalidICS when they hold a line break or control character, so no
// field can inject a property. ProviderID, ETag, Raw and AttendeesTruncated
// are not written. Output over three times MaxICSBytes is refused with
// ErrTooLarge.
func EncodeICS(method string, events []Event) ([]byte, error) {
	return EncodeICSAt(method, events, time.Unix(0, 0))
}

// EncodeICSAt is EncodeICS with now as the DTSTAMP of each event whose
// Updated is zero.
func EncodeICSAt(method string, events []Event, now time.Time) ([]byte, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: no events", ErrInvalidICS)
	}
	cal, err := newCalendar(method)
	if err != nil {
		return nil, err
	}
	for i := range events {
		stamp := events[i].Updated
		if stamp.IsZero() {
			stamp = now
		}
		comp, err := encodeEvent(events[i], stamp)
		if err != nil {
			return nil, err
		}
		cal.Children = append(cal.Children, comp)
	}
	return encodeCalendar(cal)
}

func newCalendar(method string) (*ical.Calendar, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, prodID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	if method != "" {
		if !tokenRE.MatchString(method) {
			return nil, fmt.Errorf("%w: bad METHOD", ErrInvalidICS)
		}
		cal.Props.SetText(ical.PropMethod, strings.ToUpper(method))
	}
	return cal, nil
}

func encodeCalendar(cal *ical.Calendar) ([]byte, error) {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("%w: cannot encode", ErrInvalidICS)
	}
	return Fold(buf.Bytes(), maxEncodedICS)
}

// timeStyle writes the date and time properties of one event in one form.
type timeStyle struct {
	allDay bool
	loc    *time.Location // nil for UTC
}

func styleOf(ev Event) timeStyle {
	if ev.AllDay {
		return timeStyle{allDay: true}
	}
	if loc, name, ok := loadZone(ev.TZID); ok && name != "" {
		return timeStyle{loc: loc}
	}
	return timeStyle{}
}

func (s timeStyle) prop(name string, t time.Time) *ical.Prop {
	p := ical.NewProp(name)
	switch {
	case s.allDay:
		p.SetDate(t.UTC())
	case s.loc != nil:
		p.SetDateTime(t.In(s.loc))
	default:
		p.SetDateTime(t.UTC())
	}
	return p
}

func encodeEvent(ev Event, dtstamp time.Time) (*ical.Component, error) {
	if ev.UID == "" || hasControl(ev.UID) {
		return nil, fmt.Errorf("%w: bad UID", ErrInvalidICS)
	}
	comp := ical.NewComponent(ical.CompEvent)
	props := comp.Props
	style := styleOf(ev)

	uid := ical.NewProp(ical.PropUID)
	uid.Value = ev.UID
	props.Set(uid)
	props.SetDateTime(ical.PropDateTimeStamp, dtstamp.UTC())
	if !ev.Start.IsZero() {
		props.Set(style.prop(ical.PropDateTimeStart, ev.Start))
	}
	if !ev.End.IsZero() {
		props.Set(style.prop(ical.PropDateTimeEnd, ev.End))
	}
	if !ev.RecurrenceID.IsZero() {
		props.Set(style.prop(ical.PropRecurrenceID, ev.RecurrenceID))
	}
	setText(props, ical.PropSummary, ev.Summary)
	setText(props, ical.PropLocation, ev.Location)
	setText(props, ical.PropDescription, ev.Description)
	switch ev.Status {
	case "":
	case StatusConfirmed, StatusTentative, StatusCancelled:
		setRaw(props, ical.PropStatus, string(ev.Status))
	default:
		return nil, fmt.Errorf("%w: bad STATUS", ErrInvalidICS)
	}
	if ev.Sequence < 0 {
		return nil, fmt.Errorf("%w: bad SEQUENCE", ErrInvalidICS)
	}
	setRaw(props, ical.PropSequence, strconv.Itoa(ev.Sequence))
	if ev.Transparent {
		setRaw(props, ical.PropTransparency, "TRANSPARENT")
	}
	if ev.Organizer != nil {
		p, err := personProp(ical.PropOrganizer, *ev.Organizer)
		if err != nil {
			return nil, err
		}
		props.Add(p)
	}
	for _, a := range ev.Attendees {
		p, err := personProp(ical.PropAttendee, a.Person)
		if err != nil {
			return nil, err
		}
		if a.PartStat != "" {
			if !tokenRE.MatchString(string(a.PartStat)) {
				return nil, fmt.Errorf("%w: bad PARTSTAT", ErrInvalidICS)
			}
			p.Params.Set(ical.ParamParticipationStatus, string(a.PartStat))
		}
		if a.Role != "" {
			if !tokenRE.MatchString(a.Role) {
				return nil, fmt.Errorf("%w: bad ROLE", ErrInvalidICS)
			}
			p.Params.Set(ical.ParamRole, a.Role)
		}
		if a.RSVP {
			p.Params.Set(ical.ParamRSVP, "TRUE")
		}
		props.Add(p)
	}
	if ev.RRule != "" {
		if hasControl(ev.RRule) {
			return nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
		}
		if _, err := rrule.StrToROption(ev.RRule); err != nil {
			return nil, fmt.Errorf("%w: bad RRULE", ErrInvalidICS)
		}
		setRaw(props, ical.PropRecurrenceRule, ev.RRule)
	}
	for _, t := range ev.RDates {
		props.Add(style.prop(ical.PropRecurrenceDates, t))
	}
	for _, t := range ev.ExDates {
		props.Add(style.prop(ical.PropExceptionDates, t))
	}
	if !ev.Updated.IsZero() {
		props.SetDateTime(ical.PropLastModified, ev.Updated.UTC())
	}
	return comp, nil
}

func setText(props ical.Props, name, text string) {
	if text == "" {
		return
	}
	props.SetText(name, cleanText(text))
}

func setRaw(props ical.Props, name, value string) {
	p := ical.NewProp(name)
	p.Value = value
	props.Set(p)
}

func personProp(name string, person Person) (*ical.Prop, error) {
	email := person.Email
	if email == "" || hasControl(email) {
		return nil, fmt.Errorf("%w: bad %s address", ErrInvalidICS, name)
	}
	p := ical.NewProp(name)
	lower := strings.ToLower(email)
	if strings.HasPrefix(lower, "mailto:") || (strings.Contains(email, ":") && !strings.Contains(email, "@")) {
		p.Value = email
	} else {
		p.Value = "mailto:" + email
	}
	// A parameter value cannot hold a double quote or a line break.
	if cn := strings.ReplaceAll(cleanLine(person.Name), `"`, "'"); cn != "" {
		p.Params.Set(ical.ParamCommonName, cn)
	}
	return p, nil
}

// Fold folds content lines longer than 75 octets (RFC 5545 section 3.1),
// never inside a valid UTF-8 sequence. Every fold moves past at least one
// octet: a run of octets with no rune start, which is not UTF-8, is cut at
// the line width. Output over limit octets is refused with ErrTooLarge
// before more of it is built.
func Fold(data []byte, limit int) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(min(len(data)+len(data)/foldWidth*3, limit))
	for line := range bytes.SplitAfterSeq(data, []byte("\r\n")) {
		body, terminated := bytes.CutSuffix(line, []byte("\r\n"))
		width := foldWidth
		for len(body) > width {
			cut := width
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
			if cut == 0 {
				cut = width
			}
			out.Write(body[:cut])
			out.WriteString("\r\n ")
			body = body[cut:]
			width = foldWidth - 1
			if out.Len() > limit {
				return nil, ErrTooLarge
			}
		}
		out.Write(body)
		if terminated {
			out.WriteString("\r\n")
		}
		if out.Len() > limit {
			return nil, ErrTooLarge
		}
	}
	return out.Bytes(), nil
}
