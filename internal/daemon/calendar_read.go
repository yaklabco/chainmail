package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/mailaddr"
)

// Invite sources.
const (
	inviteSourceCalendar = "calendar"
	inviteSourceMail     = "mail"
)

// maxFreeBusyPeople is the most people one FreeBusy asks a provider about.
const maxFreeBusyPeople = 100

var (
	errFreeBusyAccount = &argError{msg: "name the account whose provider is asked about people", hint: "pass account; " + calendarsHint}
	errFreeBusyPeople  = &argError{msg: "at most 100 people can be asked about at once", hint: "ask in smaller groups"}
)

// ListCalendars implements chainmailv1connect.CalendarServiceHandler.
func (h *calendarService) ListCalendars(ctx context.Context, req *connect.Request[chainmailv1.ListCalendarsRequest]) (*connect.Response[chainmailv1.ListCalendarsResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.list_calendars", calendarReadTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.ListCalendarsResponse, error) {
		cas, target, err := s.calendarScope(req.Msg.GetAccounts(), true)
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		if err := s.calendarsReady(ctx, cas); err != nil {
			return nil, err
		}
		out := &chainmailv1.ListCalendarsResponse{}
		for _, ca := range cas {
			rows, err := s.calcache.Calendars(ctx, ca.name)
			if err != nil {
				return nil, err
			}
			// The default first, then the primary, then by ID.
			for _, r := range rows {
				out.Calendars = append(out.Calendars, calendarInfoProto(r))
			}
		}
		entry.Targets = append(entry.Targets, tally("calendars", len(out.Calendars)))
		return out, nil
	})
}

// Agenda implements chainmailv1connect.CalendarServiceHandler. It expands
// the cached series within the cache window and the request's range.
func (h *calendarService) Agenda(ctx context.Context, req *connect.Request[chainmailv1.AgendaRequest]) (*connect.Response[chainmailv1.AgendaResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.agenda", calendarReadTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.AgendaResponse, error) {
		m := req.Msg
		cas, target, err := s.calendarScope(m.GetAccounts(), true)
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		from, to, err := calendarRange(optTime(m.GetFrom()), optTime(m.GetTo()), s.opts.Now())
		if err != nil {
			return nil, err
		}
		if err := s.calendarsReady(ctx, cas); err != nil {
			return nil, err
		}
		out := &chainmailv1.AgendaResponse{Events: []*chainmailv1.CalendarEvent{}}
		from, to, ok := s.calendarWindow(from, to)
		if !ok {
			return out, nil
		}
		insts, truncated, err := s.expandRange(ctx, accountNames(cas), m.GetCalendars(), from, to)
		if err != nil {
			return nil, err
		}
		for _, in := range insts {
			if in.ev.Status == calendar.StatusCancelled || (in.myPartStat == calendar.PartStatDeclined && !m.GetIncludeDeclined()) {
				continue
			}
			out.Events = append(out.Events, eventProto(in, s.ownAddress))
		}
		out.Truncated = truncated
		entry.Targets = append(entry.Targets, tally("events", len(out.Events)))
		return out, nil
	})
}

// optTime is a request time, zero when unset.
func optTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func accountNames(cas []*calAccount) []string {
	out := make([]string, len(cas))
	for i, ca := range cas {
		out[i] = ca.name
	}
	return out
}

// expandRange expands every cached series and single of accounts (and
// calIDs, empty meaning all) that overlaps [from, to), sorted by start
// and then ref. truncated is set when a series had more instances than
// calendar.MaxInstances in the range, or a rule was not expanded.
func (s *Server) expandRange(ctx context.Context, accounts, calIDs []string, from, to time.Time) ([]eventView, bool, error) {
	rows, err := s.calcache.Range(ctx, accounts, calIDs, from, to)
	if err != nil {
		return nil, false, err
	}
	var (
		out       []eventView
		truncated bool
	)
	for _, group := range seriesGroups(rows) {
		insts, trunc, err := expandGroup(group, from, to, calendar.MaxInstances)
		if err != nil {
			// The rule is unsafe or broken: its instances are left out.
			s.opts.Logger.Info("a calendar series was not expanded", "account", group[0].Account, "ref", calendar.EventRef(group[0].Account, group[0].CalendarID, group[0].Event.ProviderID), "reason", expandReason(err))
			truncated = true
			continue
		}
		truncated = truncated || trunc
		out = append(out, insts...)
	}
	slices.SortStableFunc(out, func(a, b eventView) int {
		return cmp.Or(a.ev.Start.Compare(b.ev.Start), cmp.Compare(a.ref, b.ref))
	})
	return out, truncated, nil
}

