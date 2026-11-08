package calendar

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	_ "time/tzdata" // the tests name zones, so hosts without zoneinfo still run them
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

func mustParse(t *testing.T, data []byte) (string, []Event) {
	t.Helper()
	method, events, err := ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	return method, events
}

// eventDiff reports the first field of got that differs from want, ignoring Raw.
func eventDiff(got, want Event) string {
	got.Raw, want.Raw = nil, nil
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := range gv.NumField() {
		g, w := gv.Field(i).Interface(), wv.Field(i).Interface()
		if gt, ok := g.(time.Time); ok {
			if !gt.Equal(w.(time.Time)) {
				return fmt.Sprintf("%s = %v, want %v", gv.Type().Field(i).Name, g, w)
			}
			continue
		}
		if !reflect.DeepEqual(g, w) {
			return fmt.Sprintf("%s = %#v, want %#v", gv.Type().Field(i).Name, g, w)
		}
	}
	return ""
}

func TestParseICSGoogleInvite(t *testing.T) {
	data := readFixture(t, "google_invite.ics")
	method, events := mustParse(t, data)
	if method != MethodRequest {
		t.Fatalf("method = %q, want REQUEST", method)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	want := Event{
		UID:         "7kq3abc123def456@google.com",
		Summary:     "Weekly sync",
		Location:    "Room 4, Building B",
		Description: "Weekly sync.\nAgenda below, as usual.\n\n-::~:~::~:~:~:~:~:~:~" + strings.Repeat(":~", 28) + "::~:~::-",
		Start:       utc(2026, 11, 10, 17, 0, 0),
		End:         utc(2026, 11, 10, 17, 30, 0),
		TZID:        "America/Los_Angeles",
		Status:      StatusConfirmed,
		Organizer:   &Person{Email: "alice@example.com", Name: "Alice Example"},
		Attendees: []Attendee{
			{Person: Person{Email: "james@example.org", Name: "james@example.org"}, PartStat: PartStatNeedsAction, Role: "REQ-PARTICIPANT", RSVP: true},
			{Person: Person{Email: "alice@example.com", Name: "Alice Example"}, PartStat: PartStatAccepted, Role: "REQ-PARTICIPANT", RSVP: true},
		},
		RRule:   "FREQ=WEEKLY;BYDAY=TU",
		ExDates: []time.Time{utc(2026, 11, 17, 17, 0, 0)},
		Updated: utc(2026, 11, 1, 12, 0, 30),
	}
	if d := eventDiff(ev, want); d != "" {
		t.Fatal(d)
	}
	if !bytes.Equal(ev.Raw, data) {
		t.Fatal("Raw is not the whole input")
	}
	if !ev.IsMaster() {
		t.Fatal("a recurring event without RECURRENCE-ID is a master")
	}
}

func TestParseICSOutlookInvite(t *testing.T) {
	method, events := mustParse(t, readFixture(t, "outlook_invite.ics"))
	if method != MethodRequest || len(events) != 1 {
		t.Fatalf("method %q, %d events; want REQUEST, 1", method, len(events))
	}
	want := Event{
		UID:         "040000008200E00074C5B7101A82E00800000000A0B1C2D3E4F5A6B700000000000000001000000000AABBCCDDEEFF",
		Summary:     "Q4 planning",
		Location:    "Conf Room 12",
		Description: "Quarterly planning\n\n",
		// A Windows zone name maps to its IANA zone: 14:00 PDT is 21:00 UTC.
		Start:       utc(2026, 7, 15, 21, 0, 0),
		End:         utc(2026, 7, 15, 22, 30, 0),
		TZID:        "America/Los_Angeles",
		Status:      StatusTentative,
		Sequence:    2,
		Transparent: true,
		Organizer:   &Person{Email: "bob@contoso.com", Name: "Bob Smith, Jr."},
		Attendees: []Attendee{
			{Person: Person{Email: "james@example.org", Name: "James Ainslie"}, PartStat: PartStatNeedsAction, Role: "REQ-PARTICIPANT", RSVP: true},
			{Person: Person{Email: "carol@contoso.com", Name: "Carol"}, PartStat: PartStatTentative, Role: "OPT-PARTICIPANT"},
			{Person: Person{Email: "room12@contoso.com", Name: "Conf Room 12"}, PartStat: PartStatNeedsAction, Role: "NON-PARTICIPANT"},
		},
		Updated: utc(2026, 7, 1, 8, 0, 0),
	}
	if d := eventDiff(events[0], want); d != "" {
		t.Fatal(d)
	}
}

func TestParseICSAppleSeries(t *testing.T) {
	method, events := mustParse(t, readFixture(t, "apple_series.ics"))
	if method != "" {
		t.Fatalf("method = %q, want empty", method)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want master and two overrides", len(events))
	}
	uid := "5B9E2B6A-0C1D-4E3F-9A8B-7C6D5E4F3A2B"
	wantMaster := Event{
		UID:       uid,
		Summary:   "Standup",
		Start:     utc(2026, 11, 2, 10, 0, 0),
		End:       utc(2026, 11, 2, 10, 30, 0),
		TZID:      "Europe/London",
		Sequence:  1,
		Organizer: &Person{Email: "james@example.org", Name: "James"},
		Attendees: []Attendee{
			{Person: Person{Email: "james@example.org", Name: "James"}, PartStat: PartStatAccepted, Role: "CHAIR"},
			{Person: Person{Email: "dana@example.net", Name: "Dana"}, PartStat: PartStatNeedsAction, RSVP: true},
		},
		RRule:   "FREQ=WEEKLY;UNTIL=20261231T235959Z",
		RDates:  []time.Time{utc(2026, 11, 4, 15, 0, 0)},
		ExDates: []time.Time{utc(2026, 11, 9, 10, 0, 0), utc(2026, 11, 16, 10, 0, 0), utc(2026, 11, 23, 10, 0, 0)},
		Updated: utc(2026, 10, 1, 10, 5, 0),
	}
	if d := eventDiff(events[0], wantMaster); d != "" {
		t.Fatalf("master: %s", d)
	}
	wantMoved := Event{
		UID:          uid,
		RecurrenceID: utc(2026, 11, 30, 10, 0, 0),
		Summary:      "Standup (moved)",
		Start:        utc(2026, 11, 30, 11, 0, 0),
		End:          utc(2026, 11, 30, 11, 30, 0),
		TZID:         "Europe/London",
		Sequence:     2,
		Updated:      utc(2026, 10, 5, 9, 0, 0),
	}
	if d := eventDiff(events[1], wantMoved); d != "" {
		t.Fatalf("moved override: %s", d)
	}
	if events[1].IsMaster() {
		t.Fatal("an override is not a master")
	}
	if got := events[2]; !got.RecurrenceID.Equal(utc(2026, 12, 7, 10, 0, 0)) || got.Status != StatusCancelled {
		t.Fatalf("cancelled override: rid %v status %q", got.RecurrenceID, got.Status)
	}
}

func TestParseICSAllDayAndFloating(t *testing.T) {
	data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" +
		"BEGIN:VEVENT\r\nUID:allday\r\nDTSTAMP:20261101T000000Z\r\nDTSTART;VALUE=DATE:20261224\r\nDTEND;VALUE=DATE:20261227\r\nRRULE:FREQ=YEARLY\r\nEXDATE;VALUE=DATE:20271224\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:allday-noend\r\nDTSTAMP:20261101T000000Z\r\nDTSTART;VALUE=DATE:20261231\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:floating\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000\r\nDURATION:PT45M\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:unknown-zone\r\nDTSTAMP:20261101T000000Z\r\nDTSTART;TZID=Nowhere/Special:20261110T090000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	_, events := mustParse(t, data)
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	ad := events[0]
	if !ad.AllDay || !ad.Start.Equal(utc(2026, 12, 24, 0, 0, 0)) || !ad.End.Equal(utc(2026, 12, 27, 0, 0, 0)) || ad.TZID != "" {
		t.Fatalf("all-day: %+v", ad)
	}
	if len(ad.ExDates) != 1 || !ad.ExDates[0].Equal(utc(2027, 12, 24, 0, 0, 0)) {
		t.Fatalf("all-day EXDATE: %v", ad.ExDates)
	}
	if ne := events[1]; !ne.AllDay || !ne.End.Equal(utc(2027, 1, 1, 0, 0, 0)) {
		t.Fatalf("all-day without DTEND lasts one day: %+v", ne)
	}
	fl := events[2]
	if fl.AllDay || fl.TZID != "" || !fl.Start.Equal(utc(2026, 11, 10, 9, 0, 0)) || !fl.End.Equal(utc(2026, 11, 10, 9, 45, 0)) {
		t.Fatalf("floating: start %v end %v tzid %q", fl.Start, fl.End, fl.TZID)
	}
	// An unknown zone without a VTIMEZONE reads as floating.
	if uz := events[3]; uz.TZID != "" || !uz.Start.Equal(utc(2026, 11, 10, 9, 0, 0)) || !uz.End.Equal(uz.Start) {
		t.Fatalf("unknown zone: start %v end %v tzid %q", uz.Start, uz.End, uz.TZID)
	}
}

// A zone that is neither IANA nor a known Windows name is resolved from its
// VTIMEZONE rules, on both sides of the DST change.
func TestParseICSCustomVTimezone(t *testing.T) {
	data := []byte(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:x
BEGIN:VTIMEZONE
TZID:(UTC-08:00) Pacific Time (US & Canada)
BEGIN:STANDARD
DTSTART:16011104T020000
RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=11
TZOFFSETFROM:-0700
TZOFFSETTO:-0800
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010311T020000
RRULE:FREQ=YEARLY;BYDAY=2SU;BYMONTH=3
TZOFFSETFROM:-0800
TZOFFSETTO:-0700
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
UID:summer
DTSTAMP:20260101T000000Z
DTSTART;TZID="(UTC-08:00) Pacific Time (US & Canada)":20260715T090000
DTEND;TZID="(UTC-08:00) Pacific Time (US & Canada)":20260715T100000
END:VEVENT
BEGIN:VEVENT
UID:winter
DTSTAMP:20260101T000000Z
DTSTART;TZID="(UTC-08:00) Pacific Time (US & Canada)":20260115T090000
DTEND;TZID="(UTC-08:00) Pacific Time (US & Canada)":20260115T100000
EXDATE;TZID="(UTC-08:00) Pacific Time (US & Canada)":20260716T090000
EXDATE;TZID="(UTC-08:00) Pacific Time (US & Canada)":25000716T090000,17500716T090000
END:VEVENT
END:VCALENDAR
`)
	_, events := mustParse(t, data)
	if len(events) != 2 {
		t.Fatalf("got %d events", len(events))
	}
	if s := events[0]; !s.Start.Equal(utc(2026, 7, 15, 16, 0, 0)) || !s.End.Equal(utc(2026, 7, 15, 17, 0, 0)) || s.TZID != "" {
		t.Fatalf("summer: start %v end %v tzid %q", s.Start, s.End, s.TZID)
	}
	if w := events[1]; !w.Start.Equal(utc(2026, 1, 15, 17, 0, 0)) || !w.ExDates[0].Equal(utc(2026, 7, 16, 16, 0, 0)) {
		t.Fatalf("winter: start %v exdates %v", w.Start, w.ExDates)
	}
	// Years outside 1970 to 2100 are read with the rules of the nearest one.
	if w := events[1]; !w.ExDates[1].Equal(utc(2500, 7, 16, 16, 0, 0)) || !w.ExDates[2].Equal(utc(1750, 7, 16, 16, 0, 0)) {
		t.Fatalf("far years: exdates %v", w.ExDates)
	}
}

func TestParseICSZoneNameForms(t *testing.T) {
	for _, tc := range []struct{ tzid, want string }{
		{"Europe/Berlin", "Europe/Berlin"},
		{"/mozilla.org/20050126_1/Europe/Berlin", "Europe/Berlin"},
		{"/freeassociation.sourceforge.net/Tzfile/Europe/Berlin", "Europe/Berlin"},
		{"W. Europe Standard Time", "Europe/Berlin"},
		{"UTC", ""},
		{"Etc/UTC", ""},
		{"Local", ""},
	} {
		data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261101T000000Z\r\n" +
			"DTSTART;TZID=\"" + tc.tzid + "\":20260715T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
		_, events := mustParse(t, data)
		ev := events[0]
		if ev.TZID != tc.want {
			t.Errorf("TZID %q read as %q, want %q", tc.tzid, ev.TZID, tc.want)
		}
		wantStart := utc(2026, 7, 15, 9, 0, 0)
		if tc.want != "" {
			wantStart = utc(2026, 7, 15, 7, 0, 0) // 09:00 CEST
		}
		if !ev.Start.Equal(wantStart) {
			t.Errorf("TZID %q: start %v, want %v", tc.tzid, ev.Start, wantStart)
		}
	}
}

func TestParseICSAttendeeLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:big\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000Z\r\n")
	for i := range MaxAttendees + 1 {
		fmt.Fprintf(&b, "ATTENDEE;PARTSTAT=ACCEPTED:mailto:p%d@example.com\r\n", i)
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	_, events := mustParse(t, []byte(b.String()))
	ev := events[0]
	if len(ev.Attendees) != MaxAttendees || !ev.AttendeesTruncated {
		t.Fatalf("got %d attendees, truncated %v; want %d, true", len(ev.Attendees), ev.AttendeesTruncated, MaxAttendees)
	}
	if ev.Attendees[MaxAttendees-1].Email != fmt.Sprintf("p%d@example.com", MaxAttendees-1) {
		t.Fatal("the first attendees are kept")
	}

	_, events = mustParse(t, readFixture(t, "google_invite.ics"))
	if events[0].AttendeesTruncated {
		t.Fatal("a small event is not truncated")
	}
}

func TestParseICSTooLarge(t *testing.T) {
	head := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000Z\r\nDESCRIPTION:"
	tail := "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	fill := func(n int) []byte {
		return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
	}
	if _, _, err := ParseICS(fill(MaxICSBytes + 1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("1 MiB + 1 byte: err = %v, want ErrTooLarge", err)
	}
	if _, _, err := ParseICS(fill(MaxICSBytes)); err != nil {
		t.Fatalf("exactly 1 MiB: %v", err)
	}
}

func TestParseICSMalformed(t *testing.T) {
	const secret = "SECRET-TEXT-42"
	cases := map[string]string{
		"empty":           "",
		"not ics":         "hello " + secret,
		"no end":          "BEGIN:VCALENDAR\r\nSUMMARY:" + secret + "\r\n",
		"wrong top level": "BEGIN:VEVENT\r\nUID:" + secret + "\r\nEND:VEVENT\r\n",
		"mismatched end":  "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:" + secret + "\r\nEND:VTODO\r\nEND:VCALENDAR\r\n",
		"param no colon":  "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nATTENDEE;CN=" + secret + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"bad param char":  "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nATTENDEE;CN=\"" + secret + "\"x:mailto:a@b\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"bad dtstart":     "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTART:" + secret + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"bad sequence":    "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTART:20261110T090000Z\r\nSEQUENCE:" + secret + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"bad exdate":      "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTART:20261110T090000Z\r\nEXDATE:20261110T090000Z," + secret + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseICS([]byte(s))
			if !errors.Is(err, ErrInvalidICS) {
				t.Fatalf("err = %v, want ErrInvalidICS", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q echoes the input", err)
			}
		})
	}
}

func TestParseICSSanitisesText(t *testing.T) {
	data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000Z\r\n" +
		"SUMMARY:bad \xff byte\\, and \\q escape\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, events := mustParse(t, data)
	if got, want := events[0].Summary, "bad � byte, and q escape"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func roundTripEvents() []Event {
	return []Event{
		{
			UID:         "series-1@chainmail",
			Summary:     "Planning; with, punctuation\\ and\nlines",
			Location:    "Room 1",
			Description: "Line one\nLine two",
			Start:       utc(2026, 11, 10, 17, 0, 0),
			End:         utc(2026, 11, 10, 18, 0, 0),
			TZID:        "America/Los_Angeles",
			Status:      StatusConfirmed,
			Sequence:    3,
			Transparent: true,
			Organizer:   &Person{Email: "james@example.org", Name: "James \"JA\" Ainslie"},
			Attendees: []Attendee{
				{Person: Person{Email: "james@example.org", Name: "James"}, PartStat: PartStatAccepted, Role: "CHAIR"},
				{Person: Person{Email: "a@example.com", Name: "Alice, A."}, PartStat: PartStatNeedsAction, Role: "REQ-PARTICIPANT", RSVP: true},
				{Person: Person{Email: "b@example.com"}, PartStat: PartStatDeclined, Role: "OPT-PARTICIPANT"},
				{Person: Person{Email: "c@example.com"}, PartStat: PartStatTentative},
				{Person: Person{Email: "d@example.com"}, PartStat: PartStatDelegated, Role: "NON-PARTICIPANT"},
			},
			RRule:   "FREQ=WEEKLY;BYDAY=TU;UNTIL=20270101T000000Z",
			RDates:  []time.Time{utc(2026, 11, 12, 17, 0, 0)},
			ExDates: []time.Time{utc(2026, 11, 17, 17, 0, 0), utc(2026, 11, 24, 17, 0, 0)},
			Updated: utc(2026, 11, 1, 12, 0, 0),
		},
		{
			UID:          "series-1@chainmail",
			RecurrenceID: utc(2026, 12, 1, 17, 0, 0),
			Summary:      "Planning (moved)",
			Start:        utc(2026, 12, 2, 18, 0, 0),
			End:          utc(2026, 12, 2, 19, 0, 0),
			TZID:         "America/Los_Angeles",
			Status:       StatusTentative,
			Sequence:     4,
			Organizer:    &Person{Email: "james@example.org"},
			Updated:      utc(2026, 11, 2, 12, 0, 0),
		},
		{
			UID:          "series-1@chainmail",
			RecurrenceID: utc(2026, 12, 8, 17, 0, 0),
			Start:        utc(2026, 12, 8, 17, 0, 0),
			End:          utc(2026, 12, 8, 18, 0, 0),
			Status:       StatusCancelled,
			Updated:      utc(2026, 11, 3, 12, 0, 0),
		},
		{
			UID:     "allday@chainmail",
			Summary: "Holiday",
			Start:   utc(2026, 12, 24, 0, 0, 0),
			End:     utc(2026, 12, 27, 0, 0, 0),
			AllDay:  true,
			RRule:   "FREQ=YEARLY",
			ExDates: []time.Time{utc(2027, 12, 24, 0, 0, 0)},
			Updated: utc(2026, 11, 1, 0, 0, 0),
		},
		{
			UID:     "utc@chainmail",
			Summary: strings.Repeat("long ünïcödé summary ", 20),
			Start:   utc(2026, 11, 20, 9, 0, 0),
			End:     utc(2026, 11, 20, 9, 30, 0),
			Updated: utc(2026, 11, 1, 0, 0, 0),
		},
	}
}

func TestEncodeICSRoundTrip(t *testing.T) {
	in := roundTripEvents()
	data, err := EncodeICS(MethodRequest, in)
	if err != nil {
		t.Fatalf("EncodeICS: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("line %d is %d octets, longer than 75: %q", i, len(line), line)
		}
	}
	if !bytes.Contains(data, []byte("PRODID:-//chainmail//EN\r\n")) || !bytes.Contains(data, []byte("VERSION:2.0\r\n")) {
		t.Fatalf("missing PRODID or VERSION:\n%s", data)
	}
	if !bytes.Contains(data, []byte("DTSTART;TZID=America/Los_Angeles:20261110T090000\r\n")) {
		t.Fatalf("timed event with a zone is not written TZID-qualified:\n%s", data)
	}
	method, out := mustParse(t, data)
	if method != MethodRequest {
		t.Fatalf("method = %q", method)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d events back, want %d", len(out), len(in))
	}
	for i := range in {
		// The encoder replaces a double quote in a parameter value, which
		// iCalendar cannot carry.
		want := in[i]
		if want.Organizer != nil {
			o := *want.Organizer
			o.Name = strings.ReplaceAll(o.Name, `"`, "'")
			want.Organizer = &o
		}
		if d := eventDiff(out[i], want); d != "" {
			t.Errorf("event %d: %s", i, d)
		}
	}

	_, events, err := ParseICS(mustEncode(t, "", in[4:]))
	if err != nil || len(events) != 1 {
		t.Fatalf("no METHOD: %v", err)
	}
}

