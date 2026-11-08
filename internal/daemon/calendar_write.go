package daemon

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/idempotency"
	"github.com/yaklabco/chainmail/internal/mailaddr"
)

const (
	// maxTitleRunes bounds an event's title, maxLocationRunes its location,
	// maxDescriptionBytes its description and maxRuleBytes its RRULE.
	maxTitleRunes       = 1024
	maxLocationRunes    = 1024
	maxDescriptionBytes = 64 << 10
	maxRuleBytes        = 1024
	// createHeldHint is CalendarHeld.hint for a held create.
	createHeldHint = "held by policy: the event was created as a draft and no invitations were sent; James can send them from the calendar app"
)

var (
	errCreateAccount  = &argError{msg: "name the account to create the event on", hint: "name the account: chainmail calendars lists them"}
	errNoDefault      = &argError{msg: "the account has no default calendar", hint: "name a calendar; " + calendarsHint}
	errEventTitle     = &argError{msg: "title must be 1 to 1024 characters", hint: "pass a title"}
	errEventLocation  = &argError{msg: "location must be at most 1024 characters", hint: "shorten the location"}
	errEventDesc      = &argError{msg: "description must be at most 64 KiB", hint: "shorten the description"}
	errEventTimes     = &argError{msg: "start and end must be set, with end after start", hint: "pass start and end; an all-day event ends at the midnight after its last day"}
	errEventZone      = &argError{msg: "time_zone must be an IANA zone name", hint: "such as Europe/London; leave it empty for the [calendar] timezone"}
	errEventRule      = &argError{msg: "the recurrence rule is not one chainmail can expand", hint: "pass an RRULE value such as FREQ=WEEKLY;BYDAY=MO, at most hourly"}
	errEventAttendees = &argError{msg: "an event has at most 500 attendees", hint: "invite fewer people"}
)

// CreateEvent implements chainmailv1connect.CalendarServiceHandler. It
// creates the event in the account's default calendar or the one named,
// with UID derived from the idempotency key, so a retry finds the event a
// lost create made (Amendment 14). Attendees other than James are checked
// against the send policy: a held create makes a draft event that notifies
// no one.
func (h *calendarService) CreateEvent(ctx context.Context, req *connect.Request[chainmailv1.CreateEventRequest]) (*connect.Response[chainmailv1.CreateEventResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.create_event", calendarWriteTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.CreateEventResponse, error) {
		m := req.Msg
		ca, err := s.createAccount(m.GetAccount())
		entry.Account = cmp.Or(ca.name, invalidTarget)
		if err != nil {
			return nil, err
		}
		key := m.GetIdempotencyKey()
		switch {
		case key == "":
			return nil, errKeyRequired
		case !idempotency.ValidKey(key):
			return nil, errCalendarKey
		}
		acct, err := s.opts.Config.Account(ca.name)
		if err != nil {
			return nil, err
		}
		ev, err := s.newEvent(m, acct)
		if err != nil {
			return nil, err
		}
		entry.Targets = append(entry.Targets, tally("attendees", len(ev.Attendees)))
		if err := s.calendarsReady(ctx, []*calAccount{ca}); err != nil {
			return nil, err
		}
		cal, err := s.writableCalendar(ctx, ca.name, m.GetCalendarId())
		if err != nil {
			return nil, err
		}
		entry.Targets = append([]string{"calendar=" + plain(cal.ID)}, entry.Targets...)
		canon := proto.Clone(m).(*chainmailv1.CreateEventRequest)
		canon.Account, canon.CalendarId, canon.Attendees = ca.name, cal.ID, canonicalAddresses(m.GetAttendees())
		hash, err := calendarHash(canon)
		if err != nil {
			return nil, err
		}
		claimKey := calendarKey("cal", key)
		claim, err := s.claimCalendar(ctx, ca.name, claimKey, hash)
		if err != nil {
			return nil, err
		}
		if claim.result != nil {
			entry.Decision = decisionDuplicate
			out := &chainmailv1.CreateEventResponse{}
			if err := proto.Unmarshal(claim.result, out); err != nil {
				return nil, err
			}
			entry.Targets = append(entry.Targets, out.GetEvent().GetRef())
			return out, nil
		}
		out, err := s.createEvent(ctx, ca, cal.ID, ev, entry)
		if claim.proceed {
			s.settleCalendar(ctx, ca.name, claimKey, out, err)
		}
		return out, err
	})
}

