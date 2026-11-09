package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailaddr"
	"github.com/yaklabco/chainmail/internal/watch"
)

const (
	// inviteRetryInterval is how often the retry loop announces the
	// invites whose event could not be published.
	inviteRetryInterval = 5 * time.Minute
	// inviteAnnounceBatch is how many pending invites of an account one
	// announcing pass reads at most.
	inviteAnnounceBatch = 100
)

// inviteHookOptions are what the invite hook works with. Only Cache is
// required; every other nil field does nothing.
type inviteHookOptions struct {
	// Cache is the calendar cache the hook stores mail invites in.
	Cache *calcache.Store
	// Log receives the invite events of store none accounts.
	Log *watch.Log
	// Publish appends one event to the log. Nil means Log.Publish. Tests
	// set it to fail a publish.
	Publish func(ctx context.Context, e watch.Event) error
	// Poke asks the account's calendar syncer for a cycle soon, without
	// blocking.
	Poke func(account string)
	// StoreNone reports whether the account's calendar store is none, so
	// invites reach it only through mail and the hook announces them.
	StoreNone func(account string) bool
	// Self reports whether a bare address is one of James's own.
	Self func(addr string) bool
	// Now is the hook's clock. Nil means time.Now.
	Now func() time.Time
	// ReadPart reads a calendar part the cache builder deferred, once, as
	// background work. Nil means queued parts are not read.
	ReadPart func(ctx context.Context, c cache.InviteCandidate) (cache.FoundInvite, error)
	// Logger receives counts and reason codes, never invite text. Nil
	// discards.
	Logger *slog.Logger
}

// inviteHook takes the invites the cache builder finds in new mail
// (cache.AccountOptions.OnInvites): it parses each, stores it as a mail
// invite row in calendar.db, pokes the account's calendar syncer, and on a
// store none account publishes an invite event for each new REQUEST UID.
// An event that cannot be published stays pending in calendar.db (the row
// is not announced) and is tried again on the account's next hook call and
// by the retry loop.
type inviteHook struct {
	o inviteHookOptions
	// mu serializes announcing, so the hook calls of several cache workers
	// and the retry loop never publish one invite twice.
	mu sync.Mutex
	// backlogMu guards backlog: deferred parts whose queue write failed,
	// kept in memory for the retry loop to queue, at most
	// inviteBacklogCap of them.
	backlogMu sync.Mutex
	backlog   []calcache.InviteFetchRow
}

// newInviteHook returns the invite hook over o, or nil when o has no
// calendar cache to store invites in.
func newInviteHook(o inviteHookOptions) *inviteHook {
	if o.Cache == nil {
		return nil
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Publish == nil && o.Log != nil {
		log := o.Log
		o.Publish = func(ctx context.Context, e watch.Event) error { return log.Publish(ctx, e) }
	}
	return &inviteHook{o: o}
}

// hookFunc is h's OnInvites, nil when h is.
func (h *inviteHook) hookFunc() func(context.Context, []cache.FoundInvite) {
	if h == nil {
		return nil
	}
	return h.onInvites
}

// calendarInviteHook returns the OnInvites hook of the cache workers over
// the calendar cache cal and the syncer poke poke, or nil when cal is nil,
// which leaves invite detection off. Store none comes from the account's
// resolved calendar table, own addresses from the policy, and events go to
// the daemon's log. Every call over the same cache shares one hook with the
// retry loop.
func (s *Server) calendarInviteHook(cal *calcache.Store, poke func(account string)) func(context.Context, []cache.FoundInvite) {
	return s.serverInviteHook(cal, poke).hookFunc()
}

// serverInviteHook returns the server's invite hook over cal, built on
// first use, or nil when cal is nil.
func (s *Server) serverInviteHook(cal *calcache.Store, poke func(account string)) *inviteHook {
	if cal == nil {
		return nil
	}
	s.invitesMu.Lock()
	defer s.invitesMu.Unlock()
	if s.invites == nil || s.invites.o.Cache != cal {
		s.invites = newInviteHook(inviteHookOptions{
			Cache:     cal,
			Log:       s.watchLog,
			Poke:      poke,
			StoreNone: s.calendarStoreNone,
			Self:      s.policy.IsOwn,
			Now:       s.opts.Now,
			Logger:    s.opts.Logger,
			ReadPart:  s.readInvitePart,
		})
	}
	return s.invites
}

// startInviteRetries runs the invite retry loop on wg until ctx ends, when
// calendar.db is open and some account has a cache worker, whose deferred
// invite parts the loop reads, or a store of none, whose pending invites it
// announces. startWatch has recorded the cache workers by then.
func (s *Server) startInviteRetries(ctx context.Context, wg *sync.WaitGroup) {
	h := s.serverInviteHook(s.calcache, s.pokeCalendar)
	if h == nil {
		return
	}
	var accounts []string
	for _, name := range s.opts.Config.AccountNames() {
		if s.calendarStoreNone(name) {
			accounts = append(accounts, name)
		}
	}
	if len(accounts) == 0 && len(s.cacheAccounts) == 0 {
		return
	}
	wg.Go(func() { h.retryLoop(ctx, s.calClock, accounts) })
}

// calendarStoreNone reports whether account's calendar store is none. An
// unknown account reports false.
func (s *Server) calendarStoreNone(account string) bool {
	acct, err := s.opts.Config.Account(account)
	return err == nil && acct.Calendar.Store == config.CalendarStoreNone
}

// onInvites handles one batch of found invites, in order. An invite that
// does not parse, or whose row cannot be read or written, is logged by
// reason code and skipped; the rest of the batch goes on. Then each
// account of the batch announces its pending invites, and each account
// with an invite stored is poked once.
//
// The row is written before its event is published, and marked announced
// only once the publish succeeds, so a failed publish or a crash between
// the two leaves the event pending, to go out on a later hook call or
// retry pass (at least once, as the syncer's events are).
func (h *inviteHook) onInvites(ctx context.Context, invites []cache.FoundInvite) {
	var accounts, poke []string
	for _, f := range invites {
		if !slices.Contains(accounts, f.Account) {
			accounts = append(accounts, f.Account)
		}
		if h.handle(ctx, f) == inviteStored && !slices.Contains(poke, f.Account) {
			poke = append(poke, f.Account)
		}
	}
	h.finish(ctx, accounts, poke)
}

// finish announces the pending invites of each of accounts and pokes each
// of poke, once invites of theirs were handled.
func (h *inviteHook) finish(ctx context.Context, accounts, poke []string) {
	for _, account := range accounts {
		if ctx.Err() != nil {
			return
		}
		h.announce(ctx, account)
	}
	if h.o.Poke == nil {
		return
	}
	for _, account := range poke {
		h.o.Poke(account)
	}
}

// retryLoop runs a pass at once, then every inviteRetryInterval on clock,
// until ctx ends: each reads the queued invite parts that are due and
// announces the pending invites of accounts.
func (h *inviteHook) retryLoop(ctx context.Context, clock watch.Clock, accounts []string) {
	for {
		h.pass(ctx, accounts)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.After(inviteRetryInterval):
		}
	}
}