func mustEncode(t *testing.T, method string, events []Event) []byte {
	t.Helper()
	b, err := EncodeICS(method, events)
	if err != nil {
		t.Fatalf("EncodeICS: %v", err)
	}
	if method == "" && bytes.Contains(b, []byte("METHOD")) {
		t.Fatal("METHOD written without a method")
	}
	return b
}

func TestEncodeICSUnknownZoneFallsBackToUTC(t *testing.T) {
	ev := Event{UID: "u", Start: utc(2026, 11, 10, 17, 0, 0), End: utc(2026, 11, 10, 18, 0, 0), TZID: "Nowhere/Special", Updated: utc(2026, 1, 1, 0, 0, 0)}
	data := mustEncode(t, "", []Event{ev})
	if !bytes.Contains(data, []byte("DTSTART:20261110T170000Z\r\n")) {
		t.Fatalf("unknown zone not written as UTC:\n%s", data)
	}
}

func TestEncodeICSRejectsInjection(t *testing.T) {
	base := Event{UID: "u", Start: utc(2026, 11, 10, 17, 0, 0), End: utc(2026, 11, 10, 18, 0, 0), Updated: utc(2026, 1, 1, 0, 0, 0)}
	cases := map[string]func(*Event){
		"uid newline":      func(e *Event) { e.UID = "u\r\nATTENDEE:mailto:x@evil" },
		"rrule newline":    func(e *Event) { e.RRule = "FREQ=DAILY\r\nATTENDEE:mailto:x@evil" },
		"email newline":    func(e *Event) { e.Attendees = []Attendee{{Person: Person{Email: "a@b\nATTENDEE:mailto:x@evil"}}} },
		"organizer ctl":    func(e *Event) { e.Organizer = &Person{Email: "a@b\x00"} },
		"role newline":     func(e *Event) { e.Attendees = []Attendee{{Person: Person{Email: "a@b"}, Role: "CHAIR\r\nX:y"}} },
		"partstat newline": func(e *Event) { e.Attendees = []Attendee{{Person: Person{Email: "a@b"}, PartStat: "ACCEPTED\nX:y"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := base
			mutate(&ev)
			if _, err := EncodeICS("", []Event{ev}); !errors.Is(err, ErrInvalidICS) {
				t.Fatalf("err = %v, want ErrInvalidICS", err)
			}
		})
	}

	// Names are text: line breaks and controls are dropped, not refused.
	ev := base
	ev.Attendees = []Attendee{{Person: Person{Email: "a@b", Name: "Evil\r\nATTENDEE:mailto:x@evil"}}}
	_, events := mustParse(t, mustEncode(t, "", []Event{ev}))
	if len(events[0].Attendees) != 1 || strings.ContainsAny(events[0].Attendees[0].Name, "\r\n") {
		t.Fatalf("name with a line break was not neutralised: %+v", events[0].Attendees)
	}

	if _, err := EncodeICS("", nil); !errors.Is(err, ErrInvalidICS) {
		t.Fatalf("no events: err = %v, want ErrInvalidICS", err)
	}
}

// go-ical builds parameter values a byte at a time, which is quadratic, so
// ParseICS refuses an overlong parameter value before decoding.
func TestParseICSLongParameterValue(t *testing.T) {
	long := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000Z\r\n" +
		"ATTENDEE;CN=" + strings.Repeat("a", MaxICSBytes-300) + ":mailto:a@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, _, err := ParseICS([]byte(long)); !errors.Is(err, ErrInvalidICS) {
		t.Fatalf("unquoted: err = %v, want ErrInvalidICS", err)
	}
	quoted := strings.Replace(long, "CN=a", `CN="a`, 1)
	quoted = strings.Replace(quoted, "a:mailto", `a":mailto`, 1)
	if _, _, err := ParseICS([]byte(quoted)); !errors.Is(err, ErrInvalidICS) {
		t.Fatalf("quoted: err = %v, want ErrInvalidICS", err)
	}
	folded := strings.Replace(long, "CN=", "CN=\r\n ", 1)
	if _, _, err := ParseICS([]byte(folded)); !errors.Is(err, ErrInvalidICS) {
		t.Fatalf("folded: err = %v, want ErrInvalidICS", err)
	}

	// A long property value is fine; only parameters are bounded.
	ok := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000Z\r\n" +
		"ATTENDEE;CN=" + strings.Repeat("b", maxParamValue) + ":mailto:" + strings.Repeat("c", 100000) + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, _, err := ParseICS([]byte(ok)); err != nil {
		t.Fatalf("long value: %v", err)
	}
}