// createAccount resolves a create's account. With none named, the one
// account with a calendar store is used, and anything else is refused.
func (s *Server) createAccount(name string) (*calAccount, error) {
	if name == "" {
		var only *calAccount
		for _, n := range s.opts.Config.AccountNames() {
			if ca := s.calendars[n]; ca != nil && ca.store != nil {
				if only != nil {
					return &calAccount{}, errCreateAccount
				}
				only = ca
			}
		}
		if only == nil {
			return &calAccount{}, errCreateAccount
		}
		return only, nil
	}
	if _, err := accountTarget(name); err != nil {
		return &calAccount{}, err
	}
	ca, err := s.calendarAccount(name)
	if err != nil {
		return &calAccount{name: name}, err
	}
	if ca.store == nil {
		return ca, errNoCalendarStore
	}
	return ca, nil
}

// writableCalendar returns the account's calendar id, or its default when
// id is empty. A calendar the account may only read is CALENDAR_READ_ONLY.
func (s *Server) writableCalendar(ctx context.Context, account, id string) (calcache.CalendarRow, error) {
	var (
		cal calcache.CalendarRow
		err error
	)
	if id == "" {
		cals, err := s.calcache.Calendars(ctx, account)
		if err != nil {
			return calcache.CalendarRow{}, err
		}
		i := slices.IndexFunc(cals, func(c calcache.CalendarRow) bool { return c.IsDefault })
		if i < 0 {
			return calcache.CalendarRow{}, errNoDefault
		}
		cal = cals[i]
	} else if cal, err = s.calcache.Calendar(ctx, account, id); err != nil {
		return calcache.CalendarRow{}, err
	}
	if cal.Access != calendar.AccessWrite {
		return calcache.CalendarRow{}, calstore.ErrReadOnly
	}
	return cal, nil
}

// newEvent checks a create request and builds its event: James as the
// organizer, and as an accepted attendee when he invites others, with the
// UID derived from the account and key.
func (s *Server) newEvent(m *chainmailv1.CreateEventRequest, acct config.ResolvedAccount) (calendar.Event, error) {
	ev := calendar.Event{
		UID:       derivedUID(acct.Name, m.GetIdempotencyKey()),
		Status:    calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: acct.Address, Name: acct.DisplayName},
	}
	var err error
	if ev.Summary, err = checkTitle(m.GetTitle()); err != nil {
		return calendar.Event{}, err
	}
	if ev.Location, ev.Description, err = checkText(m.GetLocation(), m.GetDescription()); err != nil {
		return calendar.Event{}, err
	}
	loc, zone, err := s.eventZone(m.GetTimeZone())
	if err != nil {
		return calendar.Event{}, err
	}
	if m.GetStart() == nil || m.GetEnd() == nil {
		return calendar.Event{}, errEventTimes
	}
	ev.AllDay = m.GetAllDay()
	ev.Start, ev.End = eventTime(m.GetStart(), ev.AllDay, loc), eventTime(m.GetEnd(), ev.AllDay, loc)
	if !ev.AllDay {
		ev.TZID = zone
	}
	if !ev.End.After(ev.Start) {
		return calendar.Event{}, errEventTimes
	}
	if ev.RRule, err = checkRule(m.GetRecurrenceRule(), ev); err != nil {
		return calendar.Event{}, err
	}
	people, err := parseAttendees(m.GetAttendees())
	if err != nil {
		return calendar.Event{}, err
	}
	ev.Attendees = s.mergeAttendees(nil, people)
	if hasOtherAttendees(ev, s.ownAddress) && !slices.ContainsFunc(ev.Attendees, func(a calendar.Attendee) bool { return strings.EqualFold(a.Email, acct.Address) }) {
		self := calendar.Attendee{Person: *ev.Organizer, PartStat: calendar.PartStatAccepted, Role: "REQ-PARTICIPANT"}
		ev.Attendees = append([]calendar.Attendee{self}, ev.Attendees...)
	}
	return ev, nil
}

// derivedUID is the UID of an event created with key on account
// (Amendment 14).
func derivedUID(account, key string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + key))
	return hex.EncodeToString(sum[:]) + "@chainmail"
}

