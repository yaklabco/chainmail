package daemon

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

const (
	// calendarReadTimeout is the deadline of a CalendarService read whose
	// client set none. Reads come from calendar.db, except free/busy of
	// other people, which asks the provider.
	calendarReadTimeout = callTimeout
	// maxCalendarSpan is the longest range Agenda and FreeBusy read.
	maxCalendarSpan = 400 * 24 * time.Hour
	// defaultCalendarSpan is the range Agenda and FreeBusy read from now
	// when the request names no end.
	defaultCalendarSpan = 7 * 24 * time.Hour
	// calendarsHint points at the list of calendars and accounts.
	calendarsHint = "chainmail calendars lists the accounts and calendars"
)

// calendarRefusal is a CalendarService refusal with its own code and
// reason. Its text and hint are fixed: none repeats a value from the
// request or from an event.
type calendarRefusal struct {
	code   connect.Code
	reason string
	msg    string
	hint   string
}

func (e *calendarRefusal) Error() string { return e.msg }

// The service's own refusals.
var (
	errNoCalendarStore = &calendarRefusal{
		code: connect.CodeFailedPrecondition, reason: api.ReasonNoCalendarStore,
		msg:  "the account has no calendar store",
		hint: "set store in the account's [calendar] table, or name another account; " + calendarsHint,
	}

	errCalendarRange = &argError{msg: "from must be before to, and at most 400 days before it", hint: "leave from and to unset for the next 7 days"}
)

// calendarService implements chainmailv1connect.CalendarServiceHandler.
// Every call is audited as calendar.<rpc>, with accounts, calendar IDs,
// event refs, the decision and counts as targets. No error, hint, audit
// target or log line holds event text: summaries, locations, descriptions,
// names, comments or calendar names. Event text comes back wrapped as
// Untrusted.
type calendarService struct {
	chainmailv1connect.UnimplementedCalendarServiceHandler
	s *Server
}

var _ chainmailv1connect.CalendarServiceHandler = (*calendarService)(nil)