// go-ical reads an equals sign inside an unquoted parameter value as part
// of the value, so it does not end one for the length guard either.
func TestParseICSLongParameterValueWithEquals(t *testing.T) {
	head := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261110T090000Z\r\n"
	tail := ":mailto:a@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for name, param := range map[string]string{
		"equals":        "CN=" + strings.Repeat("a=", maxParamValue),
		"second param":  "ROLE=CHAIR;CN=" + strings.Repeat("=", maxParamValue+1),
		"after a comma": "DELEGATED-TO=x," + strings.Repeat("a=", maxParamValue),
	} {
		if _, _, err := ParseICS([]byte(head + "ATTENDEE;" + param + tail)); !errors.Is(err, ErrInvalidICS) {
			t.Errorf("%s: err = %v, want ErrInvalidICS", name, err)
		}
	}
	// Values separated by commas are bounded one by one.
	short := strings.Repeat("a=", maxParamValue/4)
	_, events := mustParse(t, []byte(head+"ATTENDEE;DELEGATED-TO="+short+","+short+","+short+tail))
	if len(events) != 1 || len(events[0].Attendees) != 1 {
		t.Fatalf("comma-separated values: %+v", events)
	}
}

// EncodeICS is deterministic: a zero Updated gives the Unix epoch as
// DTSTAMP, and EncodeICSAt takes the time to use instead.
func TestEncodeICSDeterministicStamp(t *testing.T) {
	ev := Event{UID: "u", Start: utc(2026, 11, 10, 17, 0, 0), End: utc(2026, 11, 10, 18, 0, 0)}
	a, b := mustEncode(t, "", []Event{ev}), mustEncode(t, "", []Event{ev})
	if !bytes.Equal(a, b) || !bytes.Contains(a, []byte("DTSTAMP:19700101T000000Z\r\n")) {
		t.Fatalf("zero Updated:\n%s\n%s", a, b)
	}
	now := utc(2026, 11, 8, 12, 0, 0)
	c, err := EncodeICSAt("", []Event{ev}, now)
	if err != nil || !bytes.Contains(c, []byte("DTSTAMP:20261108T120000Z\r\n")) {
		t.Fatalf("EncodeICSAt: %v\n%s", err, c)
	}
	ev.Updated = utc(2026, 1, 1, 0, 0, 0)
	d, err := EncodeICSAt("", []Event{ev}, now)
	if err != nil || !bytes.Contains(d, []byte("DTSTAMP:20260101T000000Z\r\n")) {
		t.Fatalf("Updated wins over now: %v\n%s", err, d)
	}
}