// expandReason is a fixed code for an expansion failure, for logs.
func expandReason(err error) string {
	if errors.Is(err, calendar.ErrUnsafeRecurrence) {
		return "unsafe_recurrence"
	}
	return "invalid"
}

// seriesGroups splits rows, which calcache returns by account, calendar,
// ProviderID and recurrence ID, into one group per object.
func seriesGroups(rows []calcache.EventRow) [][]calcache.EventRow {
	var out [][]calcache.EventRow
	for i := 0; i < len(rows); {
		j := i + 1
		for j < len(rows) && rows[j].Account == rows[i].Account && rows[j].CalendarID == rows[i].CalendarID && rows[j].Event.ProviderID == rows[i].Event.ProviderID {
			j++
		}
		out = append(out, rows[i:j])
		i = j
	}
	return out
}

// expandGroup expands the rows of one object over [from, to), at most limit
// instances, each with the row it comes from and James's role in it.
func expandGroup(group []calcache.EventRow, from, to time.Time, limit int) ([]eventView, bool, error) {
	evs := make([]calendar.Event, len(group))
	var master *calcache.EventRow
	byRID := make(map[time.Time]calcache.EventRow, len(group))
	for i, r := range group {
		evs[i] = r.Event
		byRID[r.Event.RecurrenceID] = r
		if r.Event.RecurrenceID.IsZero() {
			master = &group[i]
		}
	}
	insts, truncated, err := calendar.Expand(evs, from, to, limit)
	if err != nil {
		return nil, false, err
	}
	first := group[0]
	base := calendar.EventRef(first.Account, first.CalendarID, first.Event.ProviderID)
	out := make([]eventView, 0, len(insts))
	for _, in := range insts {
		row, ok := byRID[in.Event.RecurrenceID]
		if !ok {
			continue
		}
		res := resolvedEvent{row: row, ref: base, rid: in.RecurrenceID}
		if !row.Event.RecurrenceID.IsZero() {
			res.master = master
		}
		if !in.RecurrenceID.IsZero() {
			res.ref = calendar.InstanceRef(base, in.RecurrenceID)
		}
		res.ev = in.Event
		res.ev.Start, res.ev.End, res.ev.RecurrenceID = in.Start, in.End, in.RecurrenceID
		out = append(out, viewOfResolved(res))
	}
	return out, truncated, nil
}

// GetEvent implements chainmailv1connect.CalendarServiceHandler. A ref of
// an invite known only from mail returns that invite's event.
func (h *calendarService) GetEvent(ctx context.Context, req *connect.Request[chainmailv1.GetEventRequest]) (*connect.Response[chainmailv1.GetEventResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.get_event", calendarReadTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.GetEventResponse, error) {
		target, _, err := eventRefTarget(req.Msg.GetRef())
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		res, err := s.resolveEvent(ctx, req.Msg.GetRef())
		if errors.Is(err, calcache.ErrNotFound) {
			inv, ierr := s.mailInvite(ctx, req.Msg.GetRef())
			if ierr == nil {
				entry.Account = inv.GetEvent().GetAccount()
				return &chainmailv1.GetEventResponse{Event: inv.GetEvent()}, nil
			}
		}
		if err != nil {
			return nil, err
		}
		entry.Account = res.row.Account
		v := viewOfResolved(res)
		v.messageRef = s.inviteMessageRef(ctx, res.row.Account, res.row.Event.UID)
		return &chainmailv1.GetEventResponse{Event: eventProto(v, s.ownAddress)}, nil
	})
}

// ListInvites implements chainmailv1connect.CalendarServiceHandler. Each
// invite appears once: when a calendar event and a mail invite share a
// UID, the calendar event is listed with the mail message's ref.
func (h *calendarService) ListInvites(ctx context.Context, req *connect.Request[chainmailv1.ListInvitesRequest]) (*connect.Response[chainmailv1.ListInvitesResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.list_invites", calendarReadTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.ListInvitesResponse, error) {
		cas, target, err := s.calendarScope(req.Msg.GetAccounts(), false)
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		if err := s.calendarsReady(ctx, cas); err != nil {
			return nil, err
		}
		out := &chainmailv1.ListInvitesResponse{Invites: []*chainmailv1.Invite{}}
		for _, ca := range cas {
			invs, err := s.accountInvites(ctx, ca)
			if err != nil {
				return nil, err
			}
			out.Invites = append(out.Invites, invs...)
		}
		slices.SortStableFunc(out.Invites, func(a, b *chainmailv1.Invite) int {
			return cmp.Or(a.GetEvent().GetStart().AsTime().Compare(b.GetEvent().GetStart().AsTime()), cmp.Compare(a.GetEvent().GetRef(), b.GetEvent().GetRef()))
		})
		entry.Targets = append(entry.Targets, tally("invites", len(out.Invites)))
		return out, nil
	})
}

