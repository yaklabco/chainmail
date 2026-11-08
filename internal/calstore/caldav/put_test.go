package caldav

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

func newMeeting(uid string) calendar.Event {
	ny, _ := time.LoadLocation("America/New_York")
	return calendar.Event{
		UID:       uid,
		Summary:   "Planning",
		Location:  "Room 4",
		Start:     time.Date(2026, 11, 20, 10, 0, 0, 0, ny).UTC(),
		End:       time.Date(2026, 11, 20, 11, 0, 0, 0, ny).UTC(),
		TZID:      "America/New_York",
		Status:    calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: "jane@example.com", Name: "Jane"},
		Attendees: []calendar.Attendee{
			{Person: calendar.Person{Email: "jane@example.com", Name: "Jane"}, PartStat: calendar.PartStatAccepted},
			{Person: calendar.Person{Email: "bob@example.com", Name: "Bob"}, PartStat: calendar.PartStatNeedsAction, RSVP: true, Role: "REQ-PARTICIPANT"},
		},
	}
}

func TestPutCreate(t *testing.T) {
	f := newFakeServer(t)
	s := f.store(t, WithClock(clock))
	ctx := context.Background()
	uid := "0f3a@chainmail"
	href := testCal + "0f3a@chainmail.ics"

	got, err := s.Put(ctx, testCal, newMeeting(uid), "", calendar.NotifySend)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got.ProviderID != href || got.ETag != f.backend.etag(href) || got.UID != uid || got.Summary != "Planning" {
		t.Errorf("created event = ProviderID %q ETag %q UID %q Summary %q", got.ProviderID, got.ETag, got.UID, got.Summary)
	}
	puts := f.seen(http.MethodPut)
	if len(puts) != 1 || puts[0].path != href || puts[0].header.Get("If-None-Match") != "*" || puts[0].header.Get("If-Match") != "" {
		t.Fatalf("PUTs = %+v, want one to %s with If-None-Match: *", puts, href)
	}
	if ct := puts[0].header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Errorf("Content-Type %q", ct)
	}
	body := puts[0].body
	if strings.Contains(body, "METHOD:") {
		t.Error("a stored object carries METHOD")
	}
	if !strings.Contains(body, "BEGIN:VTIMEZONE") || line(body, "TZID:") != "TZID:America/New_York" {
		t.Errorf("no VTIMEZONE for the event's zone:\n%s", body)
	}
	if l := line(body, "DTSTART;"); l != "DTSTART;TZID=America/New_York:20261120T100000" {
		t.Errorf("DTSTART line %q", l)
	}
	if strings.Contains(body, "SCHEDULE-AGENT") {
		t.Error("a notifying create set SCHEDULE-AGENT")
	}
	for _, l := range strings.Split(body, "\r\n") {
		if len(l) > 75 {
			t.Errorf("unfolded line of %d octets: %q", len(l), l)
		}
	}

	// A retry of the same create finds the object.
	if _, err := s.Put(ctx, testCal, newMeeting(uid), "", calendar.NotifySend); !errors.Is(err, calstore.ErrExists) {
		t.Errorf("second create: err = %v, want ErrExists", err)
	}
}

func TestPutCreateWithoutNotifications(t *testing.T) {
	f := newFakeServer(t)
	s := f.store(t, WithClock(clock))
	if _, err := s.Put(context.Background(), testCal, newMeeting("held@chainmail"), "", calendar.NotifyNone); err != nil {
		t.Fatal(err)
	}
	raw := f.backend.raw(t, testCal+"held@chainmail.ics")
	atts := lines(raw, "ATTENDEE")
	if len(atts) != 2 {
		t.Fatalf("attendees %v", atts)
	}
	for _, a := range atts {
		if !strings.Contains(a, "SCHEDULE-AGENT=CLIENT") {
			t.Errorf("attendee without SCHEDULE-AGENT=CLIENT: %s", a)
		}
	}
}

func TestPutCreateEscapesTheUID(t *testing.T) {
	f := newFakeServer(t)
	s := f.store(t, WithClock(clock))
	ev := newMeeting("a/b c@chainmail")
	got, err := s.Put(context.Background(), testCal, ev, "", calendar.NotifySend)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderID != testCal+"a%2Fb%20c@chainmail.ics" {
		t.Errorf("ProviderID %q", got.ProviderID)
	}
}

