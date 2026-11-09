package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/watch"
)

// FoundInvite is an iCalendar invite the builder found in a new message:
// a calendar part with method REQUEST, UPDATE or CANCEL. Data, Part's
// method and charset and everything in them come from the message's
// sender and are untrusted.
type FoundInvite struct {
	Account    string
	MessageRef string
	Part       mailstore.CalendarPart
	// Data is the part with its transfer encoding decoded, still in
	// Part.Charset, at most calendar.MaxICSBytes.
	Data []byte
	// FoundAt is when chainmail found the message: the time of its new
	// event in the log, so a segment applied again, or a part read later
	// through ReadInvite, keeps it. It is the builder's clock when the
	// event has no time.
	FoundAt time.Time
}

// inviteCandidate is a new message's calendar part to read for an invite.
type inviteCandidate struct {
	ref  ref.Ref
	part mailstore.CalendarPart
	// foundAt is FoundInvite.FoundAt, set by readInvites.
	foundAt time.Time
}

// inviteMethod reports whether a calendar part's method marks an invite
// the builder reads.
func inviteMethod(method string) bool {
	switch method {
	case calendar.MethodRequest, calendar.MethodUpdate, calendar.MethodCancel:
		return true
	default:
		return false
	}
}

// inviteOf returns the part of h to read for an invite: its first
// calendar part with an invite method and a reported size of at most
// calendar.MaxICSBytes. tooLarge says h has invite parts, all of them over
// the limit.
func inviteOf(h mailstore.Header) (c inviteCandidate, ok, tooLarge bool) {
	for _, p := range h.CalendarParts {
		if !inviteMethod(p.Method) {
			continue
		}
		if p.Size > calendar.MaxICSBytes {
			tooLarge = true
			continue
		}
		return inviteCandidate{ref: h.Summary.Ref, part: p}, true, false
	}
	return inviteCandidate{}, false, tooLarge
}

// ErrInviteTooLarge is a part whose decoded bytes run past
// calendar.MaxICSBytes.
var ErrInviteTooLarge = errors.New("invite part over the size limit")

// ErrInviteUndecodable is a part that fails the same way on every read:
// its transfer encoding (base64 or quoted-printable) does not decode, or
// the server sends it in a form chainmail cannot read
// (mailstore.ErrMalformed). It carries no part text.
var ErrInviteUndecodable = errors.New("invite part does not decode")

// errInviteNotOurs is a deferred part ReadInvite was handed for a message
// that is not one of its account's. It is not found, for this account.
var errInviteNotOurs = fmt.Errorf("%w: the invite part is not this account's", mailstore.ErrNotFound)

// InviteCandidate is a calendar part of a new message that the builder's
// invite step did not read: past the per-segment cap, after the step's
// deadline (a busy pool included), or after a stream failure that may pass.
// The builder hands it to OnDeferredInvites, and ReadInvite reads it later.
// Part's method and charset come from the message and are untrusted.
type InviteCandidate struct {
	Account    string
	MessageRef string
	Part       mailstore.CalendarPart
	// FoundAt is when chainmail found the message, as FoundInvite.FoundAt;
	// ReadInvite hands it on.
	FoundAt time.Time
}

// inviteGone reports whether a part could not be read because its message
// or folder is gone, which no later read changes.
func inviteGone(err error) bool {
	return errors.Is(err, mailstore.ErrNotFound) || errors.Is(err, mailstore.ErrStaleRef)
}

// messageKey names a message by folder, UIDVALIDITY and UID, the part of a
// ref that a new event and the header fetched for it share.
type messageKey struct {
	folder           string
	uidValidity, uid uint32
}

// keyOf is r's messageKey.
func keyOf(r ref.Ref) messageKey {
	return messageKey{folder: r.Folder, uidValidity: r.UIDValidity, uid: r.UID}
}

// newMailTimes returns the time of each new event of seg, by message: when
// the watcher found the message. An event with an unreadable ref or no
// time is left out.
func newMailTimes(seg []watch.Event) map[messageKey]time.Time {
	out := make(map[messageKey]time.Time)
	for _, e := range seg {
		if e.Kind != watch.KindNew || e.At.IsZero() {
			continue
		}
		r, err := ref.Parse(e.Ref)
		if err != nil {
			continue
		}
		if _, ok := out[keyOf(r)]; !ok {
			out[keyOf(r)] = e.At
		}
	}
	return out
}

