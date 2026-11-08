package google

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// apiTime is an event time: date for all-day events, dateTime otherwise.
type apiTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type apiPerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type apiAttendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	Optional       bool   `json:"optional"`
	Resource       bool   `json:"resource"`
	ResponseStatus string `json:"responseStatus"`
}

// apiEvent is the part of an event resource chainmail models.
type apiEvent struct {
	ID                string        `json:"id"`
	ETag              string        `json:"etag"`
	Status            string        `json:"status"`
	ICalUID           string        `json:"iCalUID"`
	RecurringEventID  string        `json:"recurringEventId"`
	OriginalStartTime *apiTime      `json:"originalStartTime"`
	Summary           string        `json:"summary"`
	Description       string        `json:"description"`
	Location          string        `json:"location"`
	Start             *apiTime      `json:"start"`
	End               *apiTime      `json:"end"`
	Recurrence        []string      `json:"recurrence"`
	Transparency      string        `json:"transparency"`
	Sequence          int           `json:"sequence"`
	Organizer         *apiPerson    `json:"organizer"`
	Attendees         []apiAttendee `json:"attendees"`
	AttendeesOmitted  bool          `json:"attendeesOmitted"`
	Updated           string        `json:"updated"`
}

// deleted reports whether a is a deleted event, rather than a cancelled
// instance of a series.
func (a apiEvent) deleted() bool {
	return a.Status == "cancelled" && a.RecurringEventID == ""
}

// decodeEvent decodes one event resource. It returns the apiEvent too, so
// callers can tell deletions apart.
func decodeEvent(raw []byte) (calendar.Event, apiEvent, error) {
	var a apiEvent
	if err := json.Unmarshal(raw, &a); err != nil || a.ID == "" {
		return calendar.Event{}, a, errBadResponse
	}
	ev, err := toEvent(a, raw)
	return ev, a, err
}

// toEvent maps an event resource to a calendar.Event. Errors name only the
// event id and the field.
func toEvent(a apiEvent, raw []byte) (calendar.Event, error) {
	ev := calendar.Event{
		ProviderID:  a.ID,
		ETag:        a.ETag,
		UID:         a.ICalUID,
		Summary:     a.Summary,
		Location:    a.Location,
		Description: a.Description,
		Status:      eventStatus(a.Status),
		Sequence:    a.Sequence,
		Transparent: a.Transparency == "transparent",
		Raw:         bytes.Clone(raw),
	}
	if a.RecurringEventID != "" {
		rid, _, _, err := parseTime(a.OriginalStartTime)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("%w: event %s: bad originalStartTime", errBadResponse, a.ID)
		}
		ev.ProviderID = a.RecurringEventID
		ev.RecurrenceID = rid
	}
	switch {
	case a.Start != nil:
		start, allDay, tzid, err := parseTime(a.Start)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("%w: event %s: bad start", errBadResponse, a.ID)
		}
		ev.Start, ev.AllDay, ev.TZID = start, allDay, tzid
		ev.End = start
		if a.End != nil {
			end, _, _, err := parseTime(a.End)
			if err != nil {
				return calendar.Event{}, fmt.Errorf("%w: event %s: bad end", errBadResponse, a.ID)
			}
			ev.End = end
		}
	case a.Status == "cancelled":
		// Google sends a cancelled instance or a deleted event with little
		// more than its id: it starts where it would have.
		ev.Start, ev.End = ev.RecurrenceID, ev.RecurrenceID
		if a.OriginalStartTime != nil {
			ev.AllDay = a.OriginalStartTime.Date != ""
		}
	default:
		return calendar.Event{}, fmt.Errorf("%w: event %s: no start", errBadResponse, a.ID)
	}
	if a.Organizer != nil && a.Organizer.Email != "" {
		ev.Organizer = &calendar.Person{Email: a.Organizer.Email, Name: a.Organizer.DisplayName}
	}
	for i, at := range a.Attendees {
		if i == calendar.MaxAttendees {
			ev.AttendeesTruncated = true
			break
		}
		ev.Attendees = append(ev.Attendees, toAttendee(at))
	}
	if a.AttendeesOmitted {
		ev.AttendeesTruncated = true
	}
	ev.RRule, ev.RDates, ev.ExDates = parseRecurrence(a.Recurrence, ev.TZID)
	if t, err := time.Parse(time.RFC3339, a.Updated); err == nil {
		ev.Updated = t.UTC()
	}
	return ev, nil
}

