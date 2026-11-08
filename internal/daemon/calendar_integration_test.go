//go:build integration

package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore/imapstore"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/dovecot"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
)

const (
	// calItAllowed organizes the invite James may answer: the policy
	// allows *@allowed.test.
	calItAllowed = "chair@allowed.test"
	// calItBlocked organizes the invite whose answer the policy holds.
	calItBlocked = "chair@blocked.test"
	// calItInviteWithin bounds how long a delivered invite takes to be
	// listed: the new event, the cache's header fetch, the part read and
	// the invite hook.
	calItInviteWithin = 30 * time.Second
)

// calItOptions are the harness options of a calendar invite step: watching
// with the cache, so new mail reaches the invite hook, and calendar.db
// open. The account's provider is custom, so its calendar store is none
// and the daemon starts no calendar syncer.
func calItOptions(dove *dovecot.Server) daemontest.Options {
	o := itDoveOptions(dove, nil, nil)
	o.Calendar = true
	return o
}

// calItICS is an iTIP REQUEST for one event an hour long from start, with
// uid, organized by organizer, with self invited and not yet answered. The
// summary and the organizer's name hold secretText, which no audit row
// and no plain field may carry.
func calItICS(uid, organizer, self string, start time.Time) string {
	const stamp = "20060102T150405Z"
	return strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//chainmail integration test//EN",
		"METHOD:REQUEST",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + time.Now().UTC().Format(stamp),
		"SEQUENCE:0",
		"DTSTART:" + start.UTC().Format(stamp),
		"DTEND:" + start.Add(time.Hour).UTC().Format(stamp),
		"SUMMARY:Planning " + secretText,
		"STATUS:CONFIRMED",
		"ORGANIZER;CN=Chair " + secretText + ":mailto:" + organizer,
		"ATTENDEE;PARTSTAT=ACCEPTED;ROLE=CHAIR:mailto:" + organizer,
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;ROLE=REQ-PARTICIPANT:mailto:" + self,
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
}

// calItMail is an invitation email from organizer to self, as calendar
// apps send them: a multipart/alternative with a text part and the
// REQUEST as a text/calendar part with method=REQUEST.
func calItMail(uid, organizer, self, id string, start time.Time) string {
	return "From: Chair <" + organizer + ">\r\n" +
		"To: " + self + "\r\n" +
		"Subject: Invitation: Planning\r\n" +
		"Message-ID: <" + id + "@example.org>\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"calit-boundary\"\r\n" +
		"\r\n" +
		"--calit-boundary\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\r\n" +
		"You are invited to Planning.\r\n" +
		"--calit-boundary\r\n" +
		"Content-Type: text/calendar; charset=utf-8; method=REQUEST\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\r\n" +
		calItICS(uid, organizer, self, start) +
		"--calit-boundary--\r\n"
}

// TestCalendarInvitesAgainstDovecot runs the mail side of calendars end to
// end (spec section 11, Dovecot integration) on one Dovecot container. An
// invite delivered by mail to a store none account is found by the cache
// builder, listed by ListInvites and answered by an iTIP REPLY email; a
// reply to an organizer off the allowlist is held and saved to Drafts.
// Each step has a Dovecot user and a daemon of its own.
func TestCalendarInvitesAgainstDovecot(t *testing.T) {
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	t.Run("answered by email", func(t *testing.T) { checkCalItAnswered(t, dove) })
	t.Run("held reply", func(t *testing.T) { checkCalItHeld(t, dove) })
}

// calItWaitInvite polls ListInvites on account until it lists the invite
// with ref, and returns it.
func calItWaitInvite(t *testing.T, h *daemontest.Harness, account, inviteRef string) *chainmailv1.Invite {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), calItInviteWithin)
	defer cancel()
	var last []*chainmailv1.Invite
	for {
		resp, err := h.Client.Calendar.ListInvites(ctx, connect.NewRequest(&chainmailv1.ListInvitesRequest{Accounts: []string{account}}))
		if err != nil {
			t.Fatalf("ListInvites on %s: %v (last invites %v)", account, err, last)
		}
		last = resp.Msg.GetInvites()
		for _, inv := range last {
			if inv.GetEvent().GetRef() == inviteRef {
				checkUntrusted(t, resp.Msg)
				return inv
			}
		}
		if !itPause(ctx) {
			t.Fatalf("ListInvites on %s does not list %s within %v: %v", account, inviteRef, calItInviteWithin, last)
		}
	}
}

