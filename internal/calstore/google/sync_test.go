package google_test

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/testutil/fakegcal"
)

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

// timedEvent is a single event with an organizer and two attendees.
func timedEvent(id string) map[string]any {
	return map[string]any{
		"id":           id,
		"status":       "tentative",
		"summary":      "Review",
		"description":  "Notes",
		"location":     "Room 1",
		"iCalUID":      id + "@example.org",
		"sequence":     2,
		"transparency": "transparent",
		"start":        map[string]any{"dateTime": "2026-11-10T09:00:00-08:00", "timeZone": la},
		"end":          map[string]any{"dateTime": "2026-11-10T10:00:00-08:00", "timeZone": la},
		"organizer":    map[string]any{"email": "boss@example.org", "displayName": "Boss"},
		"attendees": []any{
			map[string]any{"email": "boss@example.org", "displayName": "Boss", "organizer": true, "responseStatus": "accepted"},
			map[string]any{"email": me, "self": true, "responseStatus": "needsAction", "comment": "earlier note", "additionalGuests": 1},
			map[string]any{"email": "opt@example.org", "optional": true, "responseStatus": "tentative"},
		},
	}
}

func allDayEvent(id string) map[string]any {
	return map[string]any{
		"id":    id,
		"start": map[string]any{"date": "2026-11-12"},
		"end":   map[string]any{"date": "2026-11-14"},
	}
}

// seriesEvent is a weekly series from Tuesday 10 November 2026 at 09:00 Los
// Angeles time, with one EXDATE and one RDATE.
func seriesEvent(id string) map[string]any {
	return map[string]any{
		"id":        id,
		"summary":   "Weekly",
		"iCalUID":   id + "@example.org",
		"start":     map[string]any{"dateTime": "2026-11-10T09:00:00-08:00", "timeZone": la},
		"end":       map[string]any{"dateTime": "2026-11-10T09:30:00-08:00", "timeZone": la},
		"organizer": map[string]any{"email": "boss@example.org"},
		"attendees": []any{
			map[string]any{"email": "boss@example.org", "organizer": true, "responseStatus": "accepted"},
			map[string]any{"email": me, "self": true, "responseStatus": "needsAction"},
		},
		"recurrence": []any{
			"RRULE:FREQ=WEEKLY;BYDAY=TU",
			"EXDATE;TZID=America/Los_Angeles:20261201T090000,20261208T090000",
			"RDATE:20261111T170000Z",
		},
	}
}

func exception(master, rid, start, newStart string) map[string]any {
	return map[string]any{
		"id":                master + "_" + rid,
		"recurringEventId":  master,
		"iCalUID":           master + "@example.org",
		"summary":           "Weekly (moved)",
		"originalStartTime": map[string]any{"dateTime": start, "timeZone": la},
		"start":             map[string]any{"dateTime": newStart, "timeZone": la},
		"end":               map[string]any{"dateTime": strings.Replace(newStart, ":00:00", ":30:00", 1), "timeZone": la},
	}
}

func cancelledInstance(master, rid, start string) map[string]any {
	return map[string]any{
		"id":                master + "_" + rid,
		"recurringEventId":  master,
		"status":            "cancelled",
		"originalStartTime": map[string]any{"dateTime": start, "timeZone": la},
	}
}

func byKey(evs []calendar.Event) map[string]calendar.Event {
	m := map[string]calendar.Event{}
	for _, ev := range evs {
		key := ev.ProviderID
		if !ev.RecurrenceID.IsZero() {
			key += "@" + ev.RecurrenceID.Format("20060102T150405Z")
		}
		m[key] = ev
	}
	return m
}

