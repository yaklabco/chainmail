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
	"github.com/yaklabco/chainmail/internal/calstore/google"
	"github.com/yaklabco/chainmail/internal/testutil/fakegcal"
)

// newEvent is an event chainmail creates: a weekly series of three in Los
// Angeles time with one EXDATE and two attendees.
func newEvent(uid string) calendar.Event {
	return calendar.Event{
		UID:         uid,
		Summary:     "Plan",
		Location:    "Room 2",
		Description: "Agenda",
		Start:       utc(2026, 11, 10, 17, 0),
		End:         utc(2026, 11, 10, 18, 0),
		TZID:        la,
		Attendees: []calendar.Attendee{
			{Person: calendar.Person{Email: "ann@example.org", Name: "Ann"}, PartStat: calendar.PartStatNeedsAction, Role: "REQ-PARTICIPANT"},
			{Person: calendar.Person{Email: "opt@example.org"}, PartStat: calendar.PartStatNeedsAction, Role: "OPT-PARTICIPANT"},
		},
		RRule:   "FREQ=WEEKLY;COUNT=3",
		ExDates: []time.Time{utc(2026, 11, 17, 17, 0)},
	}
}

func body(t *testing.T, r fakegcal.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("request body is not JSON: %v: %s", err, r.Body)
	}
	return m
}

func keys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

func countMethod(srv *fakegcal.Server, method string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

func TestGetEvent(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, timedEvent("single1"))
	srv.AddEvent(me, seriesEvent("series1"))
	srv.AddEvent(me, exception("series1", "20261117T170000Z", "2026-11-17T09:00:00-08:00", "2026-11-17T11:00:00-08:00"))
	srv.AddEvent(me, cancelledInstance("series1", "20261124T170000Z", "2026-11-24T09:00:00-08:00"))
	srv.AddEvent(me, seriesEvent("series2"))

	one, err := st.GetEvent(ctx(t), me, "single1")
	if err != nil || len(one) != 1 || one[0].ProviderID != "single1" || one[0].Summary != "Review" {
		t.Fatalf("single: %+v, %v", one, err)
	}

	series, err := st.GetEvent(ctx(t), me, "series1")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(series) != 3 || !series[0].IsMaster() {
		t.Fatalf("series = %+v, want the master first and two overrides", series)
	}
	got := byKey(series)
	if _, ok := got["series1@20261117T170000Z"]; !ok {
		t.Errorf("moved override missing: %v", slices.Collect(maps.Keys(got)))
	}
	if c := got["series1@20261124T170000Z"]; c.Status != calendar.StatusCancelled || c.UID != "series1@example.org" {
		t.Errorf("cancelled override = %+v", c)
	}
	for _, ev := range series {
		if ev.ProviderID != "series1" {
			t.Errorf("event of another series returned: %+v", ev)
		}
	}

	if _, err := st.GetEvent(ctx(t), me, "nothere1"); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("unknown event: err = %v, want ErrNotFound", err)
	}
}