// createEvent creates ev in calID, notifying its attendees when the policy
// allows them all, else as a held draft event that notifies no one. A
// create that finds the object already there (a retry after a lost
// answer) reads it back and returns it.
func (s *Server) createEvent(ctx context.Context, ca *calAccount, calID string, ev calendar.Event, entry *audit.Entry) (*chainmailv1.CreateEventResponse, error) {
	notified := s.notified(ev.Attendees)
	d := s.policy.CheckSend(ca.name, notified)
	mode := calendar.NotifySend
	if len(notified) > 0 && !d.Allowed {
		mode = calendar.NotifyNone
	}
	created, err := ca.store.Put(ctx, calID, ev, "", mode)
	if errors.Is(err, calstore.ErrExists) {
		created, err = s.existingEvent(ctx, ca, calID, ev.UID)
	}
	if err != nil {
		return nil, calendarError(ca.name, err)
	}
	s.syncAfterWrite(ctx, ca)
	ref := calendar.EventRef(ca.name, calID, created.ProviderID)
	entry.Targets = append(entry.Targets, ref)
	v := eventView{
		ref: ref, account: ca.name, calendarID: calID, ev: created, myPartStat: selfPartStat(created, s.ownAddress),
		isOrganizer: true, rrule: created.RRule, recurring: isSeries(created),
	}
	if row, err := s.calcache.Event(ctx, ref); err == nil {
		v = viewOfRow(row)
	}
	out := &chainmailv1.CreateEventResponse{Event: eventProto(v, s.ownAddress)}
	if mode == calendar.NotifyNone {
		heldTargets(entry, len(d.Blocked), d.TooMany)
		out.Held = &chainmailv1.CalendarHeld{Hint: createHeldHint, Blocked: untrustedList(d.Blocked), TooMany: d.TooMany, EventRef: ref}
		return out, nil
	}
	entry.Decision = decisionAllowed
	return out, nil
}

// objectNamer is a calendar store that can say which object a create of a
// UID makes. Every backend is one.
type objectNamer interface {
	ObjectID(calID, uid string) string
}

// existingEvent reads back the event a create of uid made: from the store
// by the object a create of uid names, or else from the cache after a sync.
func (s *Server) existingEvent(ctx context.Context, ca *calAccount, calID, uid string) (calendar.Event, error) {
	if n, ok := ca.store.(objectNamer); ok {
		if id := n.ObjectID(calID, uid); id != "" {
			evs, err := ca.store.GetEvent(ctx, calID, id)
			if err != nil {
				return calendar.Event{}, err
			}
			for _, e := range evs {
				if e.RecurrenceID.IsZero() {
					return e, nil
				}
			}
			return calendar.Event{}, calstore.ErrNotFound
		}
	}
	s.syncAfterWrite(ctx, ca)
	rows, err := s.calcache.ByUID(ctx, ca.name, uid)
	if err != nil {
		return calendar.Event{}, err
	}
	for _, r := range rows {
		if r.CalendarID == calID && r.Event.RecurrenceID.IsZero() {
			return r.Event, nil
		}
	}
	return calendar.Event{}, calstore.ErrExists
}

// notified is who a write to an event with attendees notifies: every
// attendee but James.
func (s *Server) notified(attendees []calendar.Attendee) []string {
	var out []string
	for _, a := range attendees {
		if !s.ownAddress(a.Email) && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, a.Email) }) {
			out = append(out, a.Email)
		}
	}
	return out
}

// checkTitle checks a title: 1 to 1024 characters that are not all white space.
func checkTitle(t string) (string, error) {
	if strings.TrimSpace(t) == "" || utf8.RuneCountInString(t) > maxTitleRunes || !utf8.ValidString(t) {
		return "", errEventTitle
	}
	return t, nil
}

// checkText checks a location and a description.
func checkText(location, description string) (string, string, error) {
	if utf8.RuneCountInString(location) > maxLocationRunes || !utf8.ValidString(location) {
		return "", "", errEventLocation
	}
	if len(description) > maxDescriptionBytes || !utf8.ValidString(description) {
		return "", "", errEventDesc
	}
	return location, description, nil
}

