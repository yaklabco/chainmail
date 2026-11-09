package caldav

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// berlinPlus3ICS is a weekly series in a VTIMEZONE named Europe/Berlin that
// the object defines as a fixed +03:00, unlike tzdata's Europe/Berlin
// (+01:00 in November). RFC 5545 makes the object's definition the one
// that counts, so 09:00 there is 06:00 UTC.
const berlinPlus3ICS = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Other//EN\r\n" +
	"BEGIN:VTIMEZONE\r\n" +
	"TZID:Europe/Berlin\r\n" +
	"BEGIN:STANDARD\r\n" +
	"DTSTART:19700101T000000\r\n" +
	"TZOFFSETFROM:+0300\r\n" +
	"TZOFFSETTO:+0300\r\n" +
	"END:STANDARD\r\n" +
	"END:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:berlin-1@example.com\r\n" +
	"DTSTAMP:20261101T000000Z\r\n" +
	"DTSTART;TZID=Europe/Berlin:20261110T090000\r\n" +
	"DTEND;TZID=Europe/Berlin:20261110T100000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=10\r\n" +
	"SUMMARY:Sync\r\n" +
	"ORGANIZER;CN=Bob:mailto:bob@example.com\r\n" +
	"ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED:mailto:bob@example.com\r\n" +
	"ATTENDEE;CN=Jane;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:jane@example.com\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestRespondWritesTheRecurrenceIDInTheObjectsZone(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "berlin.ics"
	f.backend.put(t, href, berlinPlus3ICS)
	s := f.store(t, WithClock(clock))
	rid := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC) // the second instance
	got, err := s.Respond(context.Background(), testCal, href, calendar.Response{
		Self: "jane@example.com", PartStat: calendar.PartStatAccepted, RecurrenceID: rid,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !got.RecurrenceID.Equal(rid) || !got.Start.Equal(rid) {
		t.Errorf("override rid %v start %v, want %v", got.RecurrenceID, got.Start, rid)
	}
	raw := f.backend.raw(t, href)
	for _, want := range []string{
		"RECURRENCE-ID;TZID=Europe/Berlin:20261117T090000",
		"DTSTART;TZID=Europe/Berlin:20261117T090000",
		"DTEND;TZID=Europe/Berlin:20261117T100000",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("stored object lacks %s:\n%s", want, raw)
		}
	}
}

func TestPutPatchWritesTimesInTheObjectsZone(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "berlin.ics"
	etag := f.backend.put(t, href, berlinPlus3ICS)
	s := f.store(t, WithClock(clock))
	evs, err := s.GetEvent(context.Background(), testCal, href)
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	ev.Start = ev.Start.Add(time.Hour) // 07:00 UTC, 10:00 in the object's zone
	ev.End = ev.End.Add(time.Hour)
	ev.ExDates = []time.Time{time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)}
	got, err := s.Put(context.Background(), testCal, ev, etag, calendar.NotifySend)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !got.Start.Equal(ev.Start) || !got.End.Equal(ev.End) {
		t.Errorf("stored start %v end %v, want %v %v", got.Start, got.End, ev.Start, ev.End)
	}
	raw := f.backend.raw(t, href)
	for _, want := range []string{
		"DTSTART;TZID=Europe/Berlin:20261110T100000",
		"DTEND;TZID=Europe/Berlin:20261110T110000",
		"EXDATE;TZID=Europe/Berlin:20261124T100000",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("stored object lacks %s:\n%s", want, raw)
		}
	}
}

func TestPutAddedOverrideUsesTheObjectsZone(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "berlin.ics"
	etag := f.backend.put(t, href, berlinPlus3ICS)
	s := f.store(t, WithClock(clock))
	evs, err := s.GetEvent(context.Background(), testCal, href)
	if err != nil {
		t.Fatal(err)
	}
	ov := evs[0]
	ov.RRule = ""
	ov.RecurrenceID = time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	ov.Start = ov.RecurrenceID.Add(time.Hour)
	ov.End = ov.Start.Add(time.Hour)
	got, err := s.Put(context.Background(), testCal, ov, etag, calendar.NotifySend)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !got.RecurrenceID.Equal(ov.RecurrenceID) || !got.Start.Equal(ov.Start) {
		t.Errorf("override rid %v start %v, want %v %v", got.RecurrenceID, got.Start, ov.RecurrenceID, ov.Start)
	}
	raw := f.backend.raw(t, href)
	for _, want := range []string{
		"RECURRENCE-ID;TZID=Europe/Berlin:20261124T090000",
		"DTSTART;TZID=Europe/Berlin:20261124T100000",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("stored object lacks %s:\n%s", want, raw)
		}
	}
}
