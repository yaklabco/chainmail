package daemon_test

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
)

// crossInvite is an event the work account organizes with the mw account
// invited, as each calendar holds it. The guest is listed first, as Google
// lists guests in the order they were added.
func crossInvite(uid string, e *calEnv) calendar.Event {
	ev := own(uid, "james@work.example", e.start)
	ev.Attendees = []calendar.Attendee{
		person("james@mw.example", calendar.PartStatNeedsAction),
		person("james@work.example", calendar.PartStatAccepted),
	}
	return ev
}

// One of James's accounts inviting another is an invite on the guest
// account: James on an account is that account's own address, not every
// account's (design section 6).
func TestInviteBetweenOwnAccounts(t *testing.T) {
	t.Parallel()
	var guest, host string
	e := startCal(t, func(e *calEnv) {
		guest = e.mw.Add(mwCal, crossInvite("cross@x", e))
		host = e.work.Add(workCal, crossInvite("cross@x", e))
	})
	ctx := t.Context()
	guestRef := calendar.EventRef("mw", mwCal, guest)
	hostRef := calendar.EventRef("work", workCal, host)
	// The same invite by mail to the store none account.
	mailInv := crossInvite("mailcross@x", e)
	mailInv.Attendees[0] = person("me@example.com", calendar.PartStatNeedsAction)
	mailRef := e.putMailInvite(t, "host", "host:INBOX:1:1", "REQUEST", mailInv)

	resp, err := e.Client.Calendar.ListInvites(ctx, connect.NewRequest(&chainmailv1.ListInvitesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*chainmailv1.CalendarEvent{}
	for _, inv := range resp.Msg.GetInvites() {
		got[inv.GetEvent().GetRef()] = inv.GetEvent()
	}
	if len(got) != 2 || got[guestRef] == nil || got[mailRef] == nil {
		t.Fatalf("invites = %v, want the mw calendar invite and the host mail invite", resp.Msg.GetInvites())
	}
	if g := got[guestRef]; g.GetIsOrganizer() || g.GetMyPartstat() != "NEEDS-ACTION" {
		t.Errorf("guest copy: organizer %v, my partstat %s; want an invite", g.GetIsOrganizer(), g.GetMyPartstat())
	}
	for _, a := range got[guestRef].GetAttendees() {
		if want := a.GetAddress().GetEmail().GetUntrusted() == "james@mw.example"; a.GetIsSelf() != want {
			t.Errorf("guest copy attendee %v: is_self %v", a.GetAddress(), a.GetIsSelf())
		}
	}

	// The organizer's copy: work organizes it and accepted, whoever is
	// listed first.
	ev, err := e.Client.Calendar.GetEvent(ctx, connect.NewRequest(&chainmailv1.GetEventRequest{Ref: hostRef}))
	if err != nil {
		t.Fatal(err)
	}
	if g := ev.Msg.GetEvent(); !g.GetIsOrganizer() || g.GetMyPartstat() != "ACCEPTED" {
		t.Errorf("organizer copy: organizer %v, my partstat %s; want organizer, ACCEPTED", g.GetIsOrganizer(), g.GetMyPartstat())
	}

	// mw answers through its store; the organizer is one of James's own
	// addresses, which the policy always allows.
	r, err := e.respond(t, &chainmailv1.RespondRequest{Ref: guestRef, Response: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if r.GetVia() != "store" || r.GetHeld() != nil || r.GetEvent().GetMyPartstat() != "ACCEPTED" {
		t.Errorf("respond = %v", r)
	}
	if calls := e.mw.CallsTo(calfake.MethodRespond); len(calls) != 1 || calls[0].Response.Self != "james@mw.example" {
		t.Errorf("Respond calls = %+v, want one as james@mw.example", calls)
	}
	// The store none account answers the mail invite by email to work,
	// which the policy allows as one of James's own addresses.
	srv := e.smtp(t)
	r, err = e.respond(t, &chainmailv1.RespondRequest{Ref: mailRef, Response: "decline"})
	if err != nil {
		t.Fatal(err)
	}
	if r.GetVia() != "email" || r.GetHeld() != nil {
		t.Errorf("respond by email = %v", r)
	}
	if msgs := srv.Messages(); len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "james@work.example" || msgs[0].From != "me@example.com" {
		t.Errorf("messages = %+v, want one REPLY from host to work", msgs)
	}

	// The organizer still cannot answer its own event.
	_, err = e.respond(t, &chainmailv1.RespondRequest{Ref: hostRef, Response: "decline"})
	wantReason(t, "organizer answers", err, api.ReasonInvalidArgument)
	if err == nil || !strings.Contains(err.Error(), "James organizes this event") {
		t.Errorf("organizer answers: %v, want the own event refusal", err)
	}

	// The guest copy is the organizer's to change, not mw's.
	_, err = e.Client.Calendar.UpdateEvent(ctx, connect.NewRequest(&chainmailv1.UpdateEventRequest{
		Ref: guestRef, Etag: got[guestRef].GetEtag(), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}, Title: "t",
	}))
	wantReason(t, "guest edits", err, api.ReasonNotOrganizer)
	_, err = e.Client.Calendar.CancelEvent(ctx, connect.NewRequest(&chainmailv1.CancelEventRequest{Ref: guestRef, Etag: got[guestRef].GetEtag()}))
	wantReason(t, "guest cancels", err, api.ReasonNotOrganizer)
}

// An event one of James's accounts creates with another of his accounts
// invited asks that account to answer, like any guest, and the organizer's
// account is listed as having accepted.
func TestCreateEventInvitingOwnAccount(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	got, err := e.create(t, &chainmailv1.CreateEventRequest{
		Account: "work", Title: "Plan", Start: ts(e.start), End: ts(e.start.Add(time.Hour)), Attendees: []string{"james@mw.example"}, IdempotencyKey: "own-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetHeld() != nil {
		t.Errorf("held = %v; James's own address is always allowed", got.GetHeld())
	}
	calls := e.work.CallsTo(calfake.MethodPut)
	if len(calls) != 1 || calls[0].Notify != calendar.NotifySend {
		t.Fatalf("Put calls = %+v, want one that notifies", calls)
	}
	put := calls[0].Event
	if len(put.Attendees) != 2 ||
		put.Attendees[0].Email != "james@work.example" || put.Attendees[0].PartStat != calendar.PartStatAccepted ||
		put.Attendees[1].Email != "james@mw.example" || put.Attendees[1].PartStat != calendar.PartStatNeedsAction || !put.Attendees[1].RSVP {
		t.Errorf("attendees put = %+v; want work ACCEPTED, then mw NEEDS-ACTION asked to answer", put.Attendees)
	}
}