func TestSyncFullThenIncremental(t *testing.T) {
	st, srv := newStore(t)
	srv.SetPageSize(2)
	srv.AddEvent(me, timedEvent("single1"))
	srv.AddEvent(me, allDayEvent("allday1"))
	srv.AddEvent(me, seriesEvent("series1"))
	srv.AddEvent(me, exception("series1", "20261117T170000Z", "2026-11-17T09:00:00-08:00", "2026-11-17T11:00:00-08:00"))
	srv.AddEvent(me, cancelledInstance("series1", "20261124T170000Z", "2026-11-24T09:00:00-08:00"))
	srv.AddEvent(me, timedEvent("gone1"))
	srv.CancelEvent(me, "gone1")

	ch, err := st.Sync(ctx(t), me, "")
	if err != nil {
		t.Fatalf("full Sync: %v", err)
	}
	if !ch.Full || ch.ObjectReplace || ch.Token == "" {
		t.Errorf("full sync: Full %v ObjectReplace %v Token %q", ch.Full, ch.ObjectReplace, ch.Token)
	}
	if !slices.Equal(ch.Deletes, []string{"gone1"}) {
		t.Errorf("Deletes = %v, want [gone1]", ch.Deletes)
	}
	got := byKey(ch.Upserts)
	if len(got) != 5 {
		t.Fatalf("got %d upserts, want 5: %v", len(got), slices.Collect(maps.Keys(got)))
	}

	for _, r := range srv.Requests() {
		if !strings.HasSuffix(r.Path, "/events") {
			continue
		}
		q := r.Query
		if q.Get("showDeleted") != "true" || q.Get("singleEvents") != "false" || q.Get("maxResults") != "250" {
			t.Errorf("events.list query = %v", q)
		}
		if q.Get("syncToken") != "" {
			t.Errorf("full sync sent a sync token: %v", q)
		}
	}

	single := got["single1"]
	if single.UID != "single1@example.org" || single.ETag == "" || single.Summary != "Review" ||
		single.Location != "Room 1" || single.Description != "Notes" || single.Status != calendar.StatusTentative ||
		single.Sequence != 2 || !single.Transparent || single.AllDay || single.TZID != la {
		t.Errorf("single = %+v", single)
	}
	if !single.Start.Equal(utc(2026, 11, 10, 17, 0)) || !single.End.Equal(utc(2026, 11, 10, 18, 0)) ||
		single.Start.Location() != time.UTC {
		t.Errorf("single times = %v to %v", single.Start, single.End)
	}
	if single.Organizer == nil || *single.Organizer != (calendar.Person{Email: "boss@example.org", Name: "Boss"}) {
		t.Errorf("organizer = %+v", single.Organizer)
	}
	wantAtt := []calendar.Attendee{
		{Person: calendar.Person{Email: "boss@example.org", Name: "Boss"}, PartStat: calendar.PartStatAccepted, Role: "REQ-PARTICIPANT"},
		{Person: calendar.Person{Email: me}, PartStat: calendar.PartStatNeedsAction, Role: "REQ-PARTICIPANT"},
		{Person: calendar.Person{Email: "opt@example.org"}, PartStat: calendar.PartStatTentative, Role: "OPT-PARTICIPANT"},
	}
	if !slices.Equal(single.Attendees, wantAtt) || single.AttendeesTruncated {
		t.Errorf("attendees = %+v (truncated %v)", single.Attendees, single.AttendeesTruncated)
	}
	if single.Updated.IsZero() {
		t.Error("updated not read")
	}
	var raw map[string]any
	if err := json.Unmarshal(single.Raw, &raw); err != nil || raw["id"] != "single1" {
		t.Errorf("Raw is not the event JSON: %v %s", err, single.Raw)
	}

	allDay := got["allday1"]
	if !allDay.AllDay || !allDay.Start.Equal(utc(2026, 11, 12, 0, 0)) || !allDay.End.Equal(utc(2026, 11, 14, 0, 0)) || allDay.TZID != "" {
		t.Errorf("all-day = %+v", allDay)
	}

	master := got["series1"]
	if !master.IsMaster() || master.RRule != "FREQ=WEEKLY;BYDAY=TU" || master.TZID != la {
		t.Errorf("master = %+v", master)
	}
	if !slices.EqualFunc(master.ExDates, []time.Time{utc(2026, 12, 1, 17, 0), utc(2026, 12, 8, 17, 0)}, time.Time.Equal) {
		t.Errorf("ExDates = %v", master.ExDates)
	}
	if !slices.EqualFunc(master.RDates, []time.Time{utc(2026, 11, 11, 17, 0)}, time.Time.Equal) {
		t.Errorf("RDates = %v", master.RDates)
	}

	moved := got["series1@20261117T170000Z"]
	if moved.ProviderID != "series1" || !moved.RecurrenceID.Equal(utc(2026, 11, 17, 17, 0)) ||
		!moved.Start.Equal(utc(2026, 11, 17, 19, 0)) || moved.IsMaster() || moved.UID != "series1@example.org" {
		t.Errorf("override = %+v", moved)
	}
	cancelled := got["series1@20261124T170000Z"]
	if cancelled.Status != calendar.StatusCancelled || cancelled.ProviderID != "series1" {
		t.Errorf("cancelled instance = %+v", cancelled)
	}

	// The series expands as Google shows it: the override moves one
	// instance and the cancelled one is gone.
	series := []calendar.Event{master, moved, cancelled}
	insts, _, err := calendar.Expand(series, utc(2026, 11, 9, 0, 0), utc(2026, 12, 16, 0, 0), 0)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var starts []string
	for _, in := range insts {
		starts = append(starts, in.Start.Format("01-02T15:04"))
	}
	if want := "11-10T17:00,11-11T17:00,11-17T19:00,12-15T17:00"; strings.Join(starts, ",") != want {
		t.Errorf("instances = %v, want %s", starts, want)
	}

	// Organizer-side changes since the token.
	srv.ChangeEvent(me, "single1", map[string]any{"summary": "Review v2"})
	srv.CancelEvent(me, "allday1")
	srv.AddEvent(me, cancelledInstance("series1", "20261215T170000Z", "2026-12-15T09:00:00-08:00"))

	inc, err := st.Sync(ctx(t), me, ch.Token)
	if err != nil {
		t.Fatalf("incremental Sync: %v", err)
	}
	if inc.Full || inc.ObjectReplace || inc.Token == "" || inc.Token == ch.Token {
		t.Errorf("incremental: Full %v ObjectReplace %v Token %q", inc.Full, inc.ObjectReplace, inc.Token)
	}
	if !slices.Equal(inc.Deletes, []string{"allday1"}) {
		t.Errorf("incremental Deletes = %v", inc.Deletes)
	}
	upd := byKey(inc.Upserts)
	if len(upd) != 2 || upd["single1"].Summary != "Review v2" || upd["series1@20261215T170000Z"].Status != calendar.StatusCancelled {
		t.Errorf("incremental upserts = %+v", inc.Upserts)
	}
	last, _ := srv.LastRequest("GET")
	if last.Query.Get("syncToken") != ch.Token || last.Query.Get("showDeleted") != "true" {
		t.Errorf("incremental query = %v", last.Query)
	}

	// Nothing changed: no upserts, a token still.
	again, err := st.Sync(ctx(t), me, inc.Token)
	if err != nil || len(again.Upserts) != 0 || len(again.Deletes) != 0 || again.Token == "" {
		t.Errorf("quiet sync: %+v, %v", again, err)
	}
}

