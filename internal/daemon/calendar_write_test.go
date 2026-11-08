package daemon_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
)

// derivedUID is the UID the daemon gives an event created with key on
// account (Amendment 14).
func derivedUID(account, key string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + key))
	return hex.EncodeToString(sum[:]) + "@chainmail"
}

func (e *calEnv) create(t *testing.T, req *chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
	t.Helper()
	resp, err := e.Client.Calendar.CreateEvent(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// meetingRequest is a create of a one-hour meeting on mw.
func (e *calEnv) meetingRequest(key string, attendees ...string) *chainmailv1.CreateEventRequest {
	return &chainmailv1.CreateEventRequest{
		Account: "mw", Title: "Plan " + secretText, Location: "Room " + secretText, Description: "Agenda " + secretText,
		Start: ts(e.start), End: ts(e.start.Add(time.Hour)), TimeZone: "Europe/London",
		Attendees: attendees, IdempotencyKey: key,
	}
}

func TestCreateEvent(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	got, err := e.create(t, e.meetingRequest("c1", allowedAddr))
	if err != nil {
		t.Fatal(err)
	}
	uid := derivedUID("mw", "c1")
	ref := calendar.EventRef("mw", mwCal, calfake.ObjectName(uid))
	ev := got.GetEvent()
	if got.GetHeld() != nil || ev.GetRef() != ref || ev.GetUid() != uid || !ev.GetIsOrganizer() || ev.GetTimeZone() != "Europe/London" ||
		ev.GetSummary().GetUntrusted() != "Plan "+secretText || !ev.GetStart().AsTime().Equal(e.start) {
		t.Errorf("event = %v", got)
	}
	checkUntrusted(t, got)
	calls := e.mw.CallsTo(calfake.MethodPut)
	if len(calls) != 1 || calls[0].Notify != calendar.NotifySend || calls[0].ETag != "" || calls[0].CalendarID != mwCal {
		t.Fatalf("Put calls = %+v", calls)
	}
	put := calls[0].Event
	if put.UID != uid || put.Organizer == nil || put.Organizer.Email != "james@mw.example" || put.TZID != "Europe/London" ||
		len(put.Attendees) != 2 || put.Attendees[0].Email != "james@mw.example" || put.Attendees[0].PartStat != calendar.PartStatAccepted ||
		put.Attendees[1].Email != allowedAddr || put.Attendees[1].PartStat != calendar.PartStatNeedsAction {
		t.Errorf("event put = %+v", put)
	}
	if r := auditRow(t, e.Harness, "calendar.create_event"); r.Result != "ok" || r.Account != "mw" ||
		!strings.Contains(r.Targets, "calendar="+mwCal) || !strings.Contains(r.Targets, ref) {
		t.Errorf("audit row %+v", r)
	}

	// The same key returns the event already created.
	again, err := e.create(t, e.meetingRequest("c1", allowedAddr))
	if err != nil {
		t.Fatal(err)
	}
	if again.GetEvent().GetRef() != ref || e.mw.Count(calfake.MethodPut) != 1 {
		t.Errorf("replay = %v, Put calls %d", again, e.mw.Count(calfake.MethodPut))
	}
	other := e.meetingRequest("c1", allowedAddr)
	other.Title = "Another"
	_, err = e.create(t, other)
	wantReason(t, "key reused for another event", err, api.ReasonInvalidArgument)
	noSecretInAudit(t, e.Harness)
}

func TestCreateEventHeld(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	got, err := e.create(t, e.meetingRequest("held-1", allowedAddr, blockedAddr))
	if err != nil {
		t.Fatal(err)
	}
	held := got.GetHeld()
	ref := calendar.EventRef("mw", mwCal, calfake.ObjectName(derivedUID("mw", "held-1")))
	if held == nil || held.GetEventRef() != ref || got.GetEvent().GetRef() != ref || len(held.GetBlocked()) != 1 ||
		held.GetBlocked()[0].GetUntrusted() != blockedAddr || held.GetHint() == "" {
		t.Fatalf("response = %v, want a held draft event", got)
	}
	calls := e.mw.CallsTo(calfake.MethodPut)
	if len(calls) != 1 || calls[0].Notify != calendar.NotifyNone {
		t.Errorf("Put calls = %+v, want one without notifications", calls)
	}
	if r := auditRow(t, e.Harness, "calendar.create_event"); r.Reason != api.ReasonHeld || !strings.Contains(r.Targets, "blocked:1") || strings.Contains(r.Targets, blockedAddr) {
		t.Errorf("audit row %+v", r)
	}
}

func TestCreateEventAfterACrash(t *testing.T) {
	t.Parallel()
	uid := derivedUID("mw", "crash-1")
	e := startCal(t, func(e *calEnv) {
		// The create reached the server before the daemon died.
		e.mw.Add(mwCal, calendar.Event{UID: uid, Summary: "Plan", Start: e.start, End: e.start.Add(time.Hour), Organizer: &calendar.Person{Email: "james@mw.example"}})
	})
	got, err := e.create(t, e.meetingRequest("crash-1"))
	if err != nil {
		t.Fatal(err)
	}
	ref := calendar.EventRef("mw", mwCal, calfake.ObjectName(uid))
	if got.GetEvent().GetRef() != ref {
		t.Errorf("event = %v, want the one already there", got.GetEvent())
	}
	if _, ok := e.mw.Object(mwCal, calfake.ObjectName(uid)); !ok || e.mw.Count(calfake.MethodGetEvent) != 1 {
		t.Errorf("object kept %v, GetEvent calls %d", ok, e.mw.Count(calfake.MethodGetEvent))
	}

	// A key an earlier daemon left in progress is settled as unknown at
	// start-up; a repeat finds the event rather than reporting an unknown
	// send.
	if _, err := e.DB.ExecContext(t.Context(), "UPDATE idempotency SET state = 'unknown', result = x''"); err != nil {
		t.Fatal(err)
	}
	got, err = e.create(t, e.meetingRequest("crash-1"))
	if err != nil {
		t.Fatalf("repeat after an unknown outcome: %v", err)
	}
	if got.GetEvent().GetRef() != ref {
		t.Errorf("event = %v", got.GetEvent())
	}
}

func TestCreateAllDayEvent(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	day := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name       string
		start, end time.Time
		zone       string
	}{
		{"UTC midnights", day, day.Add(48 * time.Hour), ""},
		{"midnights in the request zone", time.Date(2026, 12, 24, 0, 0, 0, 0, la), time.Date(2026, 12, 26, 0, 0, 0, 0, la), "America/Los_Angeles"},
	} {
		got, err := e.create(t, &chainmailv1.CreateEventRequest{
			Account: "mw", Title: "Holiday", AllDay: true, Start: ts(tt.start), End: ts(tt.end), TimeZone: tt.zone, IdempotencyKey: "day-" + strings.ReplaceAll(tt.zone, "/", "."),
		})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		ev := got.GetEvent()
		if !ev.GetAllDay() || !ev.GetStart().AsTime().Equal(day) || !ev.GetEnd().AsTime().Equal(day.Add(48*time.Hour)) {
			t.Errorf("%s: event %v, want Dec 24 and 25", tt.name, ev)
		}
	}
}

func TestCreateEventRefusals(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	base := func(mod func(r *chainmailv1.CreateEventRequest)) *chainmailv1.CreateEventRequest {
		r := e.meetingRequest("r1", allowedAddr)
		mod(r)
		return r
	}
	many := make([]string, 501)
	for i := range many {
		many[i] = "a" + strconv.Itoa(i) + "@example.org"
	}
	for name, tt := range map[string]struct {
		req  *chainmailv1.CreateEventRequest
		want string
	}{
		"store none":      {base(func(r *chainmailv1.CreateEventRequest) { r.Account = "host" }), api.ReasonNoCalendarStore},
		"read-only":       {base(func(r *chainmailv1.CreateEventRequest) { r.CalendarId = mwShared }), api.ReasonCalendarReadOnly},
		"no calendar":     {base(func(r *chainmailv1.CreateEventRequest) { r.CalendarId = "/cal/none/" }), api.ReasonNotFound},
		"two accounts":    {base(func(r *chainmailv1.CreateEventRequest) { r.Account = "" }), api.ReasonInvalidArgument},
		"no key":          {base(func(r *chainmailv1.CreateEventRequest) { r.IdempotencyKey = "" }), api.ReasonInvalidArgument},
		"bad key":         {base(func(r *chainmailv1.CreateEventRequest) { r.IdempotencyKey = "a b" }), api.ReasonInvalidArgument},
		"no title":        {base(func(r *chainmailv1.CreateEventRequest) { r.Title = " " }), api.ReasonInvalidArgument},
		"long title":      {base(func(r *chainmailv1.CreateEventRequest) { r.Title = strings.Repeat("x", 1025) }), api.ReasonInvalidArgument},
		"backwards":       {base(func(r *chainmailv1.CreateEventRequest) { r.End = ts(e.start.Add(-time.Hour)) }), api.ReasonInvalidArgument},
		"no start":        {base(func(r *chainmailv1.CreateEventRequest) { r.Start = nil }), api.ReasonInvalidArgument},
		"bad zone":        {base(func(r *chainmailv1.CreateEventRequest) { r.TimeZone = "Mars/" + secretText }), api.ReasonInvalidArgument},
		"bad attendee":    {base(func(r *chainmailv1.CreateEventRequest) { r.Attendees = []string{"not an address " + secretText} }), api.ReasonInvalidArgument},
		"501 attendees":   {base(func(r *chainmailv1.CreateEventRequest) { r.Attendees = many }), api.ReasonInvalidArgument},
		"unsafe rule":     {base(func(r *chainmailv1.CreateEventRequest) { r.RecurrenceRule = "FREQ=SECONDLY" }), api.ReasonInvalidArgument},
		"broken rule":     {base(func(r *chainmailv1.CreateEventRequest) { r.RecurrenceRule = "FREQ=" + secretText }), api.ReasonInvalidArgument},
		"unknown account": {base(func(r *chainmailv1.CreateEventRequest) { r.Account = "nope" }), api.ReasonNotFound},
	} {
		_, err := e.create(t, tt.req)
		wantReason(t, name, err, tt.want)
		if err != nil && strings.Contains(err.Error()+api.Detail(err).GetHint(), secretText) {
			t.Errorf("%s: error %v repeats the request", name, err)
		}
	}
	if n := e.mw.Count(calfake.MethodPut); n != 0 {
		t.Errorf("Put calls = %d, want none", n)
	}
	noSecretInAudit(t, e.Harness)
}

// etagOf reads an event's etag with GetEvent, as a client does before a
// change.
func (e *calEnv) etagOf(t *testing.T, ref string) string {
	t.Helper()
	resp, err := e.Client.Calendar.GetEvent(t.Context(), connect.NewRequest(&chainmailv1.GetEventRequest{Ref: ref}))
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	return resp.Msg.GetEvent().GetEtag()
}

func (e *calEnv) update(t *testing.T, req *chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error) {
	t.Helper()
	resp, err := e.Client.Calendar.UpdateEvent(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (e *calEnv) cancel(t *testing.T, ref, etag string) (*chainmailv1.CancelEventResponse, error) {
	t.Helper()
	resp, err := e.Client.Calendar.CancelEvent(t.Context(), connect.NewRequest(&chainmailv1.CancelEventRequest{Ref: ref, Etag: etag}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

func TestUpdateEvent(t *testing.T) {
	t.Parallel()
	var p, weekly string
	e := startCal(t, func(e *calEnv) {
		p = e.mw.Add(mwCal, own("mine@x", "james@mw.example", e.start, allowedAddr))
		master := own("weekly@x", "james@mw.example", e.start)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		weekly = e.mw.Add(mwCal, master)
	})
	ref := calendar.EventRef("mw", mwCal, p)
	etag := e.etagOf(t, ref)
	got, err := e.update(t, &chainmailv1.UpdateEventRequest{
		Ref: ref, Etag: etag, UpdateMask: mask("title", "start", "location"),
		Title: "New " + secretText, Start: ts(e.start.Add(time.Hour)), Location: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := got.GetEvent()
	if got.GetHeld() != nil || ev.GetSummary().GetUntrusted() != "New "+secretText || ev.GetLocation().GetUntrusted() != "" ||
		!ev.GetStart().AsTime().Equal(e.start.Add(time.Hour)) || !ev.GetEnd().AsTime().Equal(e.start.Add(2*time.Hour)) || ev.GetEtag() == etag {
		t.Errorf("event = %v, want the new title and an hour later, an hour long", ev)
	}
	checkUntrusted(t, got)
	calls := e.mw.CallsTo(calfake.MethodPut)
	if len(calls) != 1 || calls[0].ETag != etag || calls[0].Notify != calendar.NotifySend || calls[0].Event.ProviderID != p {
		t.Fatalf("Put calls = %+v", calls)
	}
	if r := auditRow(t, e.Harness, "calendar.update_event"); r.Result != "ok" || !strings.Contains(r.Targets, ref) || !strings.Contains(r.Targets, "fields=title,start,location") {
		t.Errorf("audit row %+v", r)
	}

	// One instance of a series.
	second := e.start.Add(7 * 24 * time.Hour)
	iref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, weekly), second)
	got, err = e.update(t, &chainmailv1.UpdateEventRequest{Ref: iref, Etag: e.etagOf(t, iref), UpdateMask: mask("title"), Title: "Moved"})
	if err != nil {
		t.Fatal(err)
	}
	calls = e.mw.CallsTo(calfake.MethodPut)
	if len(calls) != 2 || !calls[1].Event.RecurrenceID.Equal(second) || got.GetEvent().GetRef() != iref || got.GetEvent().GetSummary().GetUntrusted() != "Moved" {
		t.Errorf("instance update: Put %+v, event %v", calls, got.GetEvent())
	}
	noSecretInAudit(t, e.Harness)
}

func TestUpdateEventHeld(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		p = e.mw.Add(mwCal, own("mine@x", "james@mw.example", e.start, allowedAddr))
	})
	ref := calendar.EventRef("mw", mwCal, p)
	got, err := e.update(t, &chainmailv1.UpdateEventRequest{
		Ref: ref, Etag: e.etagOf(t, ref), UpdateMask: mask("attendees", "end"),
		Attendees: []string{allowedAddr, blockedAddr}, End: ts(e.start.Add(2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	held := got.GetHeld()
	if held == nil || got.GetEvent() != nil || len(held.GetBlocked()) != 1 || held.GetHint() == "" ||
		!strings.Contains(held.GetSummary(), "attendees") || !strings.Contains(held.GetSummary(), e.start.Add(2*time.Hour).Format(time.RFC3339)) ||
		strings.Contains(held.GetSummary(), blockedAddr) {
		t.Fatalf("response = %v, want held with a summary", got)
	}
	if n := e.mw.Count(calfake.MethodPut); n != 0 {
		t.Errorf("Put calls = %d, want none: a held change is not applied", n)
	}
	if r := auditRow(t, e.Harness, "calendar.update_event"); r.Reason != api.ReasonHeld || !strings.Contains(r.Targets, "blocked:1") {
		t.Errorf("audit row %+v", r)
	}
}

func TestUpdateEventRefusals(t *testing.T) {
	t.Parallel()
	var mine, invited, shared, weekly string
	e := startCal(t, func(e *calEnv) {
		mine = e.mw.Add(mwCal, own("mine@x", "james@mw.example", e.start, allowedAddr))
		invited = e.mw.Add(mwCal, invitation("inv@x", "james@mw.example", e.start))
		shared = e.mw.Add(mwShared, own("shared@x", "james@mw.example", e.start))
		master := own("weekly@x", "james@mw.example", e.start)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		weekly = e.mw.Add(mwCal, master)
	})
	ref := calendar.EventRef("mw", mwCal, mine)
	etag := e.etagOf(t, ref)
	iref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, weekly), e.start)
	for name, tt := range map[string]struct {
		req  *chainmailv1.UpdateEventRequest
		want string
	}{
		"not organizer": {&chainmailv1.UpdateEventRequest{Ref: calendar.EventRef("mw", mwCal, invited), Etag: "x", UpdateMask: mask("title"), Title: "t"}, api.ReasonNotOrganizer},
		"read-only":     {&chainmailv1.UpdateEventRequest{Ref: calendar.EventRef("mw", mwShared, shared), Etag: "x", UpdateMask: mask("title"), Title: "t"}, api.ReasonCalendarReadOnly},
		"stale etag":    {&chainmailv1.UpdateEventRequest{Ref: ref, Etag: "stale", UpdateMask: mask("title"), Title: "t"}, api.ReasonEventConflict},
		"no etag":       {&chainmailv1.UpdateEventRequest{Ref: ref, UpdateMask: mask("title"), Title: "t"}, api.ReasonInvalidArgument},
		"no mask":       {&chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag}, api.ReasonInvalidArgument},
		"unknown path":  {&chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("uid " + secretText)}, api.ReasonInvalidArgument},
		"empty title":   {&chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("title")}, api.ReasonInvalidArgument},
		"backwards":     {&chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("end"), End: ts(e.start.Add(-time.Hour))}, api.ReasonInvalidArgument},
		"all-day alone": {&chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("all_day"), AllDay: true}, api.ReasonInvalidArgument},
		"instance rule": {&chainmailv1.UpdateEventRequest{Ref: iref, Etag: e.etagOf(t, iref), UpdateMask: mask("recurrence_rule"), RecurrenceRule: "FREQ=DAILY"}, api.ReasonInvalidArgument},
		"unknown ref":   {&chainmailv1.UpdateEventRequest{Ref: calendar.EventRef("mw", mwCal, "nope"), Etag: "x", UpdateMask: mask("title"), Title: "t"}, api.ReasonNotFound},
	} {
		_, err := e.update(t, tt.req)
		wantReason(t, name, err, tt.want)
		if err != nil && strings.Contains(err.Error()+api.Detail(err).GetHint(), secretText) {
			t.Errorf("%s: error %v repeats the request", name, err)
		}
	}
	if n := e.mw.Count(calfake.MethodPut); n != 0 {
		t.Errorf("Put calls = %d, want none", n)
	}
	// The store finds the event changed since it was read.
	e.mw.FailNext(calfake.MethodPut, calstore.ErrConflict)
	_, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("title"), Title: "t"})
	wantReason(t, "store conflict", err, api.ReasonEventConflict)
}