// zoneCalendar wraps VTIMEZONE observances and one VEVENT body in a
// VCALENDAR. tzid names the zone; the VEVENT uses it as "TZ".
func zoneCalendar(tzid, observances, event string) []byte {
	s := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:x\nBEGIN:VTIMEZONE\nTZID:" + tzid + "\n" + observances +
		"END:VTIMEZONE\n" + event + "END:VCALENDAR\n"
	return []byte(strings.ReplaceAll(s, "TZ:", tzid+":"))
}

// An observance recurs at most monthly: faster rules are ignored, and the
// observance counts only at its DTSTART.
func TestParseICSObservanceFrequencies(t *testing.T) {
	event := "BEGIN:VEVENT\nUID:u\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260715T090000\nEND:VEVENT\n"
	for _, freq := range []string{"SECONDLY", "MINUTELY", "HOURLY", "DAILY", "WEEKLY"} {
		obs := "BEGIN:STANDARD\nDTSTART:19900101T000000\nTZOFFSETFROM:+0500\nTZOFFSETTO:+0000\nEND:STANDARD\n" +
			"BEGIN:DAYLIGHT\nDTSTART:19800101T000000\nRRULE:FREQ=" + freq + "\nTZOFFSETFROM:+0000\nTZOFFSETTO:+0500\nEND:DAYLIGHT\n"
		_, events := mustParse(t, zoneCalendar("Fast Zone", obs, event))
		if got := events[0].Start; !got.Equal(utc(2026, 7, 15, 9, 0, 0)) {
			t.Errorf("FREQ=%s: start %v, want the STANDARD offset", freq, got)
		}
	}

	// MONTHLY observances are used: +05 from the 10th, +00 from the 20th.
	obs := "BEGIN:STANDARD\nDTSTART:19700120T000000\nRRULE:FREQ=MONTHLY;BYMONTHDAY=20\nTZOFFSETFROM:+0500\nTZOFFSETTO:+0000\nEND:STANDARD\n" +
		"BEGIN:DAYLIGHT\nDTSTART:19700110T000000\nRRULE:FREQ=MONTHLY;BYMONTHDAY=10\nTZOFFSETFROM:+0000\nTZOFFSETTO:+0500\nEND:DAYLIGHT\n"
	two := "BEGIN:VEVENT\nUID:a\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260715T090000\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:b\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260725T090000\nEND:VEVENT\n"
	_, events := mustParse(t, zoneCalendar("Monthly Zone", obs, two))
	if !events[0].Start.Equal(utc(2026, 7, 15, 4, 0, 0)) || !events[1].Start.Equal(utc(2026, 7, 25, 9, 0, 0)) {
		t.Fatalf("monthly observances: %v %v", events[0].Start, events[1].Start)
	}
}