func TestPutCreate(t *testing.T) {
	st, srv := newStore(t)
	ev := newEvent("abc123@chainmail")
	got, err := st.Put(ctx(t), me, ev, "", calendar.NotifySend)
	if err != nil {
		t.Fatalf("Put create: %v", err)
	}
	req, _ := srv.LastRequest("POST")
	if req.Path != "/calendars/"+me+"/events" || req.Query.Get("sendUpdates") != "all" {
		t.Errorf("create request %s %v", req.Path, req.Query)
	}
	b := body(t, req)
	if b["id"] != "2ift10ggkeebhjm18561uc205u885lcc" {
		t.Errorf("create id = %v", b["id"])
	}
	if _, ok := b["iCalUID"]; ok {
		t.Error("create sent iCalUID as well as id")
	}
	if b["summary"] != "Plan" || b["location"] != "Room 2" || b["description"] != "Agenda" {
		t.Errorf("create text fields = %v", b)
	}
	start := b["start"].(map[string]any)
	if start["dateTime"] != "2026-11-10T09:00:00-08:00" || start["timeZone"] != la {
		t.Errorf("start = %v", start)
	}
	rec := b["recurrence"].([]any)
	if len(rec) != 2 || rec[0] != "RRULE:FREQ=WEEKLY;COUNT=3" || rec[1] != "EXDATE;TZID=America/Los_Angeles:20261117T090000" {
		t.Errorf("recurrence = %v", rec)
	}
	atts := b["attendees"].([]any)
	if len(atts) != 2 {
		t.Fatalf("attendees = %v", atts)
	}
	ann, opt := atts[0].(map[string]any), atts[1].(map[string]any)
	if ann["email"] != "ann@example.org" || ann["displayName"] != "Ann" || ann["optional"] != nil {
		t.Errorf("ann = %v", ann)
	}
	if opt["email"] != "opt@example.org" || opt["optional"] != true {
		t.Errorf("optional attendee = %v", opt)
	}

	if got.ProviderID != "2ift10ggkeebhjm18561uc205u885lcc" || got.ETag == "" || got.RRule != "FREQ=WEEKLY;COUNT=3" ||
		got.Organizer == nil || got.Organizer.Email != me || len(got.Raw) == 0 || got.TZID != la {
		t.Errorf("created event = %+v", got)
	}
	if !got.Start.Equal(ev.Start) || !got.End.Equal(ev.End) {
		t.Errorf("created times %v to %v", got.Start, got.End)
	}

	// A retry after a crash finds the object it made.
	if _, err := st.Put(ctx(t), me, ev, "", calendar.NotifySend); !errors.Is(err, calstore.ErrExists) {
		t.Errorf("second create: err = %v, want ErrExists", err)
	}
}

func TestPutCreateDraftSendsNoUpdates(t *testing.T) {
	st, srv := newStore(t)
	ev := calendar.Event{
		UID: "draft@chainmail", Summary: "Offsite", AllDay: true,
		Start: utc(2026, 11, 20, 0, 0), End: utc(2026, 11, 21, 0, 0),
		Attendees: []calendar.Attendee{{Person: calendar.Person{Email: "ann@example.org"}}},
	}
	got, err := st.Put(ctx(t), me, ev, "", calendar.NotifyNone)
	if err != nil {
		t.Fatalf("draft create: %v", err)
	}
	req, _ := srv.LastRequest("POST")
	if req.Query.Get("sendUpdates") != "none" {
		t.Errorf("draft create sendUpdates = %q, want none", req.Query.Get("sendUpdates"))
	}
	b := body(t, req)
	if s := b["start"].(map[string]any); s["date"] != "2026-11-20" || s["dateTime"] != nil {
		t.Errorf("all-day start = %v", s)
	}
	if e := b["end"].(map[string]any); e["date"] != "2026-11-21" {
		t.Errorf("all-day end = %v", e)
	}
	if !got.AllDay || !got.Start.Equal(ev.Start) {
		t.Errorf("created all-day = %+v", got)
	}
}

func TestPutCreateRefusals(t *testing.T) {
	st, srv := newStore(t)
	if _, err := st.Put(ctx(t), readOnly, newEvent("ro@chainmail"), "", calendar.NotifyNone); !errors.Is(err, calstore.ErrReadOnly) {
		t.Errorf("create on a reader calendar: err = %v, want ErrReadOnly", err)
	}
	before := len(srv.Requests())
	noUID := newEvent("")
	if _, err := st.Put(ctx(t), me, noUID, "", calendar.NotifyNone); !errors.Is(err, google.ErrRequest) {
		t.Errorf("create without UID: err = %v, want ErrRequest", err)
	}
	override := newEvent("o@chainmail")
	override.RecurrenceID = utc(2026, 11, 17, 17, 0)
	if _, err := st.Put(ctx(t), me, override, "", calendar.NotifyNone); !errors.Is(err, google.ErrRequest) {
		t.Errorf("create of an override: err = %v, want ErrRequest", err)
	}
	badRule := newEvent("r@chainmail")
	badRule.RRule = "FREQ=DAILY\r\nEXDATE:20261110T000000Z"
	if _, err := st.Put(ctx(t), me, badRule, "", calendar.NotifyNone); !errors.Is(err, google.ErrRequest) {
		t.Errorf("create with a line break in RRULE: err = %v, want ErrRequest", err)
	}
	if n := len(srv.Requests()); n != before {
		t.Errorf("refused creates sent %d requests", n-before)
	}
}

