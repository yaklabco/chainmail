package daemon_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
)

// smtp starts an smtptest server and points the proxy at it.
func (e *calEnv) smtp(t *testing.T) *smtptest.Server {
	t.Helper()
	srv := smtptest.StartWith(t, e.ca, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: "me@example.com", Password: "pw"},
		smtptest.User{Username: "james@mw.example", Password: "pw"},
		smtptest.User{Username: "james@work.example", Password: "pw"})
	e.proxy.target.Store(srv.Endpoint().Addr())
	return srv
}

// calendarPart returns the Content-Type and body of the text/calendar part
// of a raw message.
func calendarPart(t *testing.T, raw []byte) (string, []byte) {
	t.Helper()
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var (
		ctype string
		body  []byte
	)
	err = ent.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil {
			return err
		}
		if t, _, _ := part.Header.ContentType(); t == "text/calendar" {
			ctype = part.Header.Get("Content-Type")
			body, err = io.ReadAll(part.Body)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatalf("no text/calendar part in %s", raw)
	}
	return ctype, body
}

func (e *calEnv) respond(t *testing.T, req *chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
	t.Helper()
	resp, err := e.Client.Calendar.Respond(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestRespondThroughTheStore(t *testing.T) {
	t.Parallel()
	var single, weekly string
	e := startCal(t, func(e *calEnv) {
		single = e.mw.Add(mwCal, invitation("single@x", "james@mw.example", e.start))
		master := invitation("weekly@x", "james@mw.example", e.start)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		weekly = e.mw.Add(mwCal, master)
	})
	ref := calendar.EventRef("mw", mwCal, single)
	got, err := e.respond(t, &chainmailv1.RespondRequest{Ref: ref, Response: "accept", Comment: "Comment " + secretText})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetVia() != "store" || got.GetHeld() != nil || got.GetEvent().GetMyPartstat() != "ACCEPTED" || got.GetEvent().GetRef() != ref {
		t.Errorf("response = %v", got)
	}
	checkUntrusted(t, got)
	calls := e.mw.CallsTo(calfake.MethodRespond)
	if len(calls) != 1 {
		t.Fatalf("Respond calls = %d, want 1", len(calls))
	}
	if r := calls[0].Response; r.Self != "james@mw.example" || r.PartStat != calendar.PartStatAccepted || r.Comment != "Comment "+secretText || !r.RecurrenceID.IsZero() {
		t.Errorf("response sent = %+v", r)
	}
	if r := auditRow(t, e.Harness, "calendar.respond"); r.Result != "ok" || r.Account != "mw" || !strings.Contains(r.Targets, ref) {
		t.Errorf("audit row %+v", r)
	}

	// One instance of a series: only that instance is answered.
	second := e.start.Add(7 * 24 * time.Hour)
	iref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, weekly), second)
	got, err = e.respond(t, &chainmailv1.RespondRequest{Ref: iref, Response: "tentative", IdempotencyKey: "rsvp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetEvent().GetMyPartstat() != "TENTATIVE" || got.GetEvent().GetRef() != iref {
		t.Errorf("instance response = %v", got.GetEvent())
	}
	calls = e.mw.CallsTo(calfake.MethodRespond)
	if len(calls) != 2 || !calls[1].Response.RecurrenceID.Equal(second) || calls[1].Response.PartStat != calendar.PartStatTentative {
		t.Fatalf("Respond calls = %+v", calls)
	}
	// The same key replays the first answer without asking the store.
	again, err := e.respond(t, &chainmailv1.RespondRequest{Ref: iref, Response: "tentative", IdempotencyKey: "rsvp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.mw.Count(calfake.MethodRespond); n != 2 || again.GetEvent().GetRef() != iref {
		t.Errorf("replay: Respond calls = %d, event %v", n, again.GetEvent())
	}
	_, err = e.respond(t, &chainmailv1.RespondRequest{Ref: iref, Response: "decline", IdempotencyKey: "rsvp-1"})
	wantReason(t, "key reused for another answer", err, api.ReasonInvalidArgument)
	noSecretInAudit(t, e.Harness)
}

func TestRespondThroughTheStoreHeld(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		ev := invitation("held@x", "james@mw.example", e.start)
		ev.Organizer.Email = blockedAddr
		p = e.mw.Add(mwCal, ev)
	})
	got, err := e.respond(t, &chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, p), Response: "decline"})
	if err != nil {
		t.Fatal(err)
	}
	held := got.GetHeld()
	if held == nil || got.GetVia() != "store" || got.GetEvent() != nil || held.GetHint() == "" ||
		len(held.GetBlocked()) != 1 || held.GetBlocked()[0].GetUntrusted() != blockedAddr {
		t.Fatalf("response = %v, want held", got)
	}
	if n := e.mw.Count(calfake.MethodRespond); n != 0 {
		t.Errorf("Respond calls = %d, want none: a held answer is not applied", n)
	}
	if r := auditRow(t, e.Harness, "calendar.respond"); r.Result != "ok" || r.Reason != api.ReasonHeld || !strings.Contains(r.Targets, "blocked:1") {
		t.Errorf("audit row %+v", r)
	}
	if strings.Contains(fmtRows(t, e), blockedAddr) {
		t.Error("an audit row names the blocked address")
	}
}