// Observance rules are expanded once, within a bounded amount of work, so
// a rule with an onset every minute of every day is ignored, and rules
// anchored in year 1 are still read correctly.
func TestParseICSObservanceWorkBounded(t *testing.T) {
	event := "BEGIN:VEVENT\nUID:u\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260715T090000\nEND:VEVENT\n"
	every := "FREQ=YEARLY;BYMONTH=" + seq(1, 12) + ";BYMONTHDAY=" + seq(1, 31) + ";BYHOUR=" + seq(0, 23) + ";BYMINUTE=" + seq(0, 59)
	obs := "BEGIN:STANDARD\nDTSTART:19900101T000000\nTZOFFSETFROM:+0500\nTZOFFSETTO:+0000\nEND:STANDARD\n" +
		"BEGIN:DAYLIGHT\nDTSTART:19800101T000000\nRRULE:" + every + "\nTZOFFSETFROM:+0000\nTZOFFSETTO:+0500\nEND:DAYLIGHT\n"
	_, events := mustParse(t, zoneCalendar("Busy Zone", obs, event))
	if got := events[0].Start; !got.Equal(utc(2026, 7, 15, 9, 0, 0)) {
		t.Errorf("every-minute observance: start %v, want the STANDARD offset", got)
	}

	ancient := "BEGIN:STANDARD\nDTSTART:00011025T030000\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\nTZOFFSETFROM:+0200\nTZOFFSETTO:+0100\nEND:STANDARD\n" +
		"BEGIN:DAYLIGHT\nDTSTART:00010329T020000\nRRULE:FREQ=MONTHLY;INTERVAL=12;BYDAY=-1SU\nTZOFFSETFROM:+0100\nTZOFFSETTO:+0200\nEND:DAYLIGHT\n"
	two := "BEGIN:VEVENT\nUID:a\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260715T090000\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:b\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20261215T090000\nEND:VEVENT\n"
	_, events = mustParse(t, zoneCalendar("Old Zone", ancient, two))
	if !events[0].Start.Equal(utc(2026, 7, 15, 7, 0, 0)) || !events[1].Start.Equal(utc(2026, 12, 15, 8, 0, 0)) {
		t.Fatalf("rules from year 1: %v %v", events[0].Start, events[1].Start)
	}
}