func getOne(t *testing.T, st *google.Store, id string) calendar.Event {
	t.Helper()
	evs, err := st.GetEvent(ctx(t), me, id)
	if err != nil || len(evs) == 0 {
		t.Fatalf("GetEvent %s: %v", id, err)
	}
	return evs[0]
}

func TestPutUpdate(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, timedEvent("single1"))
	ev := getOne(t, st, "single1")

	ev.Summary = "Review v2"
	got, err := st.Put(ctx(t), me, ev, ev.ETag, calendar.NotifySend)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	req, _ := srv.LastRequest("PATCH")
	if req.Path != "/calendars/"+me+"/events/single1" || req.IfMatch != ev.ETag || req.Query.Get("sendUpdates") != "all" {
		t.Errorf("update request %s If-Match %q query %v", req.Path, req.IfMatch, req.Query)
	}
	if k := keys(body(t, req)); !slices.Equal(k, []string{"summary"}) {
		t.Errorf("update of the summary patched %v", k)
	}
	if got.Summary != "Review v2" || got.ETag == ev.ETag {
		t.Errorf("updated event = %+v", got)
	}

	// The etag read before the first update is stale now.
	if _, err := st.Put(ctx(t), me, ev, ev.ETag, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("update with a stale etag: err = %v, want ErrConflict", err)
	}

	// Moving the event and adding an attendee patches those fields only,
	// and keeps what Google knows of the attendees already there.
	got.Start = got.Start.Add(time.Hour)
	got.End = got.End.Add(time.Hour)
	got.Attendees = append(got.Attendees, calendar.Attendee{Person: calendar.Person{Email: "new@example.org"}, PartStat: calendar.PartStatNeedsAction})
	moved, err := st.Put(ctx(t), me, got, got.ETag, calendar.NotifyNone)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	req, _ = srv.LastRequest("PATCH")
	b := body(t, req)
	if k := keys(b); !slices.Equal(k, []string{"attendees", "end", "start"}) {
		t.Errorf("move patched %v", k)
	}
	if req.Query.Get("sendUpdates") != "none" {
		t.Errorf("move sendUpdates = %q", req.Query.Get("sendUpdates"))
	}
	atts := b["attendees"].([]any)
	self := atts[1].(map[string]any)
	if self["comment"] != "earlier note" || self["additionalGuests"] != float64(1) || self["self"] != true {
		t.Errorf("existing attendee lost fields: %v", self)
	}
	if s := b["start"].(map[string]any); s["dateTime"] != "2026-11-10T10:00:00-08:00" || s["timeZone"] != la {
		t.Errorf("moved start = %v", s)
	}
	if !moved.Start.Equal(utc(2026, 11, 10, 18, 0)) || len(moved.Attendees) != 4 {
		t.Errorf("moved event = %+v", moved)
	}

	// An event whose attendee list was cut short cannot have its attendees
	// rewritten, or the rest would be dropped.
	moved.AttendeesTruncated = true
	moved.Attendees = moved.Attendees[:2]
	patches := countMethod(srv, "PATCH")
	if _, err := st.Put(ctx(t), me, moved, moved.ETag, calendar.NotifySend); !errors.Is(err, google.ErrRequest) {
		t.Errorf("attendee change on a truncated list: err = %v, want ErrRequest", err)
	}
	if countMethod(srv, "PATCH") != patches {
		t.Error("refused update was sent")
	}
}

func TestPutUpdateInstanceAndRecurrence(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, seriesEvent("series1"))
	srv.AddEvent(me, exception("series1", "20261117T170000Z", "2026-11-17T09:00:00-08:00", "2026-11-17T11:00:00-08:00"))
	series, err := st.GetEvent(ctx(t), me, "series1")
	if err != nil {
		t.Fatal(err)
	}
	evs := byKey(series)
	override := evs["series1@20261117T170000Z"]
	override.Location = "Room 9"
	if _, err := st.Put(ctx(t), me, override, override.ETag, calendar.NotifySend); err != nil {
		t.Fatalf("override update: %v", err)
	}
	req, _ := srv.LastRequest("PATCH")
	if req.Path != "/calendars/"+me+"/events/series1_20261117T170000Z" {
		t.Errorf("override update went to %s", req.Path)
	}
	if k := keys(body(t, req)); !slices.Equal(k, []string{"location"}) {
		t.Errorf("override update patched %v", k)
	}

	master := evs["series1"]
	master.RRule = "FREQ=WEEKLY;BYDAY=TU;COUNT=4"
	master.ExDates = nil
	if _, err := st.Put(ctx(t), me, master, master.ETag, calendar.NotifySend); err != nil {
		t.Fatalf("master update: %v", err)
	}
	req, _ = srv.LastRequest("PATCH")
	b := body(t, req)
	if k := keys(b); !slices.Equal(k, []string{"recurrence"}) {
		t.Errorf("recurrence update patched %v", k)
	}
	rec := b["recurrence"].([]any)
	if len(rec) != 2 || rec[0] != "RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=4" || rec[1] != "RDATE;TZID=America/Los_Angeles:20261111T090000" {
		t.Errorf("recurrence = %v", rec)
	}
}