func TestPutReplacePatchesTheStoredObject(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "series.ics"
	etag := f.backend.put(t, href, seriesICS)
	s := f.store(t, WithClock(clock))
	ctx := context.Background()
	evs, err := s.GetEvent(ctx, testCal, href)
	if err != nil {
		t.Fatal(err)
	}
	master := evs[0]
	ev := master
	ev.Summary = "Standup (new name)"
	ev.Start = master.Start.Add(30 * time.Minute)
	ev.End = master.End.Add(30 * time.Minute)
	ev.Attendees = append(ev.Attendees, calendar.Attendee{Person: calendar.Person{Email: "carol@example.com"}, PartStat: calendar.PartStatNeedsAction})

	got, err := s.Put(ctx, testCal, ev, etag, calendar.NotifySend)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got.Summary != "Standup (new name)" || got.ETag != f.backend.etag(href) || got.Sequence != 3 {
		t.Errorf("result Summary %q ETag %q Sequence %d", got.Summary, got.ETag, got.Sequence)
	}
	puts := f.seen(http.MethodPut)
	if len(puts) != 1 || puts[0].header.Get("If-Match") != etag {
		t.Fatalf("PUTs = %+v, want one with If-Match %s", puts, etag)
	}
	raw := f.backend.raw(t, href)
	for _, want := range []string{
		"X-OTHER-CLIENT:keep me",
		"BEGIN:VALARM",
		"SUMMARY:Standup (new name)",
		"SUMMARY:Standup (moved)",
		"DTSTART;TZID=America/New_York:20261102T093000",
		"RECURRENCE-ID;TZID=America/New_York:20261109T090000",
		"SEQUENCE:3",
		"RRULE:FREQ=WEEKLY;COUNT=10",
		"mailto:carol@example.com",
	} {
		if !strings.Contains(unfoldICS(raw), want) {
			t.Errorf("stored object lacks %q:\n%s", want, raw)
		}
	}
	jane := ""
	for _, a := range lines(raw, "ATTENDEE") {
		if strings.Contains(a, "jane@") && strings.Contains(a, "SCHEDULE-STATUS") {
			jane = a
		}
	}
	if jane == "" {
		t.Errorf("the master's attendee lost its server parameters:\n%s", raw)
	}
	if strings.Count(raw, "BEGIN:VEVENT") != 2 {
		t.Errorf("events in the object: %d", strings.Count(raw, "BEGIN:VEVENT"))
	}

	// The etag read before is stale now.
	if _, err := s.Put(ctx, testCal, ev, etag, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("stale etag: err = %v, want ErrConflict", err)
	}
}

func TestPutReplaceReadsTheObjectWithoutRaw(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "single.ics"
	etag := f.backend.put(t, href, singleICS)
	s := f.store(t, WithClock(clock))
	evs, err := s.GetEvent(context.Background(), testCal, href)
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	ev.Raw = nil
	ev.Location = "Clinic"
	if _, err := s.Put(context.Background(), testCal, ev, etag, calendar.NotifyNone); err != nil {
		t.Fatal(err)
	}
	raw := f.backend.raw(t, href)
	if line(raw, "LOCATION") != "LOCATION:Clinic" || line(raw, "SUMMARY") != "SUMMARY:Dentist" {
		t.Errorf("stored:\n%s", raw)
	}
	// A time-free change does not raise SEQUENCE.
	if line(raw, "SEQUENCE") != "SEQUENCE:0" {
		t.Errorf("SEQUENCE line %q", line(raw, "SEQUENCE"))
	}
	if _, err := s.Put(context.Background(), testCal, ev, `"stale"`, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("stale etag without raw: err = %v, want ErrConflict", err)
	}
}

func TestPutReplaceAddsAnOverride(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "series.ics"
	etag := f.backend.put(t, href, seriesICS)
	s := f.store(t, WithClock(clock))
	evs, err := s.GetEvent(context.Background(), testCal, href)
	if err != nil {
		t.Fatal(err)
	}
	ov := evs[0]
	ov.RRule = ""
	ov.RecurrenceID = evs[0].Start.AddDate(0, 0, 14)
	ov.Start = ov.RecurrenceID.Add(time.Hour)
	ov.End = ov.Start.Add(time.Hour)
	ov.Summary = "Standup (late)"
	got, err := s.Put(context.Background(), testCal, ov, etag, calendar.NotifySend)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RecurrenceID.Equal(ov.RecurrenceID) || got.Summary != "Standup (late)" {
		t.Errorf("override = rid %v summary %q", got.RecurrenceID, got.Summary)
	}
	if n := strings.Count(f.backend.raw(t, href), "BEGIN:VEVENT"); n != 3 {
		t.Errorf("events in the object: %d, want 3", n)
	}
}