// pacificRules are US Pacific rules under a name that is not an IANA or
// Windows zone.
const pacificRules = "BEGIN:STANDARD\nDTSTART:20071104T020000\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\nTZOFFSETFROM:-0700\nTZOFFSETTO:-0800\nEND:STANDARD\n" +
	"BEGIN:DAYLIGHT\nDTSTART:20070311T020000\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\nTZOFFSETFROM:-0800\nTZOFFSETTO:-0700\nEND:DAYLIGHT\n"

// A series in a zone known only from its VTIMEZONE keeps its local wall
// time across the zone's DST change, and its EXDATEs and overrides still
// name instances.
func TestExpandCustomZoneAcrossDST(t *testing.T) {
	series := "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20261019T090000\nDTEND;TZID=TZ:20261019T093000\n" +
		"RRULE:FREQ=WEEKLY;UNTIL=20261201T000000Z\nEXDATE;TZID=TZ:20261109T090000\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nRECURRENCE-ID;TZID=TZ:20261116T090000\n" +
		"DTSTART;TZID=TZ:20261116T100000\nDTEND;TZID=TZ:20261116T103000\nEND:VEVENT\n"
	data := zoneCalendar("Office Time", pacificRules, series)
	_, events := mustParse(t, data)
	if events[0].TZID != "" {
		t.Fatalf("custom zone TZID = %q, want \"\"", events[0].TZID)
	}
	want := []time.Time{
		utc(2026, 10, 19, 16, 0, 0), // PDT
		utc(2026, 10, 26, 16, 0, 0),
		utc(2026, 11, 2, 17, 0, 0),  // PST
		utc(2026, 11, 16, 18, 0, 0), // the override, at 10:00
		utc(2026, 11, 23, 17, 0, 0),
		utc(2026, 11, 30, 17, 0, 0),
	}
	insts, _ := mustExpand(t, events, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), want)

	// The rows a cache stores keep Raw, which is enough: the zone is read
	// from it again.
	stored := make([]Event, len(events))
	for i, ev := range events {
		ev.Raw = bytes.Clone(data)
		stored[i] = ev
	}
	insts, _ = mustExpand(t, stored, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), want)
}