func TestCancelEvent(t *testing.T) {
	t.Parallel()
	var withPeople, alone, invited, blocked, weekly string
	e := startCal(t, func(e *calEnv) {
		withPeople = e.mw.Add(mwCal, own("people@x", "james@mw.example", e.start, allowedAddr))
		alone = e.mw.Add(mwCal, own("alone@x", "james@mw.example", e.start))
		invited = e.mw.Add(mwCal, invitation("inv@x", "james@mw.example", e.start))
		blocked = e.mw.Add(mwCal, own("blocked@x", "james@mw.example", e.start, blockedAddr))
		master := own("weekly@x", "james@mw.example", e.start, allowedAddr)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		weekly = e.mw.Add(mwCal, master)
	})
	for _, p := range []string{withPeople, alone} {
		ref := calendar.EventRef("mw", mwCal, p)
		etag := e.etagOf(t, ref)
		got, err := e.cancel(t, ref, etag)
		if err != nil {
			t.Fatal(err)
		}
		if !got.GetCancelled() || got.GetHeld() != nil {
			t.Errorf("cancel of %s = %v", p, got)
		}
		calls := e.mw.CallsTo(calfake.MethodDelete)
		if last := calls[len(calls)-1]; last.ProviderID != p || last.ETag != etag || last.Notify != calendar.NotifySend {
			t.Errorf("Delete call = %+v", last)
		}
		if _, ok := e.mw.Object(mwCal, p); ok {
			t.Errorf("object %s still there", p)
		}
	}
	if r := auditRow(t, e.Harness, "calendar.cancel_event"); r.Result != "ok" {
		t.Errorf("audit row %+v", r)
	}

	ref := calendar.EventRef("mw", mwCal, blocked)
	got, err := e.cancel(t, ref, e.etagOf(t, ref))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCancelled() || got.GetHeld() == nil || !strings.Contains(got.GetHeld().GetSummary(), "attendees 1") {
		t.Errorf("held cancel = %v", got)
	}
	if n := e.mw.Count(calfake.MethodDelete); n != 2 {
		t.Errorf("Delete calls = %d, want the held cancel not applied", n)
	}

	_, err = e.cancel(t, calendar.EventRef("mw", mwCal, invited), "x")
	wantReason(t, "not organizer", err, api.ReasonNotOrganizer)
	_, err = e.cancel(t, calendar.EventRef("mw", mwCal, withPeople), "x")
	wantReason(t, "gone", err, api.ReasonNotFound)
	wref := calendar.EventRef("mw", mwCal, weekly)
	_, err = e.cancel(t, wref, "stale")
	wantReason(t, "stale etag", err, api.ReasonEventConflict)

	// One instance: an override that is cancelled.
	second := e.start.Add(7 * 24 * time.Hour)
	iref := calendar.InstanceRef(wref, second)
	if _, err := e.cancel(t, iref, e.etagOf(t, iref)); err != nil {
		t.Fatal(err)
	}
	calls := e.mw.CallsTo(calfake.MethodPut)
	if len(calls) != 1 || !calls[0].Event.RecurrenceID.Equal(second) || calls[0].Event.Status != calendar.StatusCancelled {
		t.Errorf("Put calls = %+v, want the instance cancelled", calls)
	}
	noSecretInAudit(t, e.Harness)
}

