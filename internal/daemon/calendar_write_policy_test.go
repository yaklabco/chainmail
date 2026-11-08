package daemon_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
)

// organizedBy is an event org organizes that lists only self as an
// attendee: the organizer is not an ATTENDEE, so nobody else is listed.
func organizedBy(uid, org, self string, start time.Time) calendar.Event {
	return calendar.Event{
		UID: uid, Summary: "Theirs " + secretText, Start: start, End: start.Add(time.Hour), Status: calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: org, Name: "Org " + secretText},
		Attendees: []calendar.Attendee{person(self, calendar.PartStatAccepted)},
	}
}

// Removing an event someone else organizes tells the organizer (a CalDAV
// scheduling server sends a REPLY, Google a decline), so the cancel is
// checked against the policy for the organizer even when he is not listed
// as an attendee; a held one is not applied.
func TestAttendeeSideCancelChecksTheOrganizer(t *testing.T) {
	t.Parallel()
	var blocked, allowed, series string
	e := startCal(t, func(e *calEnv) {
		blocked = e.mw.Add(mwCal, organizedBy("blocked-org@x", blockedAddr, "james@mw.example", e.start))
		allowed = e.mw.Add(mwCal, organizedBy("allowed-org@x", allowedAddr, "james@mw.example", e.start))
		master := organizedBy("series-org@x", blockedAddr, "james@mw.example", e.start)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		series = e.mw.Add(mwCal, master)
	})

	ref := calendar.EventRef("mw", mwCal, blocked)
	got, err := e.cancel(t, ref, e.etagOf(t, ref))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCancelled() || got.GetHeld() == nil || len(got.GetHeld().GetBlocked()) != 1 || got.GetHeld().GetBlocked()[0].GetUntrusted() != blockedAddr {
		t.Errorf("cancel of an event a blocked organizer runs = %v, want held for the organizer", got)
	}
	if r := auditRow(t, e.Harness, "calendar.cancel_event"); r.Reason != api.ReasonHeld {
		t.Errorf("audit row %+v, want held", r)
	}

	iref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, series), e.start.Add(7*24*time.Hour))
	got, err = e.cancel(t, iref, e.etagOf(t, iref))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCancelled() || got.GetHeld() == nil || len(got.GetHeld().GetBlocked()) != 1 {
		t.Errorf("cancel of an instance a blocked organizer runs = %v, want held", got)
	}
	if n := e.mw.Count(calfake.MethodPut) + e.mw.Count(calfake.MethodDelete); n != 0 {
		t.Fatalf("store writes = %d, want none: a held cancel is not applied", n)
	}

	aref := calendar.EventRef("mw", mwCal, allowed)
	etag := e.etagOf(t, aref)
	got, err = e.cancel(t, aref, etag)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetCancelled() || got.GetHeld() != nil {
		t.Errorf("cancel of an event an allowed organizer runs = %v, want cancelled", got)
	}
	calls := e.mw.CallsTo(calfake.MethodDelete)
	if len(calls) != 1 || calls[0].ProviderID != allowed || calls[0].ETag != etag || calls[0].Notify != calendar.NotifySend {
		t.Errorf("Delete calls = %+v, want one delete of the allowed organizer's event", calls)
	}
	noSecretInAudit(t, e.Harness)
}

// oldSeries adds a weekly series James organizes that started ten weeks
// ago, with boss invited, and an override of its second occurrence (nine
// weeks ago, so calendar.db prunes it) that mod changes.
func oldSeries(e *calEnv, uid string, mod func(ov *calendar.Event)) string {
	first := e.start.Add(-70 * 24 * time.Hour)
	master := own(uid, "james@mw.example", first, allowedAddr)
	master.RRule = "FREQ=WEEKLY;COUNT=20"
	p := e.mw.Add(mwCal, master)
	e.mw.Change(mwCal, p, func(evs []calendar.Event) []calendar.Event {
		rid := first.Add(7 * 24 * time.Hour)
		ov := evs[0]
		ov.RRule, ov.RecurrenceID = "", rid
		ov.Start, ov.End = rid, rid.Add(time.Hour)
		ov.Attendees = slices.Clone(ov.Attendees)
		mod(&ov)
		return append(evs, ov)
	})
	return p
}

// The CalDAV object and Google notify every override's attendees of a
// series change, old overrides calendar.db pruned included: the series is
// read from the store, and the change is checked against what it holds.
func TestSeriesChangeChecksPrunedOverrides(t *testing.T) {
	t.Parallel()
	var withBlocked, withCut, plainSeries string
	e := startCal(t, func(e *calEnv) {
		withBlocked = oldSeries(e, "old-blocked@x", func(ov *calendar.Event) {
			ov.Attendees = append(ov.Attendees, person(blockedAddr, calendar.PartStatNeedsAction))
		})
		withCut = oldSeries(e, "old-cut@x", func(ov *calendar.Event) { ov.AttendeesTruncated = true })
		plainSeries = oldSeries(e, "old-plain@x", func(*calendar.Event) {})
	})
	for _, p := range []string{withBlocked, withCut} {
		rows, err := e.cache.Series(t.Context(), "mw", mwCal, p)
		if err != nil || len(rows) != 1 {
			t.Fatalf("cached series %s = %d rows (%v), want only the master: the old override is pruned", p, len(rows), err)
		}
	}

	ref := calendar.EventRef("mw", mwCal, withBlocked)
	etag := e.etagOf(t, ref)
	got, err := e.cancel(t, ref, etag)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCancelled() || got.GetHeld() == nil || len(got.GetHeld().GetBlocked()) != 1 || got.GetHeld().GetBlocked()[0].GetUntrusted() != blockedAddr {
		t.Errorf("series cancel = %v, want held for the pruned override's attendee", got)
	}
	up, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("title"), Title: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if up.GetHeld() == nil || len(up.GetHeld().GetBlocked()) != 1 {
		t.Errorf("series update = %v, want held for the pruned override's attendee", up)
	}

	cref := calendar.EventRef("mw", mwCal, withCut)
	_, err = e.cancel(t, cref, e.etagOf(t, cref))
	wantCode(t, "cancel of a series with a truncated old override", err, connect.CodeFailedPrecondition)

	// A series that cannot be read fresh is refused, never checked
	// against calendar.db alone.
	pref := calendar.EventRef("mw", mwCal, plainSeries)
	petag := e.etagOf(t, pref)
	e.mw.FailNext(calfake.MethodGetEvent, errors.New("read failed "+secretText))
	_, err = e.cancel(t, pref, petag)
	wantReason(t, "cancel when the series read fails", err, api.ReasonServerUnavailable)
	e.mw.FailNext(calfake.MethodGetEvent, calstore.ErrUnavailable)
	_, err = e.update(t, &chainmailv1.UpdateEventRequest{Ref: pref, Etag: petag, UpdateMask: mask("title"), Title: "Renamed"})
	wantReason(t, "update when the series read fails", err, api.ReasonServerUnavailable)

	if n := e.mw.Count(calfake.MethodPut) + e.mw.Count(calfake.MethodDelete); n != 0 {
		t.Errorf("store writes = %d, want none", n)
	}
	noSecretInAudit(t, e.Harness)

	// With the read working, the plain series is cancelled.
	if got, err := e.cancel(t, pref, petag); err != nil || !got.GetCancelled() {
		t.Errorf("cancel of the plain series = %v, %v", got, err)
	}
}