// readInvites reads each candidate's part and hands the invites read to
// OnInvites, in candidate order, with no lock held and outside any cache.db
// transaction. tooLarge counts the messages whose invite parts the header
// fetch found over the limit, and found has the time each message was
// found (newMailTimes); a message it lacks takes the builder's clock. It
// does nothing without OnInvites.
//
// Invite work is optional, so the step is bounded: the reads of one
// segment share a deadline of inviteTimeout, and at most inviteCap parts
// are read. No part is lost to those bounds: every part the cap or the
// deadline leaves unread, and every part whose read failed with an error
// that may pass, goes to OnDeferredInvites, which queues it for a later
// read with ReadInvite. A part over the limit, whose message is gone, or
// that does not decode is dropped and counted. Without OnDeferredInvites,
// the parts it would get are skipped and counted in a log line.
//
// A read never fails the step. It fails, so the segment is not
// acknowledged and is applied again, only when OnDeferredInvites could not
// queue the parts, or when ctx ends: a stopping builder hands nothing on,
// and the next start reads the segment's invites again.
func (a *Account) readInvites(ctx context.Context, cands []inviteCandidate, tooLarge int, found map[messageKey]time.Time) error {
	if a.o.OnInvites == nil {
		return nil
	}
	now := a.clock.Now()
	for i := range cands {
		at, ok := found[keyOf(cands[i].ref)]
		if !ok {
			at = now
		}
		cands[i].foundAt = at
	}
	var deferred []inviteCandidate
	if over := len(cands) - a.inviteCap; over > 0 {
		deferred = append(deferred, cands[a.inviteCap:]...)
		cands = cands[:a.inviteCap]
		a.logger.InfoContext(ctx, a.deferText("cache skipped invite parts over the per-segment cap",
			"cache deferred invite parts over the per-segment cap"), "count", over, "cap", a.inviteCap)
	}
	readCtx, cancel := context.WithTimeout(ctx, a.inviteTimeout)
	defer cancel()
	var (
		invites                        []FoundInvite
		failed, gone, broken, timedOut int
	)
	for i, c := range cands {
		part, data, err := a.readInvite(readCtx, c)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case readCtx.Err() != nil && err != nil:
			// The step's deadline passed: this part and the rest go unread.
			timedOut = len(cands) - i
			deferred = append(deferred, cands[i:]...)
		case errors.Is(err, ErrInviteTooLarge):
			tooLarge++
		case inviteGone(err):
			gone++
		case errors.Is(err, ErrInviteUndecodable):
			broken++
		case err != nil:
			failed++
			deferred = append(deferred, c)
			a.logger.InfoContext(ctx, "cache could not read an invite part", "folder", c.ref.Folder, "reason", reasonCode(err))
		default:
			invites = append(invites, FoundInvite{Account: a.name, MessageRef: c.ref.String(), Part: part, Data: data, FoundAt: c.foundAt})
		}
		if timedOut > 0 {
			break
		}
	}
	if tooLarge > 0 {
		a.logger.InfoContext(ctx, "cache skipped invite parts over the size limit", "count", tooLarge)
	}
	if gone > 0 {
		a.logger.InfoContext(ctx, "cache skipped invite parts whose messages are gone", "count", gone)
	}
	if broken > 0 {
		a.logger.InfoContext(ctx, "cache skipped invite parts that do not decode", "count", broken)
	}
	if failed > 0 {
		a.logger.WarnContext(ctx, a.deferText("cache skipped invite parts it could not read",
			"cache deferred invite parts it could not read"), "count", failed)
	}
	if timedOut > 0 {
		a.logger.WarnContext(ctx, a.deferText("cache ran out of time for invite parts and skipped them",
			"cache ran out of time for invite parts and deferred them"), "count", timedOut, "timeout", a.inviteTimeout)
	}
	if len(invites) > 0 {
		a.o.OnInvites(ctx, invites)
	}
	if len(deferred) == 0 || a.o.OnDeferredInvites == nil {
		return nil
	}
	out := make([]InviteCandidate, len(deferred))
	for i, c := range deferred {
		out[i] = InviteCandidate{Account: a.name, MessageRef: c.ref.String(), Part: c.part, FoundAt: c.foundAt}
	}
	if err := a.o.OnDeferredInvites(ctx, out); err != nil {
		return fmt.Errorf("queueing %d deferred invite parts: %w", len(out), err)
	}
	return nil
}

// deferText is skipped without OnDeferredInvites and deferred with it.
func (a *Account) deferText(skipped, deferred string) string {
	if a.o.OnDeferredInvites == nil {
		return skipped
	}
	return deferred
}

// ReadInvite reads the part of an invite the builder deferred, once, as
// background work: when the pool has no idle connection for it, it fails
// at once with mailstore.ErrBusy, never waiting beside foreground
// requests. It reads at most calendar.MaxICSBytes + 1 bytes and counts
// them against the day's budget, as the builder's invite step does. A
// part over the limit is ErrInviteTooLarge, and a message or folder that
// is gone is mailstore.ErrNotFound or mailstore.ErrStaleRef; so is a part
// of a message that is not this account's. A part that does not decode
// is ErrInviteUndecodable. Those never pass; other errors may. The invite
// carries c.FoundAt.
func (a *Account) ReadInvite(ctx context.Context, c InviteCandidate) (FoundInvite, error) {
	r, err := ref.Parse(c.MessageRef)
	if err != nil || c.Account != a.name || r.Account != a.name {
		return FoundInvite{}, errInviteNotOurs
	}
	part, data, err := a.readPart(ctx, inviteCandidate{ref: r, part: c.Part}, false)
	if err != nil {
		return FoundInvite{}, err
	}
	return FoundInvite{Account: a.name, MessageRef: c.MessageRef, Part: part, Data: data, FoundAt: c.FoundAt}, nil
}