func TestPutUpdateReadOnly(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(readOnly, timedEvent("single1"))
	evs, err := st.GetEvent(ctx(t), readOnly, "single1")
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	ev.Summary = "mine now"
	if _, err := st.Put(ctx(t), readOnly, ev, ev.ETag, calendar.NotifyNone); !errors.Is(err, calstore.ErrReadOnly) {
		t.Errorf("update on a reader calendar: err = %v, want ErrReadOnly", err)
	}
	if err := st.Delete(ctx(t), readOnly, "single1", ev.ETag, calendar.NotifyNone); !errors.Is(err, calstore.ErrReadOnly) {
		t.Errorf("delete on a reader calendar: err = %v, want ErrReadOnly", err)
	}
}

func attendee(t *testing.T, obj map[string]any, email string) map[string]any {
	t.Helper()
	for _, a := range obj["attendees"].([]any) {
		m := a.(map[string]any)
		if strings.EqualFold(m["email"].(string), email) {
			return m
		}
	}
	t.Fatalf("no attendee %s in %v", email, obj["attendees"])
	return nil
}

func TestRespondSeries(t *testing.T) {
	st, srv := newStore(t)
	etag := srv.AddEvent(me, timedEvent("single1"))
	got, err := st.Respond(ctx(t), me, "single1", calendar.Response{Self: "ME@example.com", PartStat: calendar.PartStatAccepted, Comment: "see you"})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	req, _ := srv.LastRequest("PATCH")
	if req.Path != "/calendars/"+me+"/events/single1" || req.IfMatch != etag || req.Query.Get("sendUpdates") != "all" {
		t.Errorf("respond request %s If-Match %q query %v", req.Path, req.IfMatch, req.Query)
	}
	b := body(t, req)
	if k := keys(b); !slices.Equal(k, []string{"attendees", "attendeesOmitted"}) {
		t.Errorf("respond patched %v", k)
	}
	self := attendee(t, b, me)
	if self["responseStatus"] != "accepted" || self["comment"] != "see you" || self["additionalGuests"] != float64(1) {
		t.Errorf("self attendee = %v", self)
	}
	// A participant-only update: only self is sent, and the other attendees
	// stay as they were.
	if b["attendeesOmitted"] != true || len(b["attendees"].([]any)) != 1 {
		t.Errorf("respond body is not a participant-only update: %v", b)
	}
	stored, _ := srv.Event(me, "single1")
	if boss := attendee(t, stored, "boss@example.org"); boss["responseStatus"] != "accepted" {
		t.Errorf("organizer attendee changed: %v", boss)
	}
	if opt := attendee(t, stored, "opt@example.org"); opt["responseStatus"] != "tentative" {
		t.Errorf("other attendee changed: %v", opt)
	}
	if n := len(stored["attendees"].([]any)); n != 3 {
		t.Errorf("stored event has %d attendees, want 3", n)
	}
	var mine calendar.PartStat
	for _, a := range got.Attendees {
		if a.Email == me {
			mine = a.PartStat
		}
	}
	if mine != calendar.PartStatAccepted {
		t.Errorf("returned event has my PARTSTAT %q", mine)
	}
}