// An event Google or the parser cut at 500 attendees has attendees nobody
// checked, and any change would notify them: it is refused, not applied.
func TestEditOfATruncatedEventIsRefused(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		// boss is the last attendee chainmail reads; a blocked one lies
		// beyond the boundary.
		ev := own("big@x", "james@mw.example", e.start, allowedAddr)
		ev.AttendeesTruncated = true
		p = e.mw.Add(mwCal, ev)
	})
	ref := calendar.EventRef("mw", mwCal, p)
	etag := e.etagOf(t, ref)
	_, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("title"), Title: "Renamed"})
	wantCode(t, "update", err, connect.CodeFailedPrecondition)
	_, err = e.cancel(t, ref, etag)
	wantCode(t, "cancel", err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), "more attendees than chainmail reads") {
		t.Errorf("error = %v, want the fixed text", err)
	}
	if n := e.mw.Count(calfake.MethodPut) + e.mw.Count(calfake.MethodDelete); n != 0 {
		t.Errorf("store writes = %d, want none", n)
	}
}

// wantCode fails the test unless err has code.
func wantCode(t *testing.T, what string, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("%s: err = %v, want %v", what, err, code)
	}
}

// A change of a whole series notifies the attendees of every instance:
// those its overrides add are checked too.
func TestSeriesEditChecksEveryOverride(t *testing.T) {
	t.Parallel()
	var weekly, cut string
	second := func(e *calEnv) time.Time { return e.start.Add(7 * 24 * time.Hour) }
	e := startCal(t, func(e *calEnv) {
		master := own("weekly@x", "james@mw.example", e.start, allowedAddr)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		weekly = e.mw.Add(mwCal, master)
		e.mw.Change(mwCal, weekly, func(evs []calendar.Event) []calendar.Event {
			ov := evs[0]
			ov.RRule, ov.RecurrenceID = "", second(e)
			ov.Start, ov.End = second(e), second(e).Add(time.Hour)
			ov.Attendees = append(slices.Clone(ov.Attendees), person(blockedAddr, calendar.PartStatNeedsAction))
			return append(evs, ov)
		})
		other := own("cut@x", "james@mw.example", e.start, allowedAddr)
		other.RRule = "FREQ=WEEKLY;COUNT=3"
		cut = e.mw.Add(mwCal, other)
		e.mw.Change(mwCal, cut, func(evs []calendar.Event) []calendar.Event {
			ov := evs[0]
			ov.RRule, ov.RecurrenceID = "", second(e)
			ov.Start, ov.End = second(e), second(e).Add(time.Hour)
			ov.AttendeesTruncated = true
			return append(evs, ov)
		})
	})
	ref := calendar.EventRef("mw", mwCal, weekly)
	etag := e.etagOf(t, ref)
	got, err := e.cancel(t, ref, etag)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCancelled() || got.GetHeld() == nil || len(got.GetHeld().GetBlocked()) != 1 || got.GetHeld().GetBlocked()[0].GetUntrusted() != blockedAddr {
		t.Errorf("series cancel = %v, want held for the override's attendee", got)
	}
	up, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("title"), Title: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if up.GetHeld() == nil || len(up.GetHeld().GetBlocked()) != 1 {
		t.Errorf("series update = %v, want held for the override's attendee", up)
	}
	cref := calendar.EventRef("mw", mwCal, cut)
	_, err = e.cancel(t, cref, e.etagOf(t, cref))
	wantCode(t, "cancel of a series with a truncated override", err, connect.CodeFailedPrecondition)
	if n := e.mw.Count(calfake.MethodPut) + e.mw.Count(calfake.MethodDelete); n != 0 {
		t.Errorf("store writes = %d, want none", n)
	}
}