// inviteOutcome is what handle did with a found invite.
type inviteOutcome int

const (
	// inviteStored: the invite's row is in calendar.db.
	inviteStored inviteOutcome = iota
	// inviteUnusable: the part is no usable invite, and never will be.
	inviteUnusable
	// inviteNotStored: calendar.db could not store the row.
	inviteNotStored
)

// handle parses and stores one found invite, and reports what it did.
//
// The row holds the invite's UTF-8 text with no charset when that fits in
// calendar.MaxICSBytes, and otherwise the part's own bytes with its
// charset: a single-byte part can grow past the limit in UTF-8, and
// calendar.ParseInvite refuses data over it. Either way the row reads back
// with calendar.ParseInvite(Raw, Charset).
//
// The row is found at f.FoundAt, when the cache found the message (the
// hook's clock when that is unset), so a part the builder deferred and the
// retry loop read later keeps its place in its UID's history, and a
// segment applied again keeps the earliest time (PutMailInvite).
func (h *inviteHook) handle(ctx context.Context, f cache.FoundInvite) inviteOutcome {
	log := h.o.Logger.With("account", f.Account)
	inv, err := calendar.ParseInvite(f.Data, f.Part.Charset)
	if err != nil {
		log.InfoContext(ctx, "skipped a calendar part in mail that is not a usable invite", "reason", inviteReason(err))
		return inviteUnusable
	}
	ref := calendar.MailInviteRef(f.Account, inv.UID)
	raw, charset := f.Data, f.Part.Charset
	if len(inv.Events) > 0 && inv.Events[0].Raw != nil && len(inv.Events[0].Raw) <= calendar.MaxICSBytes {
		raw, charset = inv.Events[0].Raw, ""
	}
	foundAt := f.FoundAt
	if foundAt.IsZero() {
		foundAt = h.o.Now()
	}
	row := calcache.MailInviteRow{
		Ref: ref, Account: f.Account, MessageRef: f.MessageRef, UID: inv.UID, Method: inv.Method,
		Sequence: inv.Sequence, DTStamp: inv.DTStamp, Raw: raw, Charset: charset, FoundAt: foundAt,
	}
	if err := h.o.Cache.PutMailInvite(ctx, row); err != nil {
		log.WarnContext(ctx, "could not store a mail invite", "ref", ref)
		return inviteNotStored
	}
	return inviteStored
}