func TestRespondInstance(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, seriesEvent("series1"))
	got, err := st.Respond(ctx(t), me, "series1", calendar.Response{
		Self: me, PartStat: calendar.PartStatDeclined, RecurrenceID: utc(2026, 11, 10, 17, 0),
	})
	if err != nil {
		t.Fatalf("Respond to an instance: %v", err)
	}
	req, _ := srv.LastRequest("PATCH")
	if req.Path != "/calendars/"+me+"/events/series1_20261110T170000Z" || req.Query.Get("sendUpdates") != "all" || req.IfMatch == "" {
		t.Errorf("instance respond request %s If-Match %q query %v", req.Path, req.IfMatch, req.Query)
	}
	if self := attendee(t, body(t, req), me); self["responseStatus"] != "declined" || self["comment"] != nil {
		t.Errorf("self attendee = %v", self)
	}
	if got.ProviderID != "series1" || !got.RecurrenceID.Equal(utc(2026, 11, 10, 17, 0)) {
		t.Errorf("returned instance = %+v", got)
	}
	master, _ := srv.Event(me, "series1")
	if self := attendee(t, master, me); self["responseStatus"] != "needsAction" {
		t.Errorf("instance answer changed the series: %v", self)
	}

	allDay := map[string]any{
		"id": "daily1", "start": map[string]any{"date": "2026-11-12"}, "end": map[string]any{"date": "2026-11-13"},
		"recurrence": []any{"RRULE:FREQ=WEEKLY"},
		"attendees":  []any{map[string]any{"email": me, "responseStatus": "needsAction"}},
	}
	srv.AddEvent(me, allDay)
	if _, err := st.Respond(ctx(t), me, "daily1", calendar.Response{
		Self: me, PartStat: calendar.PartStatTentative, RecurrenceID: utc(2026, 11, 19, 0, 0),
	}); err != nil {
		t.Fatalf("Respond to an all-day instance: %v", err)
	}
	req, _ = srv.LastRequest("PATCH")
	if req.Path != "/calendars/"+me+"/events/daily1_20261119" {
		t.Errorf("all-day instance respond went to %s", req.Path)
	}
}

func TestRespondRefusals(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, timedEvent("single1"))
	if _, err := st.Respond(ctx(t), me, "single1", calendar.Response{Self: "stranger@example.org", PartStat: calendar.PartStatAccepted}); !errors.Is(err, google.ErrRequest) {
		t.Errorf("not an attendee: err = %v, want ErrRequest", err)
	}
	if _, err := st.Respond(ctx(t), me, "single1", calendar.Response{Self: me, PartStat: calendar.PartStatNeedsAction}); !errors.Is(err, google.ErrRequest) {
		t.Errorf("NEEDS-ACTION: err = %v, want ErrRequest", err)
	}
	if countMethod(srv, "PATCH") != 0 {
		t.Error("a refused respond was sent")
	}
	if _, err := st.Respond(ctx(t), me, "nothere1", calendar.Response{Self: me, PartStat: calendar.PartStatAccepted}); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("unknown event: err = %v, want ErrNotFound", err)
	}
	srv.Inject(fakegcal.Fault{Method: "PATCH", Status: 412, Body: `{"error":{"code":412,"errors":[{"reason":"conditionNotMet"}]}}`})
	if _, err := st.Respond(ctx(t), me, "single1", calendar.Response{Self: me, PartStat: calendar.PartStatAccepted}); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("changed under the respond: err = %v, want ErrConflict", err)
	}
}