func TestPutRefusesBadInput(t *testing.T) {
	f := newFakeServer(t)
	s := f.store(t, WithClock(clock))
	ev := newMeeting("x@chainmail")
	ev.ProviderID = "/other/x.ics"
	if _, err := s.Put(context.Background(), testCal, ev, `"1"`, calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("replace outside the calendar: err = %v", err)
	}
	ev = newMeeting("")
	if _, err := s.Put(context.Background(), testCal, ev, "", calendar.NotifySend); !errors.Is(err, calendar.ErrInvalidICS) {
		t.Errorf("create without UID: err = %v", err)
	}
	ev = newMeeting("x@chainmail")
	ev.Summary = "hi\r\nATTENDEE:mailto:evil@example.com"
	if _, err := s.Put(context.Background(), testCal, ev, "", calendar.NotifySend); err != nil {
		t.Fatal(err)
	}
	if raw := f.backend.raw(t, testCal+"x@chainmail.ics"); line(raw, "ATTENDEE:mailto:evil") != "" || len(lines(raw, "ATTENDEE")) != 2 {
		t.Errorf("a summary injected a property:\n%s", raw)
	}
}

func TestPutReadOnlyCalendar(t *testing.T) {
	f := newFakeServer(t)
	f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodPut {
			return false
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<d:error xmlns:d="DAV:"><d:need-privileges/></d:error>`))
		return true
	}
	_, err := f.store(t).Put(context.Background(), testCal, newMeeting("ro@chainmail"), "", calendar.NotifySend)
	if !errors.Is(err, calstore.ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "single.ics"
	etag := f.backend.put(t, href, singleICS)
	s := f.store(t, WithClock(clock))
	ctx := context.Background()

	if err := s.Delete(ctx, testCal, href, `"stale"`, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("stale etag: err = %v, want ErrConflict", err)
	}
	if err := s.Delete(ctx, testCal, href, "", calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("no etag: err = %v, want ErrConflict", err)
	}
	if err := s.Delete(ctx, testCal, href, etag, calendar.NotifySend); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.backend.raw(t, href) != "" {
		t.Error("object still there")
	}
	if err := s.Delete(ctx, testCal, href, etag, calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteWithoutNotifications(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "series.ics"
	etag := f.backend.put(t, href, seriesICS)
	s := f.store(t, WithClock(clock))
	var putBody string
	f.intercept = func(_ http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == http.MethodPut {
			putBody = string(body)
		}
		return false
	}
	if err := s.Delete(context.Background(), testCal, href, etag, calendar.NotifyNone); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, a := range lines(putBody, "ATTENDEE") {
		if !strings.Contains(a, "SCHEDULE-AGENT=CLIENT") {
			t.Errorf("attendee still scheduled by the server before the delete: %s", a)
		}
	}
	puts := f.seen(http.MethodPut)
	dels := f.seen(http.MethodDelete)
	if len(puts) != 1 || puts[0].header.Get("If-Match") != etag {
		t.Fatalf("PUTs %+v", puts)
	}
	if len(dels) != 1 || dels[0].header.Get("Schedule-Reply") != "F" || dels[0].header.Get("If-Match") == etag {
		t.Errorf("DELETE %+v, want Schedule-Reply F and the new etag", dels)
	}
	if f.backend.raw(t, href) != "" {
		t.Error("object still there")
	}
}

// longParamICS is singleICS with an attendee whose CN is longer than
// calendar.ParseICS accepts, scheduled by the client so that a silent
// delete would otherwise go ahead.
var longParamICS = strings.Replace(singleICS, "SUMMARY:Dentist\r\n",
	"SUMMARY:Dentist\r\nATTENDEE;SCHEDULE-AGENT=CLIENT;CN="+strings.Repeat("x", 3000)+":mailto:bob@example.com\r\n", 1)

func TestDeleteWithoutNotificationsGuardsTheParser(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "long.ics"
	etag := f.backend.put(t, href, longParamICS)
	s := f.store(t, WithClock(clock))
	err := s.Delete(context.Background(), testCal, href, etag, calendar.NotifyNone)
	if !errors.Is(err, calendar.ErrInvalidICS) {
		t.Errorf("silent delete of an object with a long parameter: err = %v, want ErrInvalidICS", err)
	}
	if n := len(f.seen(http.MethodPut)) + len(f.seen(http.MethodDelete)); n != 0 {
		t.Errorf("%d writes sent for an object the parser refuses", n)
	}
	if f.backend.raw(t, href) == "" {
		t.Error("object deleted")
	}
}
