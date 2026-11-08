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

func TestRespondToASeries(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "series.ics"
	etag := f.backend.put(t, href, seriesICS)
	s := f.store(t, WithClock(clock))
	got, err := s.Respond(context.Background(), testCal, href, calendar.Response{
		Self: "Jane@Example.com", PartStat: calendar.PartStatAccepted, Comment: "See you\x07 there",
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !got.IsMaster() || got.ETag != f.backend.etag(href) {
		t.Errorf("result master %v etag %q", got.IsMaster(), got.ETag)
	}
	var mine calendar.PartStat
	for _, a := range got.Attendees {
		if a.Email == "jane@example.com" {
			mine = a.PartStat
		}
	}
	if mine != calendar.PartStatAccepted {
		t.Errorf("self PARTSTAT %q", mine)
	}
	puts := f.seen(http.MethodPut)
	if len(puts) != 1 || puts[0].header.Get("If-Match") != etag {
		t.Fatalf("PUTs %+v, want one with If-Match %s", puts, etag)
	}
	raw := f.backend.raw(t, href)
	if line(raw, "COMMENT") != "COMMENT:See you there" {
		t.Errorf("COMMENT line %q", line(raw, "COMMENT"))
	}
	// Only the master changed: the override still waits for an answer.
	accepted := 0
	for _, a := range lines(raw, "ATTENDEE") {
		if strings.Contains(a, "jane@") && strings.Contains(a, "PARTSTAT=ACCEPTED") {
			accepted++
			if !strings.Contains(a, "SCHEDULE-STATUS=1.2") {
				t.Errorf("attendee lost its parameters: %s", a)
			}
		}
	}
	if accepted != 1 {
		t.Errorf("%d accepted attendee lines for jane, want 1:\n%s", accepted, raw)
	}
}

func TestRespondToAnInstance(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "series.ics"
	f.backend.put(t, href, seriesICS)
	s := f.store(t, WithClock(clock))
	ny, _ := time.LoadLocation("America/New_York")

	// The third instance has no override yet.
	rid := time.Date(2026, 11, 16, 9, 0, 0, 0, ny).UTC()
	got, err := s.Respond(context.Background(), testCal, href, calendar.Response{
		Self: "jane@example.com", PartStat: calendar.PartStatDeclined, RecurrenceID: rid,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !got.RecurrenceID.Equal(rid) || !got.Start.Equal(rid) || got.End.Sub(got.Start) != time.Hour || got.RRule != "" {
		t.Errorf("override rid %v start %v end %v rrule %q", got.RecurrenceID, got.Start, got.End, got.RRule)
	}
	raw := f.backend.raw(t, href)
	if strings.Count(raw, "BEGIN:VEVENT") != 3 {
		t.Fatalf("events: %d, want 3:\n%s", strings.Count(raw, "BEGIN:VEVENT"), raw)
	}
	if !strings.Contains(raw, "RECURRENCE-ID;TZID=America/New_York:20261116T090000") {
		t.Errorf("no RECURRENCE-ID in the master's form:\n%s", raw)
	}
	if strings.Count(raw, "BEGIN:VALARM") != 2 {
		t.Errorf("the new override did not copy the alarm:\n%s", raw)
	}
	if strings.Count(raw, "RRULE:FREQ=WEEKLY") != 1 {
		t.Errorf("the override copied the RRULE:\n%s", raw)
	}

	// The moved instance has an override: it is answered there.
	moved := time.Date(2026, 11, 9, 9, 0, 0, 0, ny).UTC()
	if _, err := s.Respond(context.Background(), testCal, href, calendar.Response{
		Self: "jane@example.com", PartStat: calendar.PartStatTentative, RecurrenceID: moved,
	}); err != nil {
		t.Fatal(err)
	}
	raw = f.backend.raw(t, href)
	if strings.Count(raw, "BEGIN:VEVENT") != 3 || !strings.Contains(raw, "PARTSTAT=TENTATIVE") {
		t.Errorf("moved instance:\n%s", raw)
	}

	// A time that is no instance.
	_, err = s.Respond(context.Background(), testCal, href, calendar.Response{
		Self: "jane@example.com", PartStat: calendar.PartStatAccepted, RecurrenceID: rid.Add(time.Hour),
	})
	if !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("not an instance: err = %v, want ErrNotFound", err)
	}
}

func TestRespondRefusals(t *testing.T) {
	f := newFakeServer(t)
	href := testCal + "series.ics"
	f.backend.put(t, href, seriesICS)
	s := f.store(t, WithClock(clock))
	ctx := context.Background()
	if _, err := s.Respond(ctx, testCal, href, calendar.Response{Self: "carol@example.com", PartStat: calendar.PartStatAccepted}); !errors.Is(err, ErrNotAttendee) {
		t.Errorf("stranger: err = %v, want ErrNotAttendee", err)
	}
	if _, err := s.Respond(ctx, testCal, href, calendar.Response{Self: "jane@example.com", PartStat: calendar.PartStatNeedsAction}); !errors.Is(err, calendar.ErrInvalidICS) {
		t.Errorf("NEEDS-ACTION: err = %v, want ErrInvalidICS", err)
	}
	if _, err := s.Respond(ctx, testCal, testCal+"missing.ics", calendar.Response{Self: "jane@example.com", PartStat: calendar.PartStatAccepted}); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	if len(f.seen(http.MethodPut)) != 0 {
		t.Error("a refused response was written")
	}

	// The object changes between the read and the write.
	f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusPreconditionFailed)
			return true
		}
		return false
	}
	if _, err := s.Respond(ctx, testCal, href, calendar.Response{Self: "jane@example.com", PartStat: calendar.PartStatAccepted}); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("concurrent change: err = %v, want ErrConflict", err)
	}
}