func TestSyncTokenInvalid(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, timedEvent("single1"))
	ch, err := st.Sync(ctx(t), me, "")
	if err != nil {
		t.Fatal(err)
	}
	srv.InvalidateSyncTokens()
	if _, err := st.Sync(ctx(t), me, ch.Token); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("invalidated token: err = %v, want ErrSyncTokenInvalid", err)
	}
	if _, err := st.Sync(ctx(t), me, "not-a-token"); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("unknown token: err = %v, want ErrSyncTokenInvalid", err)
	}
	full, err := st.Sync(ctx(t), me, "")
	if err != nil || len(full.Upserts) != 1 {
		t.Errorf("full sync after invalidation: %+v, %v", full, err)
	}
}

func TestSyncTokenInvalidMidPaging(t *testing.T) {
	st, srv := newStore(t)
	srv.SetPageSize(1)
	srv.AddEvent(me, timedEvent("single1"))
	srv.AddEvent(me, timedEvent("single2"))
	srv.Inject(fakegcal410OnPage())
	if _, err := st.Sync(ctx(t), me, ""); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("410 on a later page: err = %v, want ErrSyncTokenInvalid", err)
	}
}

func TestSyncAttendeeLimits(t *testing.T) {
	st, srv := newStore(t)
	many := timedEvent("big1")
	var atts []any
	for i := range calendar.MaxAttendees + 1 {
		atts = append(atts, map[string]any{"email": "p" + itoa(i) + "@example.org", "responseStatus": "needsAction"})
	}
	many["attendees"] = atts
	srv.AddEvent(me, many)
	omitted := timedEvent("omit1")
	omitted["attendeesOmitted"] = true
	srv.AddEvent(me, omitted)
	ch, err := st.Sync(ctx(t), me, "")
	if err != nil {
		t.Fatal(err)
	}
	got := byKey(ch.Upserts)
	if b := got["big1"]; len(b.Attendees) != calendar.MaxAttendees || !b.AttendeesTruncated {
		t.Errorf("501 attendees: kept %d, truncated %v", len(b.Attendees), b.AttendeesTruncated)
	}
	if o := got["omit1"]; !o.AttendeesTruncated {
		t.Error("attendeesOmitted not reported as truncated")
	}
}

func TestSyncUnknownCalendar(t *testing.T) {
	st, _ := newStore(t)
	if _, err := st.Sync(ctx(t), "nobody@example.com", ""); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("unknown calendar: err = %v, want ErrNotFound", err)
	}
}

// fakegcal410OnPage answers the first request for a later page of
// events.list with 410.
func fakegcal410OnPage() fakegcal.Fault {
	return fakegcal.Fault{
		Method: "GET", Path: "/events", Query: "pageToken", Status: 410,
		Body: `{"error":{"code":410,"errors":[{"reason":"fullSyncRequired"}]}}`,
	}
}

func TestSyncDeletedSeriesDropsItsOverrides(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, seriesEvent("series1"))
	srv.AddEvent(me, exception("series1", "20261117T170000Z", "2026-11-17T09:00:00-08:00", "2026-11-17T11:00:00-08:00"))
	srv.AddEvent(me, timedEvent("single1"))
	ch, err := st.Sync(ctx(t), me, "")
	if err != nil {
		t.Fatal(err)
	}
	// Deleting a series in Google Calendar cancels the master and its
	// exceptions; the exceptions come back as cancelled instances.
	srv.CancelEvent(me, "series1")
	srv.CancelEvent(me, "series1_20261117T170000Z")
	inc, err := st.Sync(ctx(t), me, ch.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(inc.Deletes, []string{"series1"}) {
		t.Errorf("Deletes = %v, want [series1]", inc.Deletes)
	}
	for _, ev := range inc.Upserts {
		if ev.ProviderID == "series1" {
			t.Errorf("upsert of a row of a deleted series: %+v", ev)
		}
	}
}