// calendarCall runs one CalendarService call as auditedCall does, and
// keeps an error that is already a Connect error, such as one from the
// send pipeline an RSVP by email uses, with its own reason.
func calendarCall[Req, Resp any](ctx context.Context, s *Server, req *connect.Request[Req], action string, timeout time.Duration,
	run func(ctx context.Context, c caller, entry *audit.Entry) (*Resp, error),
) (*connect.Response[Resp], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, timeout)
	defer cancel()
	entry := audit.Entry{Action: action}
	resp, runErr := run(ctx, c, &entry)
	if cerr, ok := errors.AsType[*connect.Error](runErr); ok {
		entry.Result, entry.Reason = audit.ResultError, api.Detail(cerr).GetReason()
		if err := s.record(ctx, c, entry, nil); err != nil {
			return nil, err
		}
		return nil, cerr
	}
	if err := s.record(ctx, c, entry, runErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// calendarScope resolves the accounts a call covers, in config order
// without repeats: the named ones, or with none every account (storeOnly
// false) or every account with a calendar store (storeOnly true). A named
// account without a store is NO_CALENDAR_STORE when storeOnly is set. The
// audit target lists the names.
func (s *Server) calendarScope(names []string, storeOnly bool) ([]*calAccount, string, error) {
	var out []*calAccount
	if len(names) == 0 {
		for _, name := range s.opts.Config.AccountNames() {
			if ca := s.calendars[name]; ca != nil && (!storeOnly || ca.store != nil) {
				out = append(out, ca)
			}
		}
		return out, "accounts=*", nil
	}
	seen := map[string]bool{}
	for _, name := range names {
		if _, err := accountTarget(name); err != nil {
			return nil, "accounts=" + invalidTarget, err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		ca, err := s.calendarAccount(name)
		if err != nil {
			return nil, "accounts=" + strings.Join(names, ","), err
		}
		if storeOnly && ca.store == nil {
			return nil, "accounts=" + strings.Join(names, ","), errNoCalendarStore
		}
		out = append(out, ca)
	}
	order := s.opts.Config.AccountNames()
	slices.SortFunc(out, func(a, b *calAccount) int {
		return cmp.Compare(slices.Index(order, a.name), slices.Index(order, b.name))
	})
	return out, "accounts=" + strings.Join(names, ","), nil
}

// calendarsReady returns nil when calendar.db can answer reads of every
// account of cas that has a store, else the first account's refusal:
// AUTH_REQUIRED naming it when its calendar server refused its
// credentials, else CACHE_NOT_READY. Accounts without a store keep only
// mail invites, which need calendar.db open.
func (s *Server) calendarsReady(ctx context.Context, cas []*calAccount) error {
	if s.calcache == nil {
		return errCalendarNotReady
	}
	for _, ca := range cas {
		if ca.store == nil {
			continue
		}
		if err := s.calendarReady(ctx, ca.name); err != nil {
			if st := ca.syncer.Status(); errors.Is(st.LastErr, calstore.ErrAuth) {
				return calendarError(ca.name, st.LastErr)
			}
			return err
		}
	}
	return nil
}

// calendarRange checks a request's range: from defaults to now, to to
// from plus 7 days, and they may be at most 400 days apart.
func calendarRange(from, to, now time.Time) (time.Time, time.Time, error) {
	if from.IsZero() {
		from = now
	}
	if to.IsZero() {
		to = from.Add(defaultCalendarSpan)
	}
	if !from.Before(to) || to.Sub(from) > maxCalendarSpan {
		return time.Time{}, time.Time{}, errCalendarRange
	}
	return from.UTC(), to.UTC(), nil
}

// calendarWindow returns [from, to) cut to the cache window around now:
// window_past before it and window_future after it. Empty when they do not
// overlap.
func (s *Server) calendarWindow(from, to time.Time) (time.Time, time.Time, bool) {
	now := s.opts.Now()
	from = maxTime(from, now.Add(-s.calSettings.WindowPast))
	to = minTime(to, now.Add(s.calSettings.WindowFuture))
	return from, to, from.Before(to)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// eventRefTarget is the audit target of a ref: the ref when it parses,
// else invalidTarget, with the parse error. The error never repeats the ref.
func eventRefTarget(ref string) (string, time.Time, error) {
	_, rid, err := calendar.ParseEventRef(ref)
	if err != nil {
		return invalidTarget, time.Time{}, err
	}
	return ref, rid, nil
}

// resolvedEvent is what a ref names in calendar.db.
type resolvedEvent struct {
	// ref is the ref as asked for.
	ref string
	// row is the stored row the event comes from: a single event, a series
	// master, an override, or the master of an instance with no override.
	row calcache.EventRow
	// master is the series master of an instance or override, when stored.
	master *calcache.EventRow
	// ev is the event the ref names: row.Event, or for an instance with no
	// override the master moved to the instance's times.
	ev calendar.Event
	// rid is the instance's recurrence ID, zero for a single event or a
	// whole series.
	rid time.Time
}

// recurring reports whether the event is a series or one of its instances.
func (r resolvedEvent) recurring() bool {
	return !r.rid.IsZero() || isSeries(r.row.Event)
}

// role is the event's MyPartStat and IsOrganizer: the row's, or the
// master's for an override that names no organizer and no attendee, as a
// sparse override takes its series' role.
func (r resolvedEvent) role() (calendar.PartStat, bool) {
	if r.master != nil && r.row.Event.Organizer == nil && len(r.row.Event.Attendees) == 0 {
		return r.master.MyPartStat, r.master.IsOrganizer
	}
	return r.row.MyPartStat, r.row.IsOrganizer
}

// isSeries reports whether ev is a series master: a row with no recurrence
// ID and an RRULE or RDATEs, as calcache counts one.
func isSeries(ev calendar.Event) bool {
	return ev.RecurrenceID.IsZero() && (ev.RRule != "" || len(ev.RDates) > 0)
}

// resolveEvent finds the event ref names in calendar.db: a stored row, or
// an instance of a stored series with no override of its own.
func (s *Server) resolveEvent(ctx context.Context, ref string) (resolvedEvent, error) {
	base, rid, err := calendar.ParseEventRef(ref)
	if err != nil {
		return resolvedEvent{}, err
	}
	if s.calcache == nil {
		return resolvedEvent{}, errCalendarNotReady
	}
	out := resolvedEvent{ref: ref, rid: rid}
	row, err := s.calcache.Event(ctx, ref)
	switch {
	case err == nil:
		out.row, out.ev = row, row.Event
		if !rid.IsZero() {
			if m, err := s.calcache.Event(ctx, base); err == nil {
				out.master = &m
			}
		}
		return out, nil
	case !errors.Is(err, calcache.ErrNotFound) || rid.IsZero():
		return resolvedEvent{}, err
	}
	master, err := s.calcache.Event(ctx, base)
	if err != nil {
		return resolvedEvent{}, err
	}
	if !isSeries(master.Event) {
		return resolvedEvent{}, calcache.ErrNotFound
	}
	series, err := s.calcache.Series(ctx, master.Account, master.CalendarID, master.Event.ProviderID)
	if err != nil {
		return resolvedEvent{}, err
	}
	evs := make([]calendar.Event, len(series))
	for i, r := range series {
		evs[i] = r.Event
	}
	insts, _, err := calendar.Expand(evs, rid, rid.Add(time.Second), calendar.MaxInstances)
	if err != nil {
		return resolvedEvent{}, calcache.ErrNotFound
	}
	for _, in := range insts {
		if in.RecurrenceID.Equal(rid) && in.Event.RecurrenceID.IsZero() {
			ev := master.Event
			ev.RecurrenceID, ev.Start, ev.End = rid, in.Start, in.End
			out.row, out.master, out.ev = master, &master, ev
			return out, nil
		}
	}
	return resolvedEvent{}, calcache.ErrNotFound
}

// ownAddress reports whether addr is one of James's own addresses, the
// set the policy always allows.
func (s *Server) ownAddress(addr string) bool {
	return addr != "" && s.policy.IsOwn(strings.TrimSpace(addr))
}
