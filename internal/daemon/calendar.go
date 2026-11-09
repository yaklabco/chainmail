package daemon

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/idempotency"
)

const (
	// calendarReadTimeout is the deadline of a CalendarService read whose
	// client set none. Reads come from calendar.db, except free/busy of
	// other people, which asks the provider.
	calendarReadTimeout = callTimeout
	// calendarWriteTimeout is the deadline of a CalendarService write: a
	// store call, a sync and, for an RSVP by email, a whole send.
	calendarWriteTimeout = composeTimeout
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

	errNotAttendee = &calendarRefusal{
		code: connect.CodeFailedPrecondition, reason: api.ReasonInvalidArgument,
		msg:  "none of the account's addresses is an attendee of the event",
		hint: "only an invite to one of the account's addresses can be answered",
	}
	errNoOrganizer = &calendarRefusal{
		code: connect.CodeFailedPrecondition, reason: api.ReasonInvalidArgument,
		msg:  "the event has no organizer to answer",
		hint: "answer it in the calendar app",
	}
	errOwnEvent = &calendarRefusal{
		code: connect.CodeFailedPrecondition, reason: api.ReasonInvalidArgument,
		msg:  "James organizes this event, so there is no invite to answer",
		hint: "change it with: chainmail event edit <ref>",
	}
	errCalendarKeyBusy = &calendarRefusal{
		code: connect.CodeUnavailable, reason: api.ReasonServerUnavailable,
		msg:  "a call with this idempotency key is still in progress",
		hint: "try again later with the same key",
	}
	errCalendarKeyFailed = &calendarRefusal{
		code: connect.CodeUnavailable, reason: api.ReasonServerUnavailable,
		msg:  "an earlier call with this idempotency key failed",
		hint: "try again with the same key",
	}

	errCalendarRange = &argError{msg: "from must be before to, and at most 400 days before it", hint: "leave from and to unset for the next 7 days"}
	errCalendarKey   = &argError{msg: "the idempotency key is malformed", hint: "pass idempotency_key: 1 to 128 of A-Z, a-z, 0-9 and . _ : -"}
	errCalendarReuse = &argError{msg: "key already used for a different request", hint: "use a new idempotency key for a different request"}
)

// calendarService implements chainmailv1connect.CalendarServiceHandler.
// Every call is audited as calendar.<rpc>, with accounts, calendar IDs,
// event refs, the decision and counts as targets. No error, hint, audit
// target or log line holds event text: summaries, locations, descriptions,
// names, comments or calendar names. Event text comes back wrapped as
// Untrusted.
type calendarService struct {
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
	// Every name is checked before any is looked up, so the target never
	// holds an entry that is not a valid account name.
	for _, name := range names {
		if _, err := accountTarget(name); err != nil {
			return nil, "accounts=" + invalidTarget, err
		}
	}
	target := "accounts=" + strings.Join(names, ",")
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		ca, err := s.calendarAccount(name)
		if err != nil {
			return nil, target, err
		}
		if storeOnly && ca.store == nil {
			return nil, target, errNoCalendarStore
		}
		out = append(out, ca)
	}
	order := s.opts.Config.AccountNames()
	slices.SortFunc(out, func(a, b *calAccount) int {
		return cmp.Compare(slices.Index(order, a.name), slices.Index(order, b.name))
	})
	return out, target, nil
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

// A sparse override (one that names no organizer, or lists no attendee)
// takes those from its series master. organizer, attendees and role give
// the effective values, which every authorization and policy check uses.

// organizer is the event's effective organizer: the row's, or the
// master's for an override that names none. Nil when neither names one.
func (r resolvedEvent) organizer() *calendar.Person {
	if o := r.ev.Organizer; o != nil && o.Email != "" {
		return o
	}
	if r.master != nil {
		if o := r.master.Event.Organizer; o != nil && o.Email != "" {
			return o
		}
	}
	return nil
}

// attendees is the event's effective attendee list and whether it is
// incomplete: the row's, or the master's for an override that lists none.
func (r resolvedEvent) attendees() ([]calendar.Attendee, bool) {
	if len(r.ev.Attendees) > 0 || r.master == nil {
		return r.ev.Attendees, r.ev.AttendeesTruncated
	}
	return r.master.Event.Attendees, r.master.Event.AttendeesTruncated
}

// role is the event's effective MyPartStat and IsOrganizer: each the
// row's, or the master's when the row is an override without the field
// it comes from.
func (r resolvedEvent) role() (calendar.PartStat, bool) {
	ps, org := r.row.MyPartStat, r.row.IsOrganizer
	if r.master != nil {
		if o := r.row.Event.Organizer; o == nil || o.Email == "" {
			org = r.master.IsOrganizer
		}
		if len(r.row.Event.Attendees) == 0 {
			ps = r.master.MyPartStat
		}
	}
	return ps, org
}

// isSeries reports whether ev is a series master: a row with no recurrence
// ID and an RRULE or RDATEs, as calcache counts one.
func isSeries(ev calendar.Event) bool {
	return ev.RecurrenceID.IsZero() && (ev.RRule != "" || len(ev.RDates) > 0)
}

// resolveEvent finds the event ref names in calendar.db: a stored row, or
// an instance of a stored series with no override of its own. The event's
// account must have finished its first sync, else the result is
// CACHE_NOT_READY (or AUTH_REQUIRED), as the cache may hold only part of
// it. A ref nothing names is calcache.ErrNotFound; callers that look no
// further pass it through refNotFound.
func (s *Server) resolveEvent(ctx context.Context, ref string) (resolvedEvent, error) {
	res, err := s.lookupEvent(ctx, ref)
	if err != nil {
		return resolvedEvent{}, err
	}
	if err := s.accountReady(ctx, res.row.Account); err != nil {
		return resolvedEvent{}, err
	}
	return res, nil
}