// The calendar's own VTIMEZONE wins over tzdata for a TZID it defines,
// while Event.TZID still names the IANA zone. A TZID with no VTIMEZONE, or
// one whose VTIMEZONE has no usable observance, uses tzdata.
func TestParseICSPrefersEmbeddedZone(t *testing.T) {
	fixed := "BEGIN:STANDARD\nDTSTART:19700101T000000\nTZOFFSETFROM:+0300\nTZOFFSETTO:+0300\nEND:STANDARD\n"
	series := "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20261019T090000\nDTEND;TZID=TZ:20261019T100000\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\nEND:VEVENT\n"
	for _, tzid := range []string{"Europe/Berlin", "W. Europe Standard Time", "/mozilla.org/20050126_1/Europe/Berlin"} {
		_, events := mustParse(t, zoneCalendar(tzid, fixed, series))
		ev := events[0]
		if ev.TZID != "Europe/Berlin" || !ev.Start.Equal(utc(2026, 10, 19, 6, 0, 0)) {
			t.Errorf("%s: TZID %q start %v, want Europe/Berlin at 06:00Z", tzid, ev.TZID, ev.Start)
			continue
		}
		// Berlin leaves DST on 2026-10-25; the embedded zone does not.
		insts, _ := mustExpand(t, events, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
		sameTimes(t, starts(insts), []time.Time{utc(2026, 10, 19, 6, 0, 0), utc(2026, 10, 26, 6, 0, 0), utc(2026, 11, 2, 6, 0, 0)})
	}

	broken := "BEGIN:STANDARD\nDTSTART:19700101T000000\nTZOFFSETFROM:+0300\nEND:STANDARD\n"
	_, events := mustParse(t, zoneCalendar("Europe/Berlin", broken, series))
	if ev := events[0]; ev.TZID != "Europe/Berlin" || !ev.Start.Equal(utc(2026, 10, 19, 7, 0, 0)) {
		t.Fatalf("unusable VTIMEZONE: TZID %q start %v, want tzdata", ev.TZID, ev.Start)
	}
	insts, _ := mustExpand(t, events, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{utc(2026, 10, 19, 7, 0, 0), utc(2026, 10, 26, 8, 0, 0), utc(2026, 11, 2, 8, 0, 0)})
}

// A VTIMEZONE that lost part of its rules (here a DAILY observance rule,
// which is ignored) does not replace tzdata for a TZID tzdata knows.
func TestParseICSDegradedZoneFallsBackToTzdata(t *testing.T) {
	rules := "BEGIN:STANDARD\nDTSTART:19701025T030000\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\nTZOFFSETFROM:+0200\nTZOFFSETTO:+0100\nEND:STANDARD\n" +
		"BEGIN:DAYLIGHT\nDTSTART:19700329T020000\nRRULE:FREQ=DAILY\nTZOFFSETFROM:+0100\nTZOFFSETTO:+0200\nEND:DAYLIGHT\n"
	event := "BEGIN:VEVENT\nUID:u\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20260715T090000\nEND:VEVENT\n"
	_, events := mustParse(t, zoneCalendar("Europe/Berlin", rules, event))
	if ev := events[0]; ev.TZID != "Europe/Berlin" || !ev.Start.Equal(utc(2026, 7, 15, 7, 0, 0)) {
		t.Fatalf("degraded VTIMEZONE: TZID %q start %v, want tzdata", ev.TZID, ev.Start)
	}
}

// A UTC DTSTART with a TZID parameter is UTC, as ParseICS reads it, so the
// series recurs in UTC.
func TestExpandUTCStartIgnoresTZID(t *testing.T) {
	series := "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20261019T160000Z\nDTEND;TZID=TZ:20261019T163000Z\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\nEND:VEVENT\n"
	_, events := mustParse(t, zoneCalendar("Office Time", pacificRules, series))
	insts, _ := mustExpand(t, events, utc(2026, 10, 1, 0, 0, 0), utc(2026, 12, 1, 0, 0, 0), MaxInstances)
	sameTimes(t, starts(insts), []time.Time{utc(2026, 10, 19, 16, 0, 0), utc(2026, 10, 26, 16, 0, 0), utc(2026, 11, 2, 16, 0, 0)})
}

// The zone embeddedZone remembers for one Raw is shared by every series
// expanded from it, from any goroutine.
func TestExpandSharedZoneConcurrently(t *testing.T) {
	series := "BEGIN:VEVENT\nUID:s\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:20261019T090000\nRRULE:FREQ=WEEKLY\nEND:VEVENT\n"
	_, events := mustParse(t, zoneCalendar("Office Time", pacificRules, series))
	// The first expansion reads the zone; the goroutines share it.
	mustExpand(t, events, utc(2026, 10, 1, 0, 0, 0), utc(2026, 11, 1, 0, 0, 0), MaxInstances)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			// Each goroutine reaches different years of the zone.
			from := utc(2027+i, 1, 1, 0, 0, 0)
			insts, _, err := Expand(events, from, from.AddDate(0, 1, 0), MaxInstances)
			if err == nil && (len(insts) < 4 || insts[0].Start.Hour() != 17) {
				err = fmt.Errorf("year %d: %d instances, first %v", from.Year(), len(insts), insts)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// berlinRules are Central European rules, the same as Europe/Berlin's.
const berlinRules = "BEGIN:STANDARD\nDTSTART:19701025T030000\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\nTZOFFSETFROM:+0200\nTZOFFSETTO:+0100\nEND:STANDARD\n" +
	"BEGIN:DAYLIGHT\nDTSTART:19700329T020000\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\nTZOFFSETFROM:+0100\nTZOFFSETTO:+0200\nEND:DAYLIGHT\n"

// A local time a DST change repeats is its first occurrence, and one a
// change skips is read with the offset before the gap (RFC 5545 3.3.5),
// in a VTIMEZONE and in an IANA zone, west and east of Greenwich.
func TestParseICSRepeatedAndSkippedLocalTimes(t *testing.T) {
	cases := []struct {
		name, iana, rules, local string
		want                     time.Time
	}{
		{"pacific repeated", "America/Los_Angeles", pacificRules, "20261101T013000", utc(2026, 11, 1, 8, 30, 0)}, // PDT
		{"pacific skipped", "America/Los_Angeles", pacificRules, "20260308T023000", utc(2026, 3, 8, 10, 30, 0)},  // read at PST
		{"berlin repeated", "Europe/Berlin", berlinRules, "20261025T023000", utc(2026, 10, 25, 0, 30, 0)},        // CEST
		{"berlin skipped", "Europe/Berlin", berlinRules, "20260329T023000", utc(2026, 3, 29, 1, 30, 0)},          // read at CET
	}
	for _, c := range cases {
		event := "BEGIN:VEVENT\nUID:u\nDTSTAMP:20260101T000000Z\nDTSTART;TZID=TZ:" + c.local + "\nEND:VEVENT\n"
		_, custom := mustParse(t, zoneCalendar("Office Time", c.rules, event))
		_, tzdata := mustParse(t, []byte("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:x\n"+strings.ReplaceAll(event, "TZ:", c.iana+":")+"END:VCALENDAR\n"))
		for zone, ev := range map[string]Event{"vtimezone": custom[0], "iana": tzdata[0]} {
			if !ev.Start.Equal(c.want) {
				t.Errorf("%s/%s: start %v, want %v", c.name, zone, ev.Start, c.want)
			}
		}
	}
}
