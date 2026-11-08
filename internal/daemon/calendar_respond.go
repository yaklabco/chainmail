package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/compose"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/idempotency"
	"github.com/yaklabco/chainmail/internal/render"
	"github.com/yaklabco/chainmail/internal/version"
)

// How a reply went.
const (
	viaStore = "store"
	viaEmail = "email"
)

const (
	// maxCommentBytes bounds an RSVP comment.
	maxCommentBytes = 4096
	// maxReplySubjectRunes bounds the event summary in a REPLY's subject.
	maxReplySubjectRunes = 200
	// rsvpStoreHeldHint is CalendarHeld.hint for a held answer through a
	// calendar store, which was not applied.
	rsvpStoreHeldHint = "held by policy: the answer was not sent; James can answer in the calendar app"
	// rsvpEmailHeldHint is CalendarHeld.hint for a held answer by email.
	rsvpEmailHeldHint = "held by policy: the reply was saved to Drafts for James to review and send"
)

var (
	errRSVPResponse = &argError{msg: "response must be accept, decline or tentative", hint: "pass accept, decline or tentative"}
	errRSVPComment  = &argError{msg: "the comment is longer than 4096 bytes", hint: "shorten the comment"}
)

// rsvpPartStats maps a response to the PARTSTAT it sets.
var rsvpPartStats = map[string]calendar.PartStat{
	"accept":    calendar.PartStatAccepted,
	"decline":   calendar.PartStatDeclined,
	"tentative": calendar.PartStatTentative,
}

// rsvpTarget is what an RSVP answers.
type rsvpTarget struct {
	ca *calAccount
	// cal is the calendar event, when the ref names one. It is answered
	// through the account's store when there is one.
	cal *resolvedEvent
	// ev is the event answered: a single event, a series master, an
	// override, or an instance (RecurrenceID set) made from the master.
	ev calendar.Event
	// invite is the mail invite, when the ref names one.
	invite *calcache.MailInviteRow
	// self is James's attendee address on the event, organizer the
	// organizer's address.
	self, organizer string
}

// Respond implements chainmailv1connect.CalendarServiceHandler (spec 6.1).
// The organizer is checked against the send policy. An event in a
// calendar store is answered through the store; a held answer is not
// applied. Otherwise an iTIP REPLY goes to the organizer through the send
// pipeline as action rsvp, so a held reply is saved to Drafts.
func (h *calendarService) Respond(ctx context.Context, req *connect.Request[chainmailv1.RespondRequest]) (*connect.Response[chainmailv1.RespondResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.respond", calendarWriteTimeout, func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.RespondResponse, error) {
		m := req.Msg
		target, _, err := eventRefTarget(m.GetRef())
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		ps, ok := rsvpPartStats[m.GetResponse()]
		if !ok {
			return nil, errRSVPResponse
		}
		if len(m.GetComment()) > maxCommentBytes {
			return nil, errRSVPComment
		}
		if k := m.GetIdempotencyKey(); k != "" && !idempotency.ValidKey(k) {
			return nil, errCalendarKey
		}
		t, err := s.rsvpTargetOf(ctx, m.GetRef())
		if err != nil {
			return nil, err
		}
		entry.Account = t.ca.name
		if t.cal != nil && t.ca.store != nil {
			entry.Targets = append(entry.Targets, "via="+viaStore)
			return s.respondInStore(ctx, m, t, ps, entry)
		}
		entry.Targets = append(entry.Targets, "via="+viaEmail)
		return s.respondByEmail(ctx, c, m, t, ps, entry)
	})
}

