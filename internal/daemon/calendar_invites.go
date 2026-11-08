package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailaddr"
	"github.com/yaklabco/chainmail/internal/watch"
)

// inviteHookOptions are what the invite hook works with. Only Cache is
// required; every other nil field does nothing.
type inviteHookOptions struct {
	// Cache is the calendar cache the hook stores mail invites in.
	Cache *calcache.Store
	// Log receives the invite events of store none accounts.
	Log *watch.Log
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
	// Logger receives counts and reason codes, never invite text. Nil
	// discards.
	Logger *slog.Logger
}

// inviteHook takes the invites the cache builder finds in new mail
// (cache.AccountOptions.OnInvites): it parses each, stores it as a mail
// invite row in calendar.db, pokes the account's calendar syncer, and on a
// store none account publishes an invite event for each new REQUEST UID.
type inviteHook struct {
	o inviteHookOptions
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
	return &inviteHook{o: o}
}

// calendarInviteHook returns the OnInvites hook of the cache workers over
// the calendar cache cal and the syncer poke poke, or nil when cal is nil,
// which leaves invite detection off. Store none comes from the account's
// resolved calendar table, own addresses from the policy, and events go to
// the daemon's log.
func (s *Server) calendarInviteHook(cal *calcache.Store, poke func(account string)) func(context.Context, []cache.FoundInvite) {
	if cal == nil {
		return nil
	}
	h := newInviteHook(inviteHookOptions{
		Cache:     cal,
		Log:       s.watchLog,
		Poke:      poke,
		StoreNone: s.calendarStoreNone,
		Self:      s.policy.IsOwn,
		Now:       s.opts.Now,
		Logger:    s.opts.Logger,
	})
	if h == nil {
		return nil
	}
	return h.onInvites
}

// calendarStoreNone reports whether account's calendar store is none. An
// unknown account reports false.
func (s *Server) calendarStoreNone(account string) bool {
	acct, err := s.opts.Config.Account(account)
	return err == nil && acct.Calendar.Store == config.CalendarStoreNone
}

// onInvites handles one batch of found invites, in order. An invite that
// does not parse, or whose row cannot be read or written, is logged by
// reason code and skipped; the rest of the batch goes on. Each account
// with an invite stored is poked once, after the batch.
//
// A store none account's REQUEST whose UID has no stored row yet, that
// James does not organize and that has not ended, is published as an
// invite event before its row is written, so a crash between the two
// repeats the event when the builder applies the message again (at least
// once, as the syncer's events are).
func (h *inviteHook) onInvites(ctx context.Context, invites []cache.FoundInvite) {
	var poke []string
	for _, f := range invites {
		if h.handle(ctx, f) && !slices.Contains(poke, f.Account) {
			poke = append(poke, f.Account)
		}
	}
	if h.o.Poke == nil {
		return
	}
	for _, account := range poke {
		h.o.Poke(account)
	}
}

// handle parses and stores one found invite, publishing its event when it
// is due one. It reports whether the row was stored.
func (h *inviteHook) handle(ctx context.Context, f cache.FoundInvite) bool {
	log := h.o.Logger.With("account", f.Account)
	inv, err := calendar.ParseInvite(f.Data, f.Part.Charset)
	if err != nil {
		log.InfoContext(ctx, "skipped a calendar part in mail that is not a usable invite", "reason", inviteReason(err))
		return false
	}
	ref := calendar.MailInviteRef(f.Account, inv.UID)
	_, err = h.o.Cache.MailInvite(ctx, ref)
	isNew := errors.Is(err, calcache.ErrNotFound)
	if err != nil && !isNew {
		log.WarnContext(ctx, "could not read the calendar cache for a mail invite", "ref", ref)
		return false
	}
	if isNew && h.announces(f.Account, inv) {
		h.publish(ctx, log, f, ref, inv)
	}
	raw := f.Data
	if len(inv.Events) > 0 && inv.Events[0].Raw != nil {
		// The parsed text, in UTF-8 whatever the part's charset, so a
		// reader parses the row with no charset.
		raw = inv.Events[0].Raw
	}
	row := calcache.MailInviteRow{
		Ref: ref, Account: f.Account, MessageRef: f.MessageRef, UID: inv.UID, Method: inv.Method,
		Sequence: inv.Sequence, DTStamp: inv.DTStamp, Raw: raw, FoundAt: h.o.Now(),
	}
	if err := h.o.Cache.PutMailInvite(ctx, row); err != nil {
		log.WarnContext(ctx, "could not store a mail invite", "ref", ref)
		return false
	}
	return true
}

// announces reports whether a new invite on account gets an invite event:
// the account's store is none, the method is REQUEST, the event is not
// cancelled, James does not organize it, and it has not ended (a
// recurring one never has).
func (h *inviteHook) announces(account string, inv calendar.MailInvite) bool {
	if h.o.Log == nil || h.o.StoreNone == nil || !h.o.StoreNone(account) || inv.Method != calendar.MethodRequest {
		return false
	}
	ev := inviteEvent(inv)
	if ev.Status == calendar.StatusCancelled {
		return false
	}
	if ev.Organizer != nil && h.o.Self != nil && h.o.Self(ev.Organizer.Email) {
		return false
	}
	if ev.RRule != "" || len(ev.RDates) > 0 {
		return true
	}
	return inviteEnd(ev).After(h.o.Now())
}

// publish appends the invite event of a new mail invite. A failure is
// logged: the row is still written, so the invite lists.
func (h *inviteHook) publish(ctx context.Context, log *slog.Logger, f cache.FoundInvite, ref string, inv calendar.MailInvite) {
	ev := inviteEvent(inv)
	change := &watch.CalendarChange{
		Ref:        ref,
		MessageRef: f.MessageRef,
		Start:      calendarTime(ev.Start),
		End:        calendarTime(inviteEnd(ev)),
	}
	if ev.Organizer != nil && mailaddr.Safe(ev.Organizer.Email) {
		change.Organizer = ev.Organizer.Email
	}
	e := watch.Event{At: h.o.Now(), Kind: watch.KindInvite, Account: f.Account, Ref: ref, Calendar: change}
	if err := h.o.Log.Publish(ctx, e); err != nil {
		log.WarnContext(ctx, "could not publish an invite event", "ref", ref)
	}
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