func TestDelete(t *testing.T) {
	st, srv := newStore(t)
	etag := srv.AddEvent(me, timedEvent("single1"))
	srv.AddEvent(me, timedEvent("single2"))
	ch, err := st.Sync(ctx(t), me, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Delete(ctx(t), me, "single1", `"1"`, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("delete with a stale etag: err = %v, want ErrConflict", err)
	}
	if err := st.Delete(ctx(t), me, "single1", etag, calendar.NotifySend); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	req, _ := srv.LastRequest("DELETE")
	if req.Path != "/calendars/"+me+"/events/single1" || req.IfMatch != etag || req.Query.Get("sendUpdates") != "all" {
		t.Errorf("delete request %s If-Match %q query %v", req.Path, req.IfMatch, req.Query)
	}
	if err := st.Delete(ctx(t), me, "single2", "", calendar.NotifyNone); err != nil {
		t.Fatalf("Delete without notifying: %v", err)
	}
	req, _ = srv.LastRequest("DELETE")
	if req.Query.Get("sendUpdates") != "none" || req.IfMatch != "" {
		t.Errorf("silent delete query %v If-Match %q", req.Query, req.IfMatch)
	}

	if err := st.Delete(ctx(t), me, "single1", etag, calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx(t), me, "nothere1", "", calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("unknown event: err = %v, want ErrNotFound", err)
	}

	inc, err := st.Sync(ctx(t), me, ch.Token)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(inc.Deletes)
	if !slices.Equal(inc.Deletes, []string{"single1", "single2"}) || len(inc.Upserts) != 0 {
		t.Errorf("sync after deletes: %+v", inc)
	}
}

func TestEventPathEscapesIDs(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(readOnly, timedEvent("single1"))
	if _, err := st.GetEvent(ctx(t), readOnly, "single1"); err != nil {
		t.Fatalf("calendar id with # and @: %v", err)
	}
	if _, err := st.Sync(ctx(t), readOnly, ""); err != nil {
		t.Fatalf("sync of a calendar id with #: %v", err)
	}
}

func TestPutUpdateSwitchesAllDay(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, timedEvent("single1"))
	ev := getOne(t, st, "single1")

	// Timed to all-day: date replaces dateTime and timeZone.
	ev.AllDay, ev.TZID = true, ""
	ev.Start, ev.End = utc(2026, 11, 10, 0, 0), utc(2026, 11, 11, 0, 0)
	got, err := st.Put(ctx(t), me, ev, ev.ETag, calendar.NotifyNone)
	if err != nil {
		t.Fatalf("to all-day: %v", err)
	}
	stored, _ := srv.Event(me, "single1")
	if s := stored["start"].(map[string]any); len(s) != 1 || s["date"] != "2026-11-10" {
		t.Errorf("stored all-day start = %v", s)
	}
	if e := stored["end"].(map[string]any); len(e) != 1 || e["date"] != "2026-11-11" {
		t.Errorf("stored all-day end = %v", e)
	}
	if !got.AllDay || !got.Start.Equal(ev.Start) || !got.End.Equal(ev.End) || got.TZID != "" {
		t.Errorf("all-day event = %+v", got)
	}

	// All-day to timed: dateTime and timeZone replace date.
	got.AllDay, got.TZID = false, la
	got.Start, got.End = utc(2026, 11, 10, 18, 0), utc(2026, 11, 10, 19, 0)
	back, err := st.Put(ctx(t), me, got, got.ETag, calendar.NotifyNone)
	if err != nil {
		t.Fatalf("to timed: %v", err)
	}
	stored, _ = srv.Event(me, "single1")
	if s := stored["start"].(map[string]any); len(s) != 2 || s["dateTime"] != "2026-11-10T10:00:00-08:00" || s["timeZone"] != la {
		t.Errorf("stored timed start = %v", s)
	}
	if back.AllDay || !back.Start.Equal(got.Start) || !back.End.Equal(got.End) || back.TZID != la {
		t.Errorf("timed event = %+v", back)
	}
}

func TestRespondOmittedAttendees(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, timedEvent("single1"))
	// Google shows only the participant of a large event.
	srv.SetMaxAttendees(2)
	if _, err := st.Respond(ctx(t), me, "single1", calendar.Response{Self: me, PartStat: calendar.PartStatAccepted}); err != nil {
		t.Fatalf("Respond with omitted attendees: %v", err)
	}
	stored, _ := srv.Event(me, "single1")
	if n := len(stored["attendees"].([]any)); n != 3 {
		t.Fatalf("respond dropped attendees: %d left, want 3", n)
	}
	self := attendee(t, stored, me)
	if self["responseStatus"] != "accepted" || self["comment"] != "earlier note" || self["additionalGuests"] != float64(1) {
		t.Errorf("self attendee = %v", self)
	}
	if boss := attendee(t, stored, "boss@example.org"); boss["responseStatus"] != "accepted" {
		t.Errorf("organizer attendee changed: %v", boss)
	}

	// Google may leave the participant out too when it omits attendees;
	// the answer still goes as a participant-only update by address.
	noFlag := timedEvent("single2")
	for _, a := range noFlag["attendees"].([]any) {
		delete(a.(map[string]any), "self")
	}
	srv.AddEvent(me, noFlag)
	if _, err := st.Respond(ctx(t), me, "single2", calendar.Response{Self: me, PartStat: calendar.PartStatDeclined, Comment: "away"}); err != nil {
		t.Fatalf("Respond when the participant is not shown: %v", err)
	}
	req, _ := srv.LastRequest("PATCH")
	if b := body(t, req); b["attendeesOmitted"] != true || len(b["attendees"].([]any)) != 1 {
		t.Errorf("respond body = %v", b)
	}
	stored, _ = srv.Event(me, "single2")
	if n := len(stored["attendees"].([]any)); n != 3 {
		t.Fatalf("respond dropped attendees: %d left, want 3", n)
	}
	if self := attendee(t, stored, me); self["responseStatus"] != "declined" || self["comment"] != "away" {
		t.Errorf("self attendee = %v", self)
	}
}