// calItDeliver appends an invitation from organizer to user's INBOX, waits
// for ListInvites to list it and checks the invite against the delivered
// message: source mail, method REQUEST, the message's ref, James not yet
// answered. It returns the invite's ref.
func calItDeliver(t *testing.T, h *daemontest.Harness, dove *dovecot.Server, user, organizer, uid, id string) string {
	t.Helper()
	start := time.Now().Add(48 * time.Hour).Truncate(time.Hour).UTC()
	if _, err := dove.Append(user, "INBOX", calItMail(uid, organizer, user, id, start), time.Now()); err != nil {
		t.Fatal(err)
	}
	inviteRef := calendar.MailInviteRef("dove", uid)
	inv := calItWaitInvite(t, h, "dove", inviteRef)
	inbox := itServerRows(t, h, "dove", "in:inbox")
	if len(inbox) != 1 {
		t.Fatalf("INBOX holds %v, want the invitation", inbox)
	}
	ev := inv.GetEvent()
	if inv.GetSource() != "mail" || inv.GetMethod() != calendar.MethodRequest {
		t.Errorf("invite source %q, method %q, want mail and REQUEST", inv.GetSource(), inv.GetMethod())
	}
	if ev.GetAccount() != "dove" || ev.GetUid() != uid || ev.GetMessageRef() != inbox[0].GetRef() {
		t.Errorf("invite event account %q, uid %q, message ref %q, want dove, %q and %q",
			ev.GetAccount(), ev.GetUid(), ev.GetMessageRef(), uid, inbox[0].GetRef())
	}
	if ev.GetMyPartstat() != string(calendar.PartStatNeedsAction) || ev.GetIsOrganizer() {
		t.Errorf("invite event partstat %q, organizer %v, want NEEDS-ACTION and not the organizer", ev.GetMyPartstat(), ev.GetIsOrganizer())
	}
	if !ev.GetStart().AsTime().Equal(start) || !ev.GetEnd().AsTime().Equal(start.Add(time.Hour)) {
		t.Errorf("invite event runs %v to %v, want %v for an hour", ev.GetStart().AsTime(), ev.GetEnd().AsTime(), start)
	}
	if got := ev.GetOrganizer().GetEmail().GetUntrusted(); got != organizer {
		t.Errorf("invite organizer %q, want %q", got, organizer)
	}
	return inviteRef
}

// calItCheckReply fails the test unless raw carries an iTIP REPLY for uid
// in a text/calendar part with method=REPLY, with self as the only
// attendee, at PARTSTAT ACCEPTED.
func calItCheckReply(t *testing.T, raw []byte, uid, self string) {
	t.Helper()
	ctype, body := calendarPart(t, raw)
	if !strings.Contains(strings.ToLower(ctype), "method=reply") {
		t.Errorf("calendar part Content-Type = %q, want method=REPLY", ctype)
	}
	if !strings.Contains(string(body), "PARTSTAT=ACCEPTED") {
		t.Errorf("REPLY lacks PARTSTAT=ACCEPTED:\n%s", body)
	}
	method, evs, err := calendar.ParseICS(body)
	if err != nil {
		t.Fatalf("parsing the REPLY: %v", err)
	}
	if method != calendar.MethodReply || len(evs) != 1 || evs[0].UID != uid || len(evs[0].Attendees) != 1 ||
		evs[0].Attendees[0].Email != self || evs[0].Attendees[0].PartStat != calendar.PartStatAccepted {
		t.Errorf("REPLY = %s %+v, want one event %s with %s alone at ACCEPTED", method, evs, uid, self)
	}
}

// checkCalItAnswered is done-when item 3 on Dovecot: an invite delivered
// to a store none account is listed by ListInvites and answered with an
// iTIP REPLY email to its organizer.
func checkCalItAnswered(t *testing.T, dove *dovecot.Server) {
	const (
		user = "cal-answer@example.test"
		uid  = "answer-1@calendar.example.org"
	)
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: user, Password: dovecot.Password})
	h := daemontest.Start(t, fmt.Sprintf(itDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port), calItOptions(dove))
	itWaitCache(t, h, "dove", itDoveFolders)
	inviteRef := calItDeliver(t, h, dove, user, calItAllowed, uid, "calanswer1")

	resp, err := h.Client.Calendar.Respond(t.Context(), connect.NewRequest(&chainmailv1.RespondRequest{
		Ref: inviteRef, Response: "accept", IdempotencyKey: "calit-answer",
	}))
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	got := resp.Msg
	if got.GetVia() != "email" || got.GetHeld() != nil || got.GetSentRef() == "" {
		t.Fatalf("Respond = %v, want sent by email with a sent ref", got)
	}
	if ev := got.GetEvent(); ev.GetRef() != inviteRef || ev.GetMyPartstat() != string(calendar.PartStatAccepted) {
		t.Errorf("Respond event ref %q, partstat %q, want %s at ACCEPTED", ev.GetRef(), ev.GetMyPartstat(), inviteRef)
	}
	checkUntrusted(t, got)

	msgs := sink.Messages()
	if len(msgs) != 1 {
		t.Fatalf("SMTP got %d messages, want the REPLY", len(msgs))
	}
	if msgs[0].From != user || !slices.Equal(msgs[0].To, []string{calItAllowed}) {
		t.Errorf("envelope = %s to %v, want %s to [%s]", msgs[0].From, msgs[0].To, user, calItAllowed)
	}
	calItCheckReply(t, msgs[0].Data, uid, user)

	env := sendEnv{Harness: h}
	if d := env.decisions(t, "rsvp"); !slices.Equal(d, []string{"allowed"}) {
		t.Errorf("rsvp decisions = %v, want [allowed]", d)
	}
	if r := auditRow(t, h, "calendar.respond"); r.Result != "ok" || r.Account != "dove" || !strings.Contains(r.Targets, inviteRef) {
		t.Errorf("calendar.respond audit row %+v, want ok on dove naming %s", r, inviteRef)
	}
	noSecretInAudit(t, h)
}