func toAttendee(a apiAttendee) calendar.Attendee {
	role := "REQ-PARTICIPANT"
	switch {
	case a.Resource:
		role = "NON-PARTICIPANT"
	case a.Optional:
		role = "OPT-PARTICIPANT"
	}
	return calendar.Attendee{
		Person:   calendar.Person{Email: a.Email, Name: a.DisplayName},
		PartStat: partStat(a.ResponseStatus),
		Role:     role,
	}
}

func eventStatus(s string) calendar.Status {
	switch s {
	case "confirmed":
		return calendar.StatusConfirmed
	case "tentative":
		return calendar.StatusTentative
	case "cancelled":
		return calendar.StatusCancelled
	}
	return ""
}

func apiStatus(s calendar.Status) string {
	switch s {
	case calendar.StatusConfirmed:
		return "confirmed"
	case calendar.StatusTentative:
		return "tentative"
	case calendar.StatusCancelled:
		return "cancelled"
	}
	return ""
}

func partStat(s string) calendar.PartStat {
	switch s {
	case "accepted":
		return calendar.PartStatAccepted
	case "declined":
		return calendar.PartStatDeclined
	case "tentative":
		return calendar.PartStatTentative
	}
	return calendar.PartStatNeedsAction
}

func responseStatus(p calendar.PartStat) string {
	switch p {
	case calendar.PartStatAccepted:
		return "accepted"
	case calendar.PartStatDeclined:
		return "declined"
	case calendar.PartStatTentative:
		return "tentative"
	case calendar.PartStatNeedsAction:
		return "needsAction"
	}
	return ""
}

// parseTime reads an event time as UTC. An all-day date is 00:00 UTC of the
// date. tzid is the event's IANA zone, "" for UTC or a zone Go cannot load.
func parseTime(t *apiTime) (at time.Time, allDay bool, tzid string, err error) {
	if t == nil {
		return time.Time{}, false, "", errBadResponse
	}
	if t.Date != "" {
		d, err := time.Parse(time.DateOnly, t.Date)
		return d, true, "", err
	}
	d, err := time.Parse(time.RFC3339, t.DateTime)
	if err != nil {
		return time.Time{}, false, "", err
	}
	return d.UTC(), false, zoneName(t.TimeZone), nil
}

// zoneName returns tz when it is an IANA zone other than UTC that Go can
// load, else "".
func zoneName(tz string) string {
	switch strings.ToUpper(tz) {
	case "", "UTC", "ETC/UTC", "GMT", "ETC/GMT", "Z", "LOCAL":
		return ""
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ""
	}
	return tz
}

// parseRecurrence reads Google's recurrence lines: RRULE, RDATE and EXDATE
// in iCalendar syntax. Values it cannot read are skipped; EXRULE and other
// lines are ignored.
func parseRecurrence(lines []string, tzid string) (rrule string, rdates, exdates []time.Time) {
	for _, line := range lines {
		head, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parts := strings.Split(head, ";")
		switch strings.ToUpper(parts[0]) {
		case "RRULE":
			if rrule == "" {
				rrule = value
			}
		case "RDATE":
			rdates = append(rdates, parseDates(parts[1:], value, tzid)...)
		case "EXDATE":
			exdates = append(exdates, parseDates(parts[1:], value, tzid)...)
		}
	}
	return rrule, rdates, exdates
}

// parseDates reads a comma-separated RDATE or EXDATE value list as UTC.
// Local times are in the TZID parameter's zone, or the event's.
func parseDates(params []string, value, tzid string) []time.Time {
	loc := time.UTC
	zone := tzid
	dateOnly := false
	for _, p := range params {
		k, v, _ := strings.Cut(p, "=")
		switch strings.ToUpper(k) {
		case "TZID":
			zone = strings.Trim(v, `"`)
		case "VALUE":
			dateOnly = strings.EqualFold(v, "DATE")
		}
	}
	if zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}
	var out []time.Time
	for v := range strings.SplitSeq(value, ",") {
		v, _, _ = strings.Cut(strings.TrimSpace(v), "/") // a PERIOD's start
		var (
			t   time.Time
			err error
		)
		switch {
		case dateOnly || len(v) == 8:
			t, err = time.Parse("20060102", v)
		case strings.HasSuffix(v, "Z"):
			t, err = time.Parse("20060102T150405Z", v)
		default:
			t, err = time.ParseInLocation("20060102T150405", v, loc)
		}
		if err == nil {
			out = append(out, t.UTC())
		}
	}
	return out
}