// accountReady is calendarsReady for the one account.
func (s *Server) accountReady(ctx context.Context, account string) error {
	ca := s.calendars[account]
	if ca == nil {
		return nil
	}
	return s.calendarsReady(ctx, []*calAccount{ca})
}

// refNotFound is the error for a ref that names nothing in calendar.db:
// while an account with a calendar store has not finished its first sync,
// the ref may name one of its events still to come, so the result is that
// account's CACHE_NOT_READY (or AUTH_REQUIRED). Any other err is kept.
func (s *Server) refNotFound(ctx context.Context, err error) error {
	if !errors.Is(err, calcache.ErrNotFound) {
		return err
	}
	cas, _, serr := s.calendarScope(nil, true)
	if serr != nil {
		return serr
	}
	if rerr := s.calendarsReady(ctx, cas); rerr != nil {
		return rerr
	}
	return err
}

// lookupEvent is resolveEvent without the readiness check.
func (s *Server) lookupEvent(ctx context.Context, ref string) (resolvedEvent, error) {
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

// syncAfterWrite runs a sync cycle of ca so calendar.db holds what a write
// changed. A failure is logged by its reason: the write itself is done, so
// the result is built from what the store returned.
func (s *Server) syncAfterWrite(ctx context.Context, ca *calAccount) {
	if ca.syncer == nil {
		return
	}
	if err := ca.syncer.SyncNow(ctx); err != nil && ctx.Err() == nil {
		s.opts.Logger.Warn("syncing the calendar after a write failed", "account", ca.name, "reason", apiReason(calendarError(ca.name, err)))
	}
}

// calendarKey is the idempotency key a calendar call claims for a
// caller's key: ns, a colon and 40 hex digits of the key's SHA-256. It fits
// the 128 bytes a key may have whatever the caller's key, and never meets
// a send's key or another kind of calendar call's.
func calendarKey(ns, key string) string {
	sum := sha256.Sum256([]byte(key))
	return ns + ":" + hex.EncodeToString(sum[:20])
}

// calendarHash is the hash of a calendar request with its idempotency key
// cleared, so a repeat with the same key is recognized.
func calendarHash(m proto.Message) ([32]byte, error) {
	m = proto.Clone(m)
	r := m.ProtoReflect()
	if fd := r.Descriptor().Fields().ByName("idempotency_key"); fd != nil {
		r.Clear(fd)
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encoding the request: %w", err)
	}
	return sha256.Sum256(b), nil
}

// calendarClaim is the outcome of claiming a calendar call's key.
type calendarClaim struct {
	// proceed means the caller holds the key: it does the work and then
	// calls settleCalendar.
	proceed bool
	// result is the stored response of a call that finished, to replay.
	result []byte
	// unknown means an earlier daemon stopped during the call. The caller
	// does the work again without settling: the stores make a repeated
	// create or answer harmless (Amendment 14).
	unknown bool
}

// claimCalendar claims (account, key) for hash. A key used with another
// request is INVALID_ARGUMENT; one another process holds, or whose attempt
// this caller waited on failed, is refused with fixed text.
func (s *Server) claimCalendar(ctx context.Context, account, key string, hash [32]byte) (calendarClaim, error) {
	claim, err := s.idem.Begin(ctx, account, key, hash)
	switch {
	case errors.Is(err, idempotency.ErrKeyMismatch):
		return calendarClaim{}, errCalendarReuse
	case err != nil:
		return calendarClaim{}, err
	case claim.Proceed:
		return calendarClaim{proceed: true}, nil
	case claim.State == idempotency.Done:
		return calendarClaim{result: claim.Result}, nil
	case claim.State == idempotency.Unknown:
		return calendarClaim{unknown: true}, nil
	case claim.State == idempotency.InProgress:
		return calendarClaim{}, errCalendarKeyBusy
	default:
		return calendarClaim{}, errCalendarKeyFailed
	}
}

// settleCalendar settles a claimed key: done with the response to replay,
// or retryable after a failure, so a repeat tries again. A client hang-up
// must not leave the key in progress, so it ignores ctx's cancellation.
func (s *Server) settleCalendar(ctx context.Context, account, key string, resp proto.Message, failed error) {
	state, b := idempotency.Done, []byte(nil)
	if failed != nil {
		state = idempotency.Retryable
	} else {
		var err error
		if b, err = proto.Marshal(resp); err != nil {
			s.opts.Logger.Error("encoding a calendar result", "account", account, "err", err)
			state = idempotency.Retryable
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := s.idem.Finish(ctx, account, key, state, b); err != nil {
		s.opts.Logger.Error("settling an idempotency key", "account", account, "state", string(state), "err", err)
	}
}

// heldTargets adds a policy decision's counts to an audit entry and marks
// it held: blocked:<n> and too_many, never an address.
func heldTargets(entry *audit.Entry, blocked int, tooMany bool) {
	entry.Decision, entry.Reason = decisionHeld, api.ReasonHeld
	if blocked > 0 {
		entry.Targets = append(entry.Targets, fmt.Sprintf("blocked:%d", blocked))
	}
	if tooMany {
		entry.Targets = append(entry.Targets, "too_many")
	}
}