// checkCalItHeld checks that an answer to an organizer off the allowlist
// is held: nothing reaches SMTP, and the REPLY is saved to Dovecot's
// Drafts with \Draft.
func checkCalItHeld(t *testing.T, dove *dovecot.Server) {
	const (
		user = "cal-held@example.test"
		uid  = "held-1@calendar.example.org"
	)
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: user, Password: dovecot.Password})
	cfgTOML := fmt.Sprintf(itDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port)
	h := daemontest.Start(t, cfgTOML, calItOptions(dove))
	itWaitCache(t, h, "dove", itDoveFolders)
	inviteRef := calItDeliver(t, h, dove, user, calItBlocked, uid, "calheld1")

	resp, err := h.Client.Calendar.Respond(t.Context(), connect.NewRequest(&chainmailv1.RespondRequest{
		Ref: inviteRef, Response: "accept", IdempotencyKey: "calit-held",
	}))
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	got := resp.Msg
	held := got.GetHeld()
	if got.GetVia() != "email" || held == nil || held.GetDraftRef() == "" || got.GetSentRef() != "" {
		t.Fatalf("Respond = %v, want held by email with a draft ref", got)
	}
	if b := held.GetBlocked(); len(b) != 1 || b[0].GetUntrusted() != calItBlocked || held.GetTooMany() {
		t.Errorf("held blocked %v, too many %v, want %s alone", b, held.GetTooMany(), calItBlocked)
	}
	if n, froms := len(sink.Messages()), sink.MailFroms(); n != 0 || len(froms) != 0 {
		t.Fatalf("SMTP sink got %d messages and MAIL FROM %v, want nothing", n, froms)
	}

	// The REPLY is in Dovecot's Drafts, read back through the IMAP store.
	draftRef := parseRef(t, held.GetDraftRef())
	if draftRef.Folder != "Drafts" {
		t.Errorf("draft ref folder = %q, want Drafts", draftRef.Folder)
	}
	cfg, err := config.Parse([]byte(cfgTOML))
	if err != nil {
		t.Fatal(err)
	}
	acct, err := cfg.Account("dove")
	if err != nil {
		t.Fatal(err)
	}
	st := imapstore.New(acct, passwordCreds{user: user}, imapstore.Options{RootCAs: dove.CA.Pool})
	t.Cleanup(func() { _ = st.Close() })
	msgs := st.FetchMessages(t.Context(), []ref.Ref{draftRef})
	if len(msgs) != 1 || msgs[0].Err != nil {
		t.Fatalf("FetchMessages(%v) = %+v", draftRef, msgs)
	}
	draft := msgs[0].Message
	if !slices.Contains(draft.Summary.Flags, `\Draft`) {
		t.Errorf("draft flags = %v, want \\Draft", draft.Summary.Flags)
	}
	if to, err := header(t, draft.Raw).AddressList("To"); err != nil || len(to) != 1 || to[0].Address != calItBlocked {
		t.Errorf("draft To = %v, %v, want %s", to, err, calItBlocked)
	}
	calItCheckReply(t, draft.Raw, uid, user)

	env := sendEnv{Harness: h}
	if d := env.decisions(t, "rsvp"); !slices.Equal(d, []string{"held"}) {
		t.Errorf("rsvp decisions = %v, want [held]", d)
	}
	if r := auditRow(t, h, "calendar.respond"); r.Reason != api.ReasonHeld || r.Account != "dove" {
		t.Errorf("calendar.respond audit row %+v, want reason %s on dove", r, api.ReasonHeld)
	}
	noSecretInAudit(t, h)
}