// accountInvites lists one account's invites: calendar events waiting for
// James's reply, then invites known only from mail.
func (s *Server) accountInvites(ctx context.Context, ca *calAccount) ([]*chainmailv1.Invite, error) {
	mailRows, err := s.calcache.MailInvites(ctx, ca.name)
	if err != nil {
		return nil, err
	}
	messageRefs := map[string]string{}
	for _, r := range mailRows {
		if r.Method != calendar.MethodCancel {
			messageRefs[r.UID] = r.MessageRef
		}
	}
	var out []*chainmailv1.Invite
	if ca.store != nil {
		invs, err := s.calendarInvites(ctx, ca.name, messageRefs)
		if err != nil {
			return nil, err
		}
		out = append(out, invs...)
	}
	for _, r := range mailRows {
		if r.Method == calendar.MethodCancel {
			continue
		}
		known, err := s.calcache.ByUID(ctx, ca.name, r.UID)
		if err != nil {
			return nil, err
		}
		if len(known) > 0 {
			continue
		}
		if inv, ok := s.mailInviteOf(r); ok {
			out = append(out, inv)
		}
	}
	return out, nil
}

// calendarInvites lists the account's calendar events where James is an
// attendee who has not answered, that he does not organize, that are not
// cancelled and that end after now. A series waiting for a reply is one
// invite; an instance waiting on its own is another.
func (s *Server) calendarInvites(ctx context.Context, account string, messageRefs map[string]string) ([]*chainmailv1.Invite, error) {
	now := s.opts.Now()
	end := now.Add(s.calSettings.WindowFuture)
	rows, err := s.calcache.Range(ctx, []string{account}, nil, now, end)
	if err != nil {
		return nil, err
	}
	var out []*chainmailv1.Invite
	for _, group := range seriesGroups(rows) {
		var master *calcache.EventRow
		for i, r := range group {
			if r.Event.RecurrenceID.IsZero() {
				master = &group[i]
			}
		}
		masterWaits := false
		if master != nil && waitsForReply(*master) && s.stillAhead(group, *master, now, end) {
			masterWaits = true
			out = append(out, s.calendarInvite(resolvedEvent{ref: master.Ref, row: *master, ev: master.Event}, messageRefs))
		}
		for _, r := range group {
			if r.Event.RecurrenceID.IsZero() || masterWaits || !waitsForReply(r) || !r.Event.End.After(now) {
				continue
			}
			out = append(out, s.calendarInvite(resolvedEvent{ref: r.Ref, row: r, ev: r.Event, master: master, rid: r.Event.RecurrenceID}, messageRefs))
		}
	}
	return out, nil
}

// waitsForReply reports whether James is asked for a reply on a row.
func waitsForReply(r calcache.EventRow) bool {
	return r.MyPartStat == calendar.PartStatNeedsAction && !r.IsOrganizer && r.Event.Status != calendar.StatusCancelled
}

// stillAhead reports whether the event of row ends after now: a single
// event by its end, a series by an instance in [now, end).
func (s *Server) stillAhead(group []calcache.EventRow, row calcache.EventRow, now, end time.Time) bool {
	if !isSeries(row.Event) {
		return row.Event.End.After(now)
	}
	insts, _, err := expandGroup(group, now, end, 1)
	return err == nil && len(insts) > 0
}

// calendarInvite is the Invite of a calendar event.
func (s *Server) calendarInvite(res resolvedEvent, messageRefs map[string]string) *chainmailv1.Invite {
	v := viewOfResolved(res)
	v.messageRef = messageRefs[res.row.Event.UID]
	return &chainmailv1.Invite{Event: eventProto(v, s.ownAddress), Source: inviteSourceCalendar, Method: calendar.MethodRequest}
}

// mailInviteOf is the Invite of a mail invite row, and false when it is not
// one to list: it does not parse, James organizes it, it is cancelled, or
// it is over.
func (s *Server) mailInviteOf(r calcache.MailInviteRow) (*chainmailv1.Invite, bool) {
	ev, evs, ok := s.parseMailInvite(r)
	if !ok || ev.Status == calendar.StatusCancelled {
		return nil, false
	}
	if ev.Organizer != nil && s.ownAddress(ev.Organizer.Email) {
		return nil, false
	}
	now := s.opts.Now()
	if isSeries(ev) {
		insts, _, err := calendar.Expand(evs, now, now.Add(s.calSettings.WindowFuture), 1)
		if err != nil || len(insts) == 0 {
			return nil, false
		}
	} else if !ev.End.After(now) {
		return nil, false
	}
	return s.mailInviteProto(r, ev), true
}

