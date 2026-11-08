package daemon_test

import (
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
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