// eventTime is the JSON of an event time: a date for all-day events, else
// the local time in tzid (UTC when tzid is "" or unknown) with its zone.
func eventTime(t time.Time, tzid string, allDay bool) map[string]any {
	if allDay {
		return map[string]any{"date": t.UTC().Format(time.DateOnly)}
	}
	loc, zone := time.UTC, "UTC"
	if tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			loc, zone = l, tzid
		}
	}
	return map[string]any{"dateTime": t.In(loc).Format(time.RFC3339), "timeZone": zone}
}

// recurrenceLines writes RRULE, RDATE and EXDATE as Google takes them, with
// local times in the event's zone as Google writes them itself. It returns
// nil for an event that does not recur.
func recurrenceLines(ev calendar.Event) ([]string, error) {
	if strings.ContainsFunc(ev.RRule, isControl) {
		return nil, fmt.Errorf("%w: control character in the recurrence rule", ErrRequest)
	}
	var lines []string
	if ev.RRule != "" {
		lines = append(lines, "RRULE:"+ev.RRule)
	}
	if len(ev.RDates) > 0 {
		lines = append(lines, dateLine("RDATE", ev.RDates, ev.TZID, ev.AllDay))
	}
	if len(ev.ExDates) > 0 {
		lines = append(lines, dateLine("EXDATE", ev.ExDates, ev.TZID, ev.AllDay))
	}
	return lines, nil
}

func dateLine(name string, ts []time.Time, tzid string, allDay bool) string {
	vals := make([]string, len(ts))
	switch {
	case allDay:
		for i, t := range ts {
			vals[i] = t.UTC().Format("20060102")
		}
		return name + ";VALUE=DATE:" + strings.Join(vals, ",")
	case zoneName(tzid) != "":
		loc, _ := time.LoadLocation(tzid) // zoneName loaded it
		for i, t := range ts {
			vals[i] = t.In(loc).Format("20060102T150405")
		}
		return name + ";TZID=" + tzid + ":" + strings.Join(vals, ",")
	default:
		for i, t := range ts {
			vals[i] = t.UTC().Format("20060102T150405Z")
		}
		return name + ":" + strings.Join(vals, ",")
	}
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// rawAttendees returns the attendee objects of an event's JSON by
// lowercased address, so a write keeps fields chainmail does not model.
func rawAttendees(raw []byte) map[string]map[string]any {
	var obj struct {
		Attendees []map[string]any `json:"attendees"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	out := make(map[string]map[string]any, len(obj.Attendees))
	for _, a := range obj.Attendees {
		if email, ok := a["email"].(string); ok {
			out[strings.ToLower(email)] = a
		}
	}
	return out
}

// attendeesJSON writes atts, starting each from what Google knows of that
// attendee.
func attendeesJSON(atts []calendar.Attendee, known map[string]map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		m := map[string]any{}
		for k, v := range known[strings.ToLower(a.Email)] {
			m[k] = v
		}
		m["email"] = a.Email
		if a.Name != "" {
			m["displayName"] = a.Name
		}
		if a.Role == "OPT-PARTICIPANT" {
			m["optional"] = true
		} else {
			delete(m, "optional")
		}
		if rs := responseStatus(a.PartStat); rs != "" {
			m["responseStatus"] = rs
		}
		out = append(out, m)
	}
	return out
}

// patchTime is eventTime for a PATCH. Google merges a nested object into the
// stored one, so the fields of the other kind are sent as JSON null: an
// event that switches between timed and all-day would otherwise keep its
// old date or dateTime next to the new one.
func patchTime(t time.Time, tzid string, allDay bool) map[string]any {
	m := eventTime(t, tzid, allDay)
	if allDay {
		m["dateTime"], m["timeZone"] = nil, nil
	} else {
		m["date"] = nil
	}
	return m
}