// announce publishes the invite event of each of account's pending
// invites that is due one, and marks it announced once published. It does
// nothing unless the account's store is none and the hook has a log. A
// publish that fails leaves the invite pending for the next call, for as
// long as the invite stays actionable: an invite that is not (it is
// cancelled, James organizes it, every event of it has ended, or its data
// does not parse) is settled, so no later pass reads it again. A UID whose
// latest row is a CANCEL is never pending. One call reads at most
// inviteAnnounceBatch invites, those first found longest ago first, and
// stops between invites once ctx ends.
func (h *inviteHook) announce(ctx context.Context, account string) {
	if h.o.Publish == nil || h.o.StoreNone == nil || !h.o.StoreNone(account) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	log := h.o.Logger.With("account", account)
	now := h.o.Now()
	rows, err := h.o.Cache.PendingInvites(ctx, account, inviteAnnounceBatch)
	if err != nil {
		if ctx.Err() == nil {
			log.WarnContext(ctx, "could not read the pending mail invites")
		}
		return
	}
	failed := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		inv, err := calendar.ParseInvite(row.Raw, row.Charset)
		if err != nil {
			log.WarnContext(ctx, "a stored mail invite does not parse", "ref", row.Ref, "reason", inviteReason(err))
			h.settle(ctx, log, row)
			continue
		}
		if !h.due(inv, now) {
			h.settle(ctx, log, row)
			continue
		}
		if err := h.publish(ctx, row, inv, now); err != nil {
			failed++
			continue
		}
		if err := h.o.Cache.MarkInviteAnnounced(ctx, row.Ref); err != nil {
			log.WarnContext(ctx, "could not mark a mail invite announced", "ref", row.Ref)
		}
	}
	if failed > 0 {
		log.WarnContext(ctx, "could not publish invite events; they stay pending", "count", failed)
	}
}

// settle marks a pending invite row that will never get an event, so
// PendingInvites leaves it out from now on. A failure is logged; the row
// is then looked at again on the next pass.
func (h *inviteHook) settle(ctx context.Context, log *slog.Logger, row calcache.MailInviteRow) {
	if err := h.o.Cache.MarkInviteSettled(ctx, row.Account, row.MessageRef, row.UID); err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "could not settle a mail invite", "ref", row.Ref)
	}
}

// due reports whether an invite gets an invite event: the method is
// REQUEST, the event is not cancelled, James does not organize it, and not
// every event of it has ended at now (a recurring one never has).
func (h *inviteHook) due(inv calendar.MailInvite, now time.Time) bool {
	if inv.Method != calendar.MethodRequest {
		return false
	}
	ev := inviteEvent(inv)
	if ev.Status == calendar.StatusCancelled {
		return false
	}
	if ev.Organizer != nil && h.o.Self != nil && h.o.Self(ev.Organizer.Email) {
		return false
	}
	return !inviteEnded(inv, now)
}

// inviteEnded reports whether every event of inv has ended at now. A
// recurring event never has.
func inviteEnded(inv calendar.MailInvite, now time.Time) bool {
	for _, ev := range inv.Events {
		if ev.RRule != "" || len(ev.RDates) > 0 || inviteEnd(ev).After(now) {
			return false
		}
	}
	return true
}

// publish appends the invite event of a pending mail invite.
func (h *inviteHook) publish(ctx context.Context, row calcache.MailInviteRow, inv calendar.MailInvite, now time.Time) error {
	ev := inviteEvent(inv)
	change := &watch.CalendarChange{
		Ref:        row.Ref,
		MessageRef: row.MessageRef,
		Start:      calendarTime(ev.Start),
		End:        calendarTime(inviteEnd(ev)),
	}
	if ev.Organizer != nil && mailaddr.Safe(ev.Organizer.Email) {
		change.Organizer = ev.Organizer.Email
	}
	e := watch.Event{At: now, Kind: watch.KindInvite, Account: row.Account, Ref: row.Ref, Calendar: change}
	return h.o.Publish(ctx, e)
}

// inviteEvent is the VEVENT an invite is about: its series master or
// single event, else its first override.
func inviteEvent(inv calendar.MailInvite) calendar.Event {
	for _, ev := range inv.Events {
		if ev.RecurrenceID.IsZero() {
			return ev
		}
	}
	if len(inv.Events) > 0 {
		return inv.Events[0]
	}
	return calendar.Event{}
}

// inviteEnd is ev's end, or its start when it has none.
func inviteEnd(ev calendar.Event) time.Time {
	if ev.End.IsZero() {
		return ev.Start
	}
	return ev.End
}

// calendarTime renders t as an RFC 3339 UTC string for a calendar event
// payload, "" for the zero time.
func calendarTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// inviteReason is the log reason of a calendar part that is not a usable
// invite.
func inviteReason(err error) string {
	switch {
	case errors.Is(err, calendar.ErrNotInvite):
		return "not_invite"
	case errors.Is(err, calendar.ErrTooLarge):
		return "too_large"
	default:
		return "invalid"
	}
}