func fmtRows(t *testing.T, e *calEnv) string {
	t.Helper()
	var b strings.Builder
	for _, r := range e.AuditRows(t) {
		b.WriteString(r.Targets + " ")
	}
	return b.String()
}

func TestRespondByEmail(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	srv := e.smtp(t)
	ref := e.putMailInvite(t, "host", "host:INBOX:1:1", "REQUEST", invitation("mail@x", "me@example.com", e.start))
	got, err := e.respond(t, &chainmailv1.RespondRequest{Ref: ref, Response: "decline", Comment: "Sorry " + secretText})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetVia() != "email" || got.GetHeld() != nil {
		t.Errorf("response = %v", got)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != allowedAddr || msgs[0].From != "me@example.com" {
		t.Fatalf("messages = %+v, want one REPLY to the organizer", msgs)
	}
	ctype, body := calendarPart(t, msgs[0].Data)
	if !strings.Contains(strings.ToLower(ctype), "method=reply") {
		t.Errorf("Content-Type = %q, want method=REPLY", ctype)
	}
	method, evs, err := calendar.ParseICS(body)
	if err != nil {
		t.Fatal(err)
	}
	if method != calendar.MethodReply || len(evs) != 1 || evs[0].UID != "mail@x" || len(evs[0].Attendees) != 1 ||
		evs[0].Attendees[0].Email != "me@example.com" || evs[0].Attendees[0].PartStat != calendar.PartStatDeclined {
		t.Errorf("REPLY = %s %+v", method, evs)
	}
	if r := auditRow(t, e.Harness, "rsvp"); r.Result != "ok" || r.Account != "host" {
		t.Errorf("rsvp audit row %+v", r)
	}
	if r := auditRow(t, e.Harness, "calendar.respond"); r.Result != "ok" || !strings.Contains(r.Targets, ref) {
		t.Errorf("calendar.respond audit row %+v", r)
	}
	noSecretInAudit(t, e.Harness)
}

func TestRespondByEmailHeld(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	ev := invitation("mail@x", "me@example.com", e.start)
	ev.Organizer.Email = blockedAddr
	ref := e.putMailInvite(t, "host", "host:INBOX:1:1", "REQUEST", ev)
	got, err := e.respond(t, &chainmailv1.RespondRequest{Ref: ref, Response: "accept", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	held := got.GetHeld()
	if got.GetVia() != "email" || held == nil || held.GetDraftRef() == "" || len(held.GetBlocked()) != 1 {
		t.Fatalf("response = %v, want held with a draft", got)
	}
	drafts := e.mail["host"].Appended()
	if len(drafts) != 1 {
		t.Fatalf("appended %d messages, want the draft", len(drafts))
	}
	ctype, _ := calendarPart(t, drafts[0])
	if !strings.Contains(strings.ToLower(ctype), "method=reply") {
		t.Errorf("draft Content-Type = %q", ctype)
	}
	if r := auditRow(t, e.Harness, "calendar.respond"); r.Reason != api.ReasonHeld {
		t.Errorf("audit row %+v", r)
	}
}

func TestRespondRefusals(t *testing.T) {
	t.Parallel()
	var mine, other string
	e := startCal(t, func(e *calEnv) {
		mine = e.mw.Add(mwCal, own("mine@x", "james@mw.example", e.start, allowedAddr))
		ev := invitation("other@x", "someone@example.org", e.start)
		other = e.mw.Add(mwCal, ev)
	})
	for name, tt := range map[string]struct {
		req  *chainmailv1.RespondRequest
		want string
	}{
		"bad response": {&chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, mine), Response: "maybe " + secretText}, api.ReasonInvalidArgument},
		"own event":    {&chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, mine), Response: "accept"}, api.ReasonInvalidArgument},
		"not attendee": {&chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, other), Response: "accept"}, api.ReasonInvalidArgument},
		"unknown ref":  {&chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, "nope"), Response: "accept"}, api.ReasonNotFound},
		"bad ref":      {&chainmailv1.RespondRequest{Ref: "x", Response: "accept"}, api.ReasonInvalidArgument},
		"bad key":      {&chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, mine), Response: "accept", IdempotencyKey: "a b"}, api.ReasonInvalidArgument},
		"long comment": {&chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, other), Response: "accept", Comment: strings.Repeat("x", 4097)}, api.ReasonInvalidArgument},
	} {
		_, err := e.respond(t, tt.req)
		wantReason(t, name, err, tt.want)
		if err != nil && strings.Contains(err.Error(), secretText) {
			t.Errorf("%s: error %v repeats the request", name, err)
		}
	}
	if n := e.mw.Count(calfake.MethodRespond); n != 0 {
		t.Errorf("Respond calls = %d, want none", n)
	}
}