// readInvite is readPart retrying while the pool is busy, until ctx ends.
func (a *Account) readInvite(ctx context.Context, c inviteCandidate) (mailstore.CalendarPart, []byte, error) {
	return a.readPart(ctx, c, true)
}

// streamErr classifies an error reading an open part's stream. A failure
// that may pass keeps its error: the server unavailable (a dropped
// connection included), a busy pool, a login needed or refused, or ctx
// ended or the stream closed. So does a message that is gone, which
// callers already know. ErrMalformed is ErrInviteUndecodable too
// (malformedErr). Anything else comes from decoding the part's transfer
// encoding, as mailstore stores report it (imapstore's decoding error
// matches no mailstore sentinel), and is ErrInviteUndecodable, without the
// decoder's text, which can quote the part.
func streamErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mailstore.ErrUnavailable), errors.Is(err, mailstore.ErrBusy),
		errors.Is(err, auth.ErrAuthRequired), errors.Is(err, auth.ErrRejected),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, fs.ErrClosed), inviteGone(err):
		return err
	case errors.Is(err, mailstore.ErrMalformed):
		return malformedErr(err)
	default:
		return ErrInviteUndecodable
	}
}

// malformedErr makes a mailstore.ErrMalformed error ErrInviteUndecodable
// as well: the server sends the part in a form chainmail cannot read, and
// asking again gets the same. Any other error is returned as it is.
func malformedErr(err error) error {
	if errors.Is(err, mailstore.ErrMalformed) && !errors.Is(err, ErrInviteUndecodable) {
		return fmt.Errorf("%w: %w", ErrInviteUndecodable, err)
	}
	return err
}

// readPart streams one candidate's part on a background context, reading
// at most calendar.MaxICSBytes + 1 bytes, and counts the bytes read
// against the day's budget. With retry it tries again while the pool is
// busy, until ctx ends; without, a busy pool is mailstore.ErrBusy. A store
// that can (mailstore.BackgroundPartStreamer) opens the part as background
// work, which never queues beside foreground requests for a connection;
// another store's StreamPart is used as it is. A part whose decoded bytes
// pass the limit returns ErrInviteTooLarge, and one that does not decode
// ErrInviteUndecodable (streamErr). The charset of the part's info fills
// in one the header lacked, in the part returned.
func (a *Account) readPart(ctx context.Context, c inviteCandidate, retry bool) (mailstore.CalendarPart, []byte, error) {
	var data []byte
	read := func() error {
		return a.withMail(ctx, func(bg context.Context, st MailStore) error {
			open := st.StreamPart
			if bs, ok := st.(mailstore.BackgroundPartStreamer); ok {
				open = bs.StreamPartBackground
			}
			info, rc, err := open(bg, c.ref, c.part.Part)
			if err != nil {
				return malformedErr(err)
			}
			defer func() { _ = rc.Close() }() // a read-only stream: the read's error is the one to report
			if c.part.Charset == "" {
				c.part.Charset = strings.ToLower(info.Params["charset"])
			}
			data, err = io.ReadAll(io.LimitReader(rc, calendar.MaxICSBytes+1))
			return streamErr(err)
		})
	}
	var err error
	if retry {
		err = a.retryBusy(ctx, read)
	} else {
		err = read()
	}
	if len(data) > 0 {
		a.chargeInvite(ctx, int64(len(data)))
	}
	if err != nil {
		return c.part, nil, err
	}
	if len(data) > calendar.MaxICSBytes {
		return c.part, nil, ErrInviteTooLarge
	}
	return c.part, data, nil
}

// Bounds of the builder's invite step for one segment.
const (
	// inviteStepTimeout is how long the step may spend reading parts.
	inviteStepTimeout = 30 * time.Second
	// maxInvitesPerSegment is how many parts the step reads at most.
	maxInvitesPerSegment = 20
)

// chargeInvite counts n bytes of an invite part read against the day's
// budget. The charge runs without ctx's cancellation, since the bytes were
// read even when a deadline cut the read short, but bounded by
// keepTimeout, as the read path's bookkeeping is: cache.db has one
// connection, and a charge must not wait for it without end. A charge that
// fails is logged and passed to OnError; it never fails the read.
func (a *Account) chargeInvite(ctx context.Context, n int64) {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.keepTimeout)
	defer cancel()
	if _, err := a.o.Store.AddBudget(bctx, a.name, a.day(), n); err != nil {
		a.noteErr(err)
		a.logger.WarnContext(ctx, "cache could not count an invite part against the budget", "reason", reasonCode(err))
	}
}