// rsvpTargetOf resolves an RSVP's ref: a calendar event, or a mail invite,
// which the account's calendar event with the same UID stands for when
// there is one. It checks that James is an attendee and that someone else
// organizes the event.
func (s *Server) rsvpTargetOf(ctx context.Context, ref string) (rsvpTarget, error) {
	var t rsvpTarget
	res, err := s.resolveEvent(ctx, ref)
	switch {
	case err == nil:
		t.cal, t.ev = &res, res.ev
	case !errors.Is(err, calcache.ErrNotFound):
		return rsvpTarget{}, err
	default:
		r, err := s.calcache.MailInvite(ctx, ref)
		if err != nil {
			return rsvpTarget{}, err
		}
		if r.Method == calendar.MethodCancel {
			return rsvpTarget{}, calcache.ErrNotFound
		}
		if res, ok := s.calendarEventOfUID(ctx, r.Account, r.UID); ok {
			t.cal, t.ev = &res, res.ev
		} else {
			ev, _, ok := s.parseMailInvite(r)
			if !ok {
				return rsvpTarget{}, calcache.ErrNotFound
			}
			t.invite, t.ev = &r, ev
		}
	}
	account := ""
	if t.cal != nil {
		account = t.cal.row.Account
	} else {
		account = t.invite.Account
	}
	ca, ok := s.calendars[account]
	if !ok {
		return rsvpTarget{}, calcache.ErrNotFound
	}
	t.ca = ca
	if t.ev.Organizer == nil || t.ev.Organizer.Email == "" {
		if t.cal == nil || t.cal.master == nil || t.cal.master.Event.Organizer == nil {
			return rsvpTarget{}, errNoOrganizer
		}
		t.ev.Organizer = t.cal.master.Event.Organizer
	}
	t.organizer = t.ev.Organizer.Email
	if s.ownAddress(t.organizer) {
		return rsvpTarget{}, errOwnEvent
	}
	acct, err := s.opts.Config.Account(account)
	if err != nil {
		return rsvpTarget{}, err
	}
	t.self = s.selfAttendee(t.ev, acct.Address)
	if t.self == "" && t.cal != nil && t.cal.master != nil && len(t.ev.Attendees) == 0 {
		t.self = s.selfAttendee(t.cal.master.Event, acct.Address)
	}
	if t.self == "" {
		return rsvpTarget{}, errNotAttendee
	}
	return t, nil
}

// selfAttendee is James's attendee address on ev: the account's own
// address when it is listed, else the first of his addresses listed, else
// "".
func (s *Server) selfAttendee(ev calendar.Event, address string) string {
	first := ""
	for _, a := range ev.Attendees {
		switch {
		case strings.EqualFold(a.Email, address):
			return a.Email
		case first == "" && s.ownAddress(a.Email):
			first = a.Email
		}
	}
	return first
}

// respondInStore answers through the account's calendar store. With a
// caller's key, a repeat replays the first outcome. A conflict reads the
// event again once and retries when James's answer is still what it was.
func (s *Server) respondInStore(ctx context.Context, m *chainmailv1.RespondRequest, t rsvpTarget, ps calendar.PartStat, entry *audit.Entry) (*chainmailv1.RespondResponse, error) {
	account := t.ca.name
	key := ""
	if m.GetIdempotencyKey() != "" {
		key = calendarKey("cal-rsvp", m.GetIdempotencyKey())
		hash, err := calendarHash(m)
		if err != nil {
			return nil, err
		}
		claim, err := s.claimCalendar(ctx, account, key, hash)
		if err != nil {
			return nil, err
		}
		if claim.result != nil {
			entry.Decision = decisionDuplicate
			out := &chainmailv1.RespondResponse{}
			if err := proto.Unmarshal(claim.result, out); err != nil {
				return nil, err
			}
			return out, nil
		}
		if claim.unknown {
			key = ""
		}
	}
	out, err := s.answerInStore(ctx, m, t, ps, entry)
	if key != "" {
		s.settleCalendar(ctx, account, key, out, err)
	}
	return out, err
}

func (s *Server) answerInStore(ctx context.Context, m *chainmailv1.RespondRequest, t rsvpTarget, ps calendar.PartStat, entry *audit.Entry) (*chainmailv1.RespondResponse, error) {
	if d := s.policy.CheckSend(t.ca.name, []string{t.organizer}); !d.Allowed {
		heldTargets(entry, len(d.Blocked), d.TooMany)
		return &chainmailv1.RespondResponse{Via: viaStore, Held: &chainmailv1.CalendarHeld{
			Hint: rsvpStoreHeldHint, Blocked: untrustedList(d.Blocked), TooMany: d.TooMany,
		}}, nil
	}
	entry.Decision = decisionAllowed
	res := *t.cal
	before, _ := res.role()
	r := calendar.Response{Self: t.self, PartStat: ps, Comment: m.GetComment(), RecurrenceID: res.ev.RecurrenceID}
	_, err := t.ca.store.Respond(ctx, res.row.CalendarID, res.row.Event.ProviderID, r)
	if errors.Is(err, calstore.ErrConflict) {
		s.syncAfterWrite(ctx, t.ca)
		again, rerr := s.resolveEvent(ctx, m.GetRef())
		if now, _ := again.role(); rerr == nil && now == before {
			res = again
			_, err = t.ca.store.Respond(ctx, res.row.CalendarID, res.row.Event.ProviderID, r)
		}
	}
	if err != nil {
		return nil, calendarError(t.ca.name, err)
	}
	s.syncAfterWrite(ctx, t.ca)
	v := viewOfResolved(res)
	if after, err := s.resolveEvent(ctx, res.ref); err == nil {
		v = viewOfResolved(after)
	} else {
		v.ev, v.myPartStat = withPartStat(v.ev, t.self, ps), ps
	}
	return &chainmailv1.RespondResponse{Via: viaStore, Event: eventProto(v, s.ownAddress)}, nil
}