// parseMailInvite reads a stored mail invite: its main event (the master,
// or the first event) and every event. A row that no longer parses is
// logged by its ref and skipped.
func (s *Server) parseMailInvite(r calcache.MailInviteRow) (calendar.Event, []calendar.Event, bool) {
	inv, err := calendar.ParseInvite(r.Raw, "")
	if err == nil && len(inv.Events) == 0 {
		err = calendar.ErrNotInvite
	}
	if err != nil {
		s.opts.Logger.Info("a stored mail invite does not parse", "account", r.Account, "ref", r.Ref, "reason", expandReason(err))
		return calendar.Event{}, nil, false
	}
	ev := inv.Events[0]
	for _, e := range inv.Events {
		if e.RecurrenceID.IsZero() {
			ev = e
			break
		}
	}
	return ev, inv.Events, true
}

// mailInviteProto is the Invite of a mail invite's event.
func (s *Server) mailInviteProto(r calcache.MailInviteRow, ev calendar.Event) *chainmailv1.Invite {
	v := eventView{
		ref: r.Ref, account: r.Account, ev: ev, myPartStat: selfPartStat(ev, s.ownAddress),
		isOrganizer: ev.Organizer != nil && s.ownAddress(ev.Organizer.Email),
		rrule:       ev.RRule, recurring: isSeries(ev), messageRef: r.MessageRef,
	}
	return &chainmailv1.Invite{Event: eventProto(v, s.ownAddress), Source: inviteSourceMail, Method: knownToken(r.Method, inviteMethods)}
}

// selfPartStat is the PARTSTAT of James's first attendee entry, "" when he
// is not an attendee.
func selfPartStat(ev calendar.Event, self func(string) bool) calendar.PartStat {
	for _, a := range ev.Attendees {
		if self(a.Email) {
			return cmp.Or(a.PartStat, calendar.PartStatNeedsAction)
		}
	}
	return ""
}

// mailInvite returns the invite known only from mail with ref.
func (s *Server) mailInvite(ctx context.Context, ref string) (*chainmailv1.Invite, error) {
	if s.calcache == nil {
		return nil, errCalendarNotReady
	}
	r, err := s.calcache.MailInvite(ctx, ref)
	if err != nil {
		return nil, err
	}
	ev, _, ok := s.parseMailInvite(r)
	if !ok {
		return nil, calcache.ErrNotFound
	}
	return s.mailInviteProto(r, ev), nil
}

// inviteMessageRef is the message ref of the account's latest mail invite
// with uid, "" when there is none or it was withdrawn.
func (s *Server) inviteMessageRef(ctx context.Context, account, uid string) string {
	if uid == "" {
		return ""
	}
	r, err := s.calcache.MailInvite(ctx, calendar.MailInviteRef(account, uid))
	if err != nil || r.Method == calendar.MethodCancel {
		return ""
	}
	return r.MessageRef
}

// GetInvite implements chainmailv1connect.CalendarServiceHandler. A
// calendar ref names an event James is invited to; a mail ref names an
// invite found in mail, shown as the account's calendar event when it has
// one with the same UID.
func (h *calendarService) GetInvite(ctx context.Context, req *connect.Request[chainmailv1.GetInviteRequest]) (*connect.Response[chainmailv1.GetInviteResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.get_invite", calendarReadTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.GetInviteResponse, error) {
		target, _, err := eventRefTarget(req.Msg.GetRef())
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		inv, err := s.getInvite(ctx, req.Msg.GetRef())
		if err != nil {
			return nil, err
		}
		entry.Account = inv.GetEvent().GetAccount()
		return &chainmailv1.GetInviteResponse{Invite: inv}, nil
	})
}

func (s *Server) getInvite(ctx context.Context, ref string) (*chainmailv1.Invite, error) {
	res, err := s.resolveEvent(ctx, ref)
	if err == nil {
		if ps, org := res.role(); ps == "" || org {
			return nil, calcache.ErrNotFound
		}
		return s.calendarInvite(res, map[string]string{res.row.Event.UID: s.inviteMessageRef(ctx, res.row.Account, res.row.Event.UID)}), nil
	}
	if !errors.Is(err, calcache.ErrNotFound) {
		return nil, err
	}
	r, err := s.calcache.MailInvite(ctx, ref)
	if err != nil {
		return nil, err
	}
	if res, ok := s.calendarEventOfUID(ctx, r.Account, r.UID); ok {
		return s.calendarInvite(res, map[string]string{r.UID: r.MessageRef}), nil
	}
	ev, _, ok := s.parseMailInvite(r)
	if !ok {
		return nil, calcache.ErrNotFound
	}
	return s.mailInviteProto(r, ev), nil
}