// eventZone returns the zone a request's times are read in, and the TZID
// a timed event gets: the request's IANA zone, else [calendar] timezone.
// UTC and the host's unnamed zone give TZID "".
func (s *Server) eventZone(tz string) (*time.Location, string, error) {
	if tz == "" {
		loc := s.calSettings.Location
		if loc == nil {
			loc = time.UTC
		}
		return loc, ianaTZID(loc.String()), nil
	}
	if tz == "Local" || zoneName(tz) == "" {
		return nil, "", errEventZone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, "", errEventZone
	}
	return loc, ianaTZID(tz), nil
}

// ianaTZID is the TZID of a zone name: "" for UTC and the host's zone.
func ianaTZID(name string) string {
	switch name {
	case "UTC", "Etc/UTC", "Local", "":
		return ""
	}
	return name
}

// eventTime reads a request time. A timed value is the instant, in UTC.
// An all-day value is 00:00 UTC of its date (Amendment 17): one already at
// 00:00 UTC is taken as it is, and any other is read as a midnight in loc,
// the request's zone, as MCP sends it.
func eventTime(ts *timestamppb.Timestamp, allDay bool, loc *time.Location) time.Time {
	t := ts.AsTime().UTC()
	if !allDay || t.Equal(t.Truncate(24*time.Hour)) {
		return t
	}
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// checkRule checks an RRULE value (an "RRULE:" prefix is dropped) by
// expanding ev with it once.
func checkRule(rule string, ev calendar.Event) (string, error) {
	rule = strings.TrimSpace(rule)
	if len(rule) >= 6 && strings.EqualFold(rule[:6], "RRULE:") {
		rule = rule[6:]
	}
	if rule == "" {
		return "", nil
	}
	if len(rule) > maxRuleBytes || strings.ContainsFunc(rule, isControl) {
		return "", errEventRule
	}
	ev.RRule, ev.RecurrenceID = rule, time.Time{}
	if _, _, err := calendar.Expand([]calendar.Event{ev}, ev.Start, ev.End.Add(time.Second), 1); err != nil {
		return "", errEventRule
	}
	return rule, nil
}

// parseAttendees reads attendee addresses, without repeats. The error
// names the position, never the value.
func parseAttendees(list []string) ([]calendar.Person, error) {
	out := make([]calendar.Person, 0, len(list))
	for i, s := range list {
		a, err := mail.ParseAddress(s)
		if err != nil || !mailaddr.Safe(a.Address) {
			return nil, &argError{msg: fmt.Sprintf("attendee %d is not an email address", i+1), hint: `write each attendee as "a@example.com" or "Name <a@example.com>"`}
		}
		if slices.ContainsFunc(out, func(p calendar.Person) bool { return strings.EqualFold(p.Email, a.Address) }) {
			continue
		}
		out = append(out, calendar.Person{Email: a.Address, Name: a.Name})
	}
	if len(out) > calendar.MaxAttendees {
		return nil, errEventAttendees
	}
	return out, nil
}

// mergeAttendees is the attendee list people asks for: an attendee who
// was already on old keeps his entry (PARTSTAT, role), a new one needs to
// answer. James's own entries on old are kept even when people leaves them
// out, so an organizer's own acceptance survives.
func (s *Server) mergeAttendees(old []calendar.Attendee, people []calendar.Person) []calendar.Attendee {
	var out []calendar.Attendee
	for _, a := range old {
		if s.ownAddress(a.Email) && !slices.ContainsFunc(people, func(p calendar.Person) bool { return strings.EqualFold(p.Email, a.Email) }) {
			out = append(out, a)
		}
	}
	for _, p := range people {
		if i := slices.IndexFunc(old, func(a calendar.Attendee) bool { return strings.EqualFold(a.Email, p.Email) }); i >= 0 {
			out = append(out, old[i])
			continue
		}
		ps := calendar.PartStatNeedsAction
		if s.ownAddress(p.Email) {
			ps = calendar.PartStatAccepted
		}
		out = append(out, calendar.Attendee{Person: p, PartStat: ps, Role: "REQ-PARTICIPANT", RSVP: !s.ownAddress(p.Email)})
	}
	return out
}

// canonicalAddresses is an attendee list in the form a request is hashed
// in: trimmed, without repeats that differ only in case, sorted.
func canonicalAddresses(list []string) []string {
	var out []string
	for _, a := range list {
		a = strings.TrimSpace(a)
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, a) }) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return out
}