// An answer is bound to the version whose organizer was checked: when the
// organizer changed on the server, the answer is checked again against the
// new one, here held, and nothing is written.
func TestRespondRechecksAChangedOrganizer(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		p = e.mw.Add(mwCal, invitation("moved@x", "james@mw.example", e.start))
	})
	// The cache still has boss as the organizer.
	e.mw.Change(mwCal, p, func(evs []calendar.Event) []calendar.Event {
		evs[0].Organizer.Email = blockedAddr
		return evs
	})
	got, err := e.respond(t, &chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, p), Response: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetHeld() == nil || len(got.GetHeld().GetBlocked()) != 1 || got.GetHeld().GetBlocked()[0].GetUntrusted() != blockedAddr {
		t.Fatalf("response = %v, want held for the new organizer", got)
	}
	rows, _ := e.mw.Object(mwCal, p)
	for _, a := range rows[0].Attendees {
		if a.Email == "james@mw.example" && a.PartStat != calendar.PartStatNeedsAction {
			t.Errorf("the answer reached the store: %+v", a)
		}
	}
	calls := e.mw.CallsTo(calfake.MethodRespond)
	if len(calls) != 1 || calls[0].Response.ETag == "" {
		t.Errorf("Respond calls = %+v, want one bound to the cached etag", calls)
	}
}

// An answer James gave elsewhere since the caller read the event is not
// overwritten.
func TestRespondKeepsANewerAnswer(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		p = e.mw.Add(mwCal, invitation("answered@x", "james@mw.example", e.start))
	})
	e.mw.SetPartStat(mwCal, p, time.Time{}, "james@mw.example", calendar.PartStatDeclined)
	_, err := e.respond(t, &chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwCal, p), Response: "accept"})
	wantReason(t, "answer changed elsewhere", err, api.ReasonEventConflict)
	rows, _ := e.mw.Object(mwCal, p)
	for _, a := range rows[0].Attendees {
		if a.Email == "james@mw.example" && a.PartStat != calendar.PartStatDeclined {
			t.Errorf("the newer answer was overwritten: %+v", a)
		}
	}
}

// An event in a calendar the account may only read is not answered
// through the store.
func TestRespondInAReadOnlyCalendar(t *testing.T) {
	t.Parallel()
	var p string
	e := startCal(t, func(e *calEnv) {
		p = e.mw.Add(mwShared, invitation("shared@x", "james@mw.example", e.start))
	})
	_, err := e.respond(t, &chainmailv1.RespondRequest{Ref: calendar.EventRef("mw", mwShared, p), Response: "accept"})
	wantReason(t, "read-only calendar", err, api.ReasonCalendarReadOnly)
	if n := e.mw.Count(calfake.MethodRespond); n != 0 {
		t.Errorf("Respond calls = %d, want none", n)
	}
}