// calendarEventOfUID returns the account's stored series master or single
// event with uid, when its calendar has one.
func (s *Server) calendarEventOfUID(ctx context.Context, account, uid string) (resolvedEvent, bool) {
	ca := s.calendars[account]
	if ca == nil || ca.store == nil {
		return resolvedEvent{}, false
	}
	rows, err := s.calcache.ByUID(ctx, account, uid)
	if err != nil {
		return resolvedEvent{}, false
	}
	for _, r := range rows {
		if r.Event.RecurrenceID.IsZero() {
			return resolvedEvent{ref: r.Ref, row: r, ev: r.Event}, true
		}
	}
	return resolvedEvent{}, false
}

// FreeBusy implements chainmailv1connect.CalendarServiceHandler. James's
// busy time comes from the cache of every account with a store; other
// people's from the named account's provider.
func (h *calendarService) FreeBusy(ctx context.Context, req *connect.Request[chainmailv1.FreeBusyRequest]) (*connect.Response[chainmailv1.FreeBusyResponse], error) {
	s := h.s
	return calendarCall(ctx, s, req, "calendar.free_busy", calendarReadTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.FreeBusyResponse, error) {
		m := req.Msg
		entry.Targets = []string{tally("people", len(m.GetPeople()))}
		from, to, err := calendarRange(optTime(m.GetFrom()), optTime(m.GetTo()), s.opts.Now())
		if err != nil {
			return nil, err
		}
		var asked *calAccount
		if len(m.GetPeople()) > 0 || m.GetAccount() != "" {
			name, err := accountTarget(m.GetAccount())
			entry.Account = name
			if m.GetAccount() == "" {
				return nil, errFreeBusyAccount
			}
			if err != nil {
				return nil, err
			}
			if asked, err = s.calendarAccount(name); err != nil {
				return nil, err
			}
			if len(m.GetPeople()) > 0 && asked.store == nil {
				return nil, errNoCalendarStore
			}
		}
		if err := checkPeople(m.GetPeople()); err != nil {
			return nil, err
		}
		cas, _, err := s.calendarScope(nil, true)
		if err != nil {
			return nil, err
		}
		if err := s.calendarsReady(ctx, cas); err != nil {
			return nil, err
		}
		out := &chainmailv1.FreeBusyResponse{SelfBusy: []*chainmailv1.BusyBlock{}, People: []*chainmailv1.PersonFreeBusy{}}
		if wf, wt, ok := s.calendarWindow(from, to); ok {
			insts, _, err := s.expandRange(ctx, accountNames(cas), nil, wf, wt)
			if err != nil {
				return nil, err
			}
			var blocks []calendar.BusyBlock
			for _, in := range insts {
				if in.ev.Status == calendar.StatusCancelled || in.ev.Transparent || in.myPartStat == calendar.PartStatDeclined {
					continue
				}
				blocks = append(blocks, calendar.BusyBlock{Start: maxTime(in.ev.Start, from), End: minTime(in.ev.End, to)})
			}
			out.SelfBusy = busyProto(calendar.MergeBusy(blocks))
		}
		if len(m.GetPeople()) > 0 {
			fb, err := asked.store.FreeBusy(ctx, m.GetPeople(), from, to)
			if err != nil {
				return nil, calendarError(asked.name, err)
			}
			for _, p := range m.GetPeople() {
				got := fb[p]
				out.People = append(out.People, &chainmailv1.PersonFreeBusy{Person: p, Known: got.Known, Busy: busyProto(got.Busy)})
			}
		}
		return out, nil
	})
}

// checkPeople refuses a free/busy list that is too long or holds something
// that is not a bare address. The error names the position, never the value.
func checkPeople(people []string) error {
	if len(people) > maxFreeBusyPeople {
		return errFreeBusyPeople
	}
	for i, p := range people {
		a, err := mail.ParseAddress(p)
		if err != nil || a.Address != p || !mailaddr.Safe(p) {
			return &argError{msg: fmt.Sprintf("person %d is not an email address", i+1), hint: "pass bare addresses, such as a@example.com"}
		}
	}
	return nil
}