func TestPutUpdateExceptionOfOtherKind(t *testing.T) {
	st, srv := newStore(t)
	// A timed series whose 17 November instance became all-day.
	srv.AddEvent(me, seriesEvent("series1"))
	toAllDay := exception("series1", "20261117T170000Z", "2026-11-17T09:00:00-08:00", "2026-11-17T11:00:00-08:00")
	toAllDay["start"] = map[string]any{"date": "2026-11-17"}
	toAllDay["end"] = map[string]any{"date": "2026-11-18"}
	srv.AddEvent(me, toAllDay)
	// An all-day series whose 19 November instance became timed.
	srv.AddEvent(me, map[string]any{
		"id": "daily1", "iCalUID": "daily1@example.org",
		"start": map[string]any{"date": "2026-11-12"}, "end": map[string]any{"date": "2026-11-13"},
		"recurrence": []any{"RRULE:FREQ=WEEKLY"},
	})
	srv.AddEvent(me, map[string]any{
		"id": "daily1_20261119", "recurringEventId": "daily1", "iCalUID": "daily1@example.org",
		"originalStartTime": map[string]any{"date": "2026-11-19"},
		"start":             map[string]any{"dateTime": "2026-11-19T09:00:00-08:00", "timeZone": la},
		"end":               map[string]any{"dateTime": "2026-11-19T10:00:00-08:00", "timeZone": la},
	})

	cases := []struct {
		series, key, wantPath string
		allDay                bool
	}{
		{"series1", "series1@20261117T170000Z", "/calendars/" + me + "/events/series1_20261117T170000Z", true},
		{"daily1", "daily1@20261119T000000Z", "/calendars/" + me + "/events/daily1_20261119", false},
	}
	for _, tc := range cases {
		evs, err := st.GetEvent(ctx(t), me, tc.series)
		if err != nil {
			t.Fatal(err)
		}
		ov, ok := byKey(evs)[tc.key]
		if !ok || ov.AllDay != tc.allDay {
			t.Fatalf("%s: override %+v (found %v)", tc.key, ov, ok)
		}
		// With the instance JSON in Raw, and without it.
		for _, keepRaw := range []bool{true, false} {
			ev := ov
			if !keepRaw {
				ev.Raw = nil
			}
			ev.Location = "Room " + tc.key
			got, err := st.Put(ctx(t), me, ev, ev.ETag, calendar.NotifyNone)
			if err != nil {
				t.Fatalf("%s (raw %v): %v", tc.key, keepRaw, err)
			}
			req, _ := srv.LastRequest("PATCH")
			if req.Path != tc.wantPath {
				t.Errorf("%s (raw %v): patched %s, want %s", tc.key, keepRaw, req.Path, tc.wantPath)
			}
			ov = got
		}
	}
}

func TestGetEventOverrideUID(t *testing.T) {
	st, srv := newStore(t)
	srv.AddEvent(me, seriesEvent("series1"))
	srv.AddEvent(me, cancelledInstance("series1", "20261124T170000Z", "2026-11-24T09:00:00-08:00"))
	evs, err := st.GetEvent(ctx(t), me, "series1_20261124T170000Z")
	if err != nil || len(evs) != 1 {
		t.Fatalf("GetEvent of a cancelled instance: %+v, %v", evs, err)
	}
	if evs[0].UID != "series1@example.org" || evs[0].ProviderID != "series1" {
		t.Errorf("cancelled instance = %+v", evs[0])
	}
}
