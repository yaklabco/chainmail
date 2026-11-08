package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/mailstore"
)

// The queue of invite parts the cache builder deferred (invite_fetches in
// calendar.db), which the retry loop reads.
const (
	// inviteFetchBatch is how many queued parts one retry pass reads at
	// most.
	inviteFetchBatch = 50
	// inviteFetchBackoffBase and inviteFetchBackoffMax bound the wait
	// before a part whose read failed is tried again: 5m doubling to 6h.
	inviteFetchBackoffBase = 5 * time.Minute
	inviteFetchBackoffMax  = 6 * time.Hour
)

// errInviteAccountGone is a queued part of an account that is no longer
// configured: its message cannot be read any more, as if it were gone.
var errInviteAccountGone = fmt.Errorf("%w: the account is not configured", mailstore.ErrNotFound)

// errInviteNoCache is a queued part of an account whose cache worker is
// not running now. The part stays queued.
var errInviteNoCache = errors.New("the account's cache is not running")

// inviteFetchBackoff is the wait before the next read of a part whose read
// failed attempts times.
func inviteFetchBackoff(attempts int) time.Duration {
	d := inviteFetchBackoffBase
	for i := 1; i < attempts && d < inviteFetchBackoffMax; i++ {
		d *= 2
	}
	return min(d, inviteFetchBackoffMax)
}

// inviteFetchDone reports whether a read error means the part will never
// be read: its message or folder is gone, or it is over the size limit.
func inviteFetchDone(err error) bool {
	return errors.Is(err, mailstore.ErrNotFound) || errors.Is(err, mailstore.ErrStaleRef) ||
		errors.Is(err, cache.ErrInviteTooLarge)
}

// deferredFunc is h's OnDeferredInvites, nil when h is.
func (h *inviteHook) deferredFunc() func(context.Context, []cache.InviteCandidate) {
	if h == nil {
		return nil
	}
	return h.onDeferred
}

// onDeferred queues the invite parts the cache builder could not read in
// its bounded step, due at once, for the retry loop to read. A part
// queued already keeps its attempts and backoff. A failure is logged with
// a count: the builder acknowledges the segment whatever happens here.
func (h *inviteHook) onDeferred(ctx context.Context, cands []cache.InviteCandidate) {
	now := h.o.Now()
	rows := make([]calcache.InviteFetchRow, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, calcache.InviteFetchRow{
			Account: c.Account, MessageRef: c.MessageRef, Part: c.Part.Part, Method: c.Part.Method,
			Charset: c.Part.Charset, Size: c.Part.Size, NextAt: now, FoundAt: now,
		})
	}
	if err := h.o.Cache.PutInviteFetches(ctx, rows); err != nil {
		h.o.Logger.WarnContext(ctx, "could not queue invite parts to read later; they are lost", "count", len(rows))
	}
}

// pass is one run of the retry loop: it reads the queued invite parts that
// are due, then announces the pending invites of accounts. It stops once
// ctx ends.
func (h *inviteHook) pass(ctx context.Context, accounts []string) {
	h.drainFetches(ctx)
	for _, account := range accounts {
		if ctx.Err() != nil {
			return
		}
		h.announce(ctx, account)
	}
}

// drainFetches reads at most inviteFetchBatch queued invite parts that are
// due, the ones queued longest ago first, through ReadPart, and runs each
// invite read through the hook as the builder's are: stored, announced
// and its account poked. A part read and handled, or that will never be
// read (its message is gone, it is over the limit, or it is no usable
// invite), leaves the queue. Any other failure puts its next read off by
// inviteFetchBackoff; a part is never dropped for its attempts, since its
// message may still be there. It stops between parts once ctx ends, and a
// read cut short by that counts no attempt. It does nothing without
// ReadPart.
func (h *inviteHook) drainFetches(ctx context.Context) {
	if h.o.ReadPart == nil {
		return
	}
	now := h.o.Now()
	rows, err := h.o.Cache.DueInviteFetches(ctx, now, inviteFetchBatch)
	if err != nil {
		if ctx.Err() == nil {
			h.o.Logger.WarnContext(ctx, "could not read the queued invite parts")
		}
		return
	}
	var (
		accounts, poke   []string
		dropped, delayed int
	)
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		log := h.o.Logger.With("account", row.Account)
		f, err := h.o.ReadPart(ctx, cache.InviteCandidate{
			Account: row.Account, MessageRef: row.MessageRef,
			Part: mailstore.CalendarPart{Part: row.Part, Method: row.Method, Charset: row.Charset, Size: row.Size},
		})
		var done bool
		switch {
		case err != nil && ctx.Err() != nil:
			continue
		case inviteFetchDone(err):
			dropped++
			done = true
		case err != nil:
			log.InfoContext(ctx, "could not read a queued invite part", "reason", apiReason(err))
		default:
			if !slices.Contains(accounts, row.Account) {
				accounts = append(accounts, row.Account)
			}
			switch h.handle(ctx, f) {
			case inviteStored:
				if !slices.Contains(poke, row.Account) {
					poke = append(poke, row.Account)
				}
				done = true
			case inviteUnusable:
				done = true
			case inviteNotStored:
			}
		}
		if done {
			if err := h.o.Cache.DeleteInviteFetch(ctx, row.Account, row.MessageRef, row.Part); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "could not remove a queued invite part")
			}
			continue
		}
		delayed++
		row.Attempts++
		row.NextAt = now.Add(inviteFetchBackoff(row.Attempts))
		if err := h.o.Cache.DeferInviteFetch(ctx, row); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "could not put off a queued invite part")
		}
	}
	if dropped > 0 {
		h.o.Logger.InfoContext(ctx, "dropped queued invite parts whose messages are gone or too large", "count", dropped)
	}
	if delayed > 0 {
		h.o.Logger.InfoContext(ctx, "put off queued invite parts that could not be read", "count", delayed)
	}
	h.finish(ctx, accounts, poke)
}

// readInvitePart reads a queued invite part through its account's cache
// worker, as background work. An account no longer configured is
// errInviteAccountGone, and one whose cache worker is not running (cache
// off, down, or watching stopped) errInviteNoCache.
func (s *Server) readInvitePart(ctx context.Context, c cache.InviteCandidate) (cache.FoundInvite, error) {
	if _, err := s.opts.Config.Account(c.Account); err != nil {
		return cache.FoundInvite{}, errInviteAccountGone
	}
	ca := s.cacheAccount(c.Account)
	if ca == nil {
		return cache.FoundInvite{}, errInviteNoCache
	}
	return ca.ReadInvite(ctx, c)
}

// calendarDeferredInvites returns the OnDeferredInvites hook of the cache
// workers over the calendar cache cal, or nil when cal is nil. It shares
// the server's invite hook.
func (s *Server) calendarDeferredInvites(cal *calcache.Store, poke func(account string)) func(context.Context, []cache.InviteCandidate) {
	return s.serverInviteHook(cal, poke).deferredFunc()
}