// sparseOverride adds to a series an override of rid that names no
// organizer and no attendee, so it takes its series's.
func sparseOverride(e *calEnv, calID, p string, rid time.Time) {
	e.mw.Change(calID, p, func(evs []calendar.Event) []calendar.Event {
		ov := evs[0]
		ov.RRule, ov.RecurrenceID, ov.Organizer, ov.Attendees = "", rid, nil, nil
		ov.Start, ov.End = rid.Add(15*time.Minute), rid.Add(75*time.Minute)
		return append(evs, ov)
	})
}

// A sparse override is organized by its series's organizer and notifies
// its series's attendees.
func TestSparseOverrideTakesItsSeriesRole(t *testing.T) {
	t.Parallel()
	var theirs, mine string
	e := startCal(t, func(e *calEnv) {
		second := e.start.Add(7 * 24 * time.Hour)
		inv := invitation("theirs@x", "james@mw.example", e.start)
		inv.RRule = "FREQ=WEEKLY;COUNT=3"
		theirs = e.mw.Add(mwCal, inv)
		sparseOverride(e, mwCal, theirs, second)
		own := own("mine@x", "james@mw.example", e.start, blockedAddr)
		own.RRule = "FREQ=WEEKLY;COUNT=3"
		mine = e.mw.Add(mwCal, own)
		sparseOverride(e, mwCal, mine, second)
	})
	second := e.start.Add(7 * 24 * time.Hour)
	tref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, theirs), second)
	_, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: tref, Etag: e.etagOf(t, tref), UpdateMask: mask("title"), Title: "Mine now"})
	wantReason(t, "update of someone else's override", err, api.ReasonNotOrganizer)
	_, err = e.cancel(t, tref, e.etagOf(t, tref))
	wantReason(t, "cancel of someone else's override", err, api.ReasonNotOrganizer)

	mref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, mine), second)
	got, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: mref, Etag: e.etagOf(t, mref), UpdateMask: mask("title"), Title: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetHeld() == nil || len(got.GetHeld().GetBlocked()) != 1 {
		t.Errorf("update of an override = %v, want held for the series's blocked attendee", got)
	}
	c, err := e.cancel(t, mref, e.etagOf(t, mref))
	if err != nil {
		t.Fatal(err)
	}
	if c.GetHeld() == nil || c.GetCancelled() {
		t.Errorf("cancel of an override = %v, want held", c)
	}
	if n := e.mw.Count(calfake.MethodPut) + e.mw.Count(calfake.MethodDelete); n != 0 {
		t.Errorf("store writes = %d, want none", n)
	}
}

// When the caller read a newer version than the cache holds, the checks
// run again on that version.
func TestEditRechecksTheRefreshedEvent(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		p = e.mw.Add(mwCal, own("alone@x", "james@mw.example", e.start))
	})
	ref := calendar.EventRef("mw", mwCal, p)
	// The event is handed to boss, who invites James; the cache has not
	// seen it yet.
	e.mw.Change(mwCal, p, func(evs []calendar.Event) []calendar.Event {
		inv := invitation("alone@x", "james@mw.example", e.start)
		inv.ProviderID = evs[0].ProviderID
		return []calendar.Event{inv}
	})
	rows, _ := e.mw.Object(mwCal, p)
	_, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: ref, Etag: rows[0].ETag, UpdateMask: mask("title"), Title: "Mine"})
	wantReason(t, "update of a version organized by someone else", err, api.ReasonNotOrganizer)
	_, err = e.cancel(t, ref, rows[0].ETag)
	wantReason(t, "cancel of a version organized by someone else", err, api.ReasonNotOrganizer)
	if n := e.mw.Count(calfake.MethodPut) + e.mw.Count(calfake.MethodDelete); n != 0 {
		t.Errorf("store writes = %d, want none", n)
	}
}
