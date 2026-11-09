package calcache_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
)

// t0 is a fixed instant the tests build events around.
var t0 = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

const me = "james@example.com"

// self reports whether addr is one of James's addresses.
func self(addr string) bool {
	return strings.EqualFold(addr, me) || strings.EqualFold(addr, "j@alias.example")
}

// single is a single event with ProviderID and UID p.
func single(p string, start, end time.Time) calendar.Event {
	return calendar.Event{
		ProviderID: p,
		ETag:       "e-" + p,
		UID:        p + "@example.com",
		Summary:    "summary of " + p,
		Start:      start,
		End:        end,
		Status:     calendar.StatusConfirmed,
		Updated:    t0,
		Raw:        []byte("raw " + p),
	}
}

// invited is a single event organized by someone else with James invited.
func invited(p string, start time.Time, ps calendar.PartStat) calendar.Event {
	ev := single(p, start, start.Add(time.Hour))
	ev.Organizer = &calendar.Person{Email: "boss@example.com", Name: "Boss"}
	ev.Attendees = []calendar.Attendee{
		{Person: calendar.Person{Email: "boss@example.com", Name: "Boss"}, PartStat: calendar.PartStatAccepted, Role: "CHAIR"},
		{Person: calendar.Person{Email: "JAMES@example.com", Name: "James"}, PartStat: ps, Role: "REQ-PARTICIPANT", RSVP: true},
	}
	return ev
}

// weekly is a weekly master with ProviderID p starting at start.
func weekly(p string, start time.Time) calendar.Event {
	ev := single(p, start, start.Add(30*time.Minute))
	ev.RRule = "FREQ=WEEKLY"
	ev.TZID = "America/Los_Angeles"
	ev.ExDates = []time.Time{start.AddDate(0, 0, 14)}
	return ev
}

// override is an override of master at rid, moved by shift.
func override(master calendar.Event, rid time.Time, shift time.Duration) calendar.Event {
	ev := master
	ev.RRule = ""
	ev.ExDates = nil
	ev.RecurrenceID = rid
	ev.Start = rid.Add(shift)
	ev.End = ev.Start.Add(master.End.Sub(master.Start))
	ev.Summary = "moved"
	return ev
}

// setup stores calendars work (write, default) and home (read) for acme.
func setup(t *testing.T) *calcache.Store {
	t.Helper()
	s, _ := openCache(t)
	cals := []calendar.Calendar{
		{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true, TimeZone: "Europe/London"},
		{ID: "home", Name: "Home", Access: calendar.AccessRead},
	}
	if err := s.SetCalendars(t.Context(), "acme", cals, "work"); err != nil {
		t.Fatalf("SetCalendars: %v", err)
	}
	return s
}

// apply writes ch to acme's calendar calID, failing the test on error.
func apply(t *testing.T, s *calcache.Store, calID string, ch calendar.Changes) {
	t.Helper()
	if err := s.Apply(t.Context(), "acme", calID, ch, self, t0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// keys lists each row as "provider" or "provider@rid".
func keys(rows []calcache.EventRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		k := r.Event.ProviderID
		if !r.Event.RecurrenceID.IsZero() {
			k += "@" + r.Event.RecurrenceID.Format("20060102T150405Z")
		}
		out = append(out, k)
	}
	return out
}

// same fails the test when got is not deeply equal to want.
func same(t *testing.T, what string, want, got any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}