// withPartStat returns ev with self's PARTSTAT set to ps.
func withPartStat(ev calendar.Event, self string, ps calendar.PartStat) calendar.Event {
	ev.Attendees = append([]calendar.Attendee(nil), ev.Attendees...)
	for i, a := range ev.Attendees {
		if strings.EqualFold(a.Email, self) {
			ev.Attendees[i].PartStat = ps
		}
	}
	return ev
}

// respondByEmail sends an iTIP REPLY to the organizer through the send
// pipeline as action rsvp, which checks the organizer against the policy
// and saves a held reply to Drafts. Without a caller's key a fresh one is
// minted, so answering again later is a new reply.
func (s *Server) respondByEmail(ctx context.Context, c caller, m *chainmailv1.RespondRequest, t rsvpTarget, ps calendar.PartStat, entry *audit.Entry) (*chainmailv1.RespondResponse, error) {
	acct, err := s.opts.Config.Account(t.ca.name)
	if err != nil {
		return nil, err
	}
	now := s.opts.Now()
	reply, err := calendar.BuildReply(t.ev, t.self, ps, m.GetComment(), now)
	if err != nil {
		return nil, err
	}
	spec := replySpec(acct, t, ps, m.GetComment(), reply, now)
	key := m.GetIdempotencyKey()
	if key == "" {
		key = mintKey()
	}
	hash, err := calendarHash(m)
	if err != nil {
		return nil, err
	}
	res, err := s.compose.run(ctx, sendJob{
		acct: acct, key: calendarKey("rsvp", key), hash: hash,
		build:  func(context.Context) (compose.Message, error) { return compose.Build(spec) },
		action: "rsvp", targets: []string{entry.Targets[0]}, caller: c,
	})
	if err != nil {
		return nil, err
	}
	if held := res.GetHeld(); held != nil {
		heldTargets(entry, len(held.GetBlocked()), held.GetTooManyRecipients())
		return &chainmailv1.RespondResponse{Via: viaEmail, Held: &chainmailv1.CalendarHeld{
			Hint: rsvpEmailHeldHint, Blocked: untrustedList(held.GetBlocked()), TooMany: held.GetTooManyRecipients(),
			DraftRef: held.GetDraft().GetRef(),
		}}, nil
	}
	sent := res.GetSent()
	entry.Decision = decisionAllowed
	if sent.GetDuplicate() {
		entry.Decision = decisionDuplicate
	}
	v := eventView{ev: withPartStat(t.ev, t.self, ps), myPartStat: ps, account: t.ca.name}
	switch {
	case t.cal != nil:
		v = viewOfResolved(*t.cal)
		v.ev, v.myPartStat = withPartStat(v.ev, t.self, ps), ps
	case t.invite != nil:
		v.ref, v.messageRef, v.rrule, v.recurring = t.invite.Ref, t.invite.MessageRef, t.ev.RRule, isSeries(t.ev)
	}
	return &chainmailv1.RespondResponse{Via: viaEmail, SentRef: sent.GetSentRef(), Event: eventProto(v, s.ownAddress)}, nil
}

// replySpec is the message that carries a REPLY: to the organizer, from
// the account, with a short text part and the REPLY as its calendar
// alternative. Only the organizer's address is used: names in an event
// are text whoever sent it chose.
func replySpec(acct config.ResolvedAccount, t rsvpTarget, ps calendar.PartStat, comment string, reply []byte, now time.Time) compose.Spec {
	verb := map[calendar.PartStat]string{
		calendar.PartStatAccepted: "Accepted", calendar.PartStatDeclined: "Declined", calendar.PartStatTentative: "Tentative",
	}[ps]
	subject := verb
	if sum := truncateRunes(render.CleanHeader(t.ev.Summary), maxReplySubjectRunes); sum != "" {
		subject += ": " + sum
	}
	who := acct.DisplayName
	if who == "" {
		who = acct.Address
	}
	body := who + " answered the invitation: " + strings.ToLower(verb) + "."
	if comment != "" {
		body += "\n\n" + comment
	}
	return compose.Spec{
		From:      mail.Address{Name: acct.DisplayName, Address: acct.Address},
		To:        []mail.Address{{Address: t.organizer}},
		Subject:   subject,
		Body:      compose.Body{Kind: compose.Text, Source: body},
		Date:      now.Local(),
		UserAgent: "chainmail/" + version.Version,
		Calendar:  &compose.CalendarAlternative{Method: calendar.MethodReply, Data: reply},
	}
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// mintKey is a fresh idempotency key for a call whose client gave none.
func mintKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return "daemon-" + hex.EncodeToString(b)
}
