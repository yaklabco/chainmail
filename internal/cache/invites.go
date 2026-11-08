package cache

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
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
}

// inviteCandidate is a new message's calendar part to read for an invite.
type inviteCandidate struct {
	ref  ref.Ref
	part mailstore.CalendarPart
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

// errInviteTooLarge is a part whose decoded bytes run past
// calendar.MaxICSBytes.
var errInviteTooLarge = errors.New("invite part over the size limit")

// readInvites reads each candidate's part and hands the invites read to
// OnInvites, in candidate order, with no lock held and outside any cache.db
// transaction. tooLarge counts the messages whose invite parts the header
// fetch found over the limit. A part that cannot be read is logged by
// reason code and skipped: invite detection never fails a build step. It
// does nothing without OnInvites.
//
// Invite work is optional, so it is bounded: the reads of one segment
// share a deadline of inviteTimeout, and at most inviteCap parts are read.
// A part the cap or the deadline leaves unread is skipped and counted in a
// log line; it is never read again, since the segment is acknowledged once
// apply returns.
func (a *Account) readInvites(ctx context.Context, cands []inviteCandidate, tooLarge int) {
	if a.o.OnInvites == nil {
		return
	}
	if over := len(cands) - a.inviteCap; over > 0 {
		cands = cands[:a.inviteCap]
		a.logger.InfoContext(ctx, "cache skipped invite parts over the per-segment cap", "count", over, "cap", a.inviteCap)
	}
	readCtx, cancel := context.WithTimeout(ctx, a.inviteTimeout)
	defer cancel()
	var (
		found    []FoundInvite
		failed   int
		timedOut int
	)
	for i, c := range cands {
		part, data, err := a.readInvite(readCtx, c)
		switch {
		case ctx.Err() != nil:
			return
		case readCtx.Err() != nil && err != nil:
			// The step's deadline passed: this part and the rest go unread.
			timedOut = len(cands) - i
		case errors.Is(err, errInviteTooLarge):
			tooLarge++
		case err != nil:
			failed++
			a.logger.InfoContext(ctx, "cache could not read an invite part", "folder", c.ref.Folder, "reason", reasonCode(err))
		default:
			found = append(found, FoundInvite{Account: a.name, MessageRef: c.ref.String(), Part: part, Data: data})
		}
		if timedOut > 0 {
			break
		}
	}
	if tooLarge > 0 {
		a.logger.InfoContext(ctx, "cache skipped invite parts over the size limit", "count", tooLarge)
	}
	if failed > 0 {
		a.logger.WarnContext(ctx, "cache skipped invite parts it could not read", "count", failed)
	}
	if timedOut > 0 {
		a.logger.WarnContext(ctx, "cache ran out of time for invite parts and skipped them", "count", timedOut, "timeout", a.inviteTimeout)
	}
	if len(found) > 0 {
		a.o.OnInvites(ctx, found)
	}
}

// readInvite streams one candidate's part on a background context,
// retrying while the pool is busy until ctx ends, reading at most
// calendar.MaxICSBytes + 1 bytes, and counts the bytes read against the
// day's budget. A store that can (mailstore.BackgroundPartStreamer) opens
// the part as background work, which never queues beside foreground
// requests for a connection; another store's StreamPart is used as it is.
// A part whose decoded bytes pass the limit returns errInviteTooLarge. The
// charset of the part's info fills in one the header lacked, in the part
// returned.
func (a *Account) readInvite(ctx context.Context, c inviteCandidate) (mailstore.CalendarPart, []byte, error) {
	var data []byte
	err := a.retryBusy(ctx, func() error {
		return a.withMail(ctx, func(bg context.Context, st MailStore) error {
			open := st.StreamPart
			if bs, ok := st.(mailstore.BackgroundPartStreamer); ok {
				open = bs.StreamPartBackground
			}
			info, rc, err := open(bg, c.ref, c.part.Part)
			if err != nil {
				return err
			}
			defer func() { _ = rc.Close() }() // a read-only stream: the read's error is the one to report
			if c.part.Charset == "" {
				c.part.Charset = strings.ToLower(info.Params["charset"])
			}
			data, err = io.ReadAll(io.LimitReader(rc, calendar.MaxICSBytes+1))
			return err
		})
	})
	if len(data) > 0 {
		a.chargeInvite(ctx, int64(len(data)))
	}
	if err != nil {
		return c.part, nil, err
	}
	if len(data) > calendar.MaxICSBytes {
		return c.part, nil, errInviteTooLarge
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
