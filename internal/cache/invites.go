package cache

import (
	"context"
	"errors"
	"io"
	"strings"

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
func (a *Account) readInvites(ctx context.Context, cands []inviteCandidate, tooLarge int) {
	if a.o.OnInvites == nil {
		return
	}
	var (
		found  []FoundInvite
		failed int
	)
	for _, c := range cands {
		part, data, err := a.readInvite(ctx, c)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errInviteTooLarge):
			tooLarge++
		case err != nil:
			failed++
			a.logger.InfoContext(ctx, "cache could not read an invite part", "folder", c.ref.Folder, "reason", reasonCode(err))
		default:
			found = append(found, FoundInvite{Account: a.name, MessageRef: c.ref.String(), Part: part, Data: data})
		}
	}
	if tooLarge > 0 {
		a.logger.InfoContext(ctx, "cache skipped invite parts over the size limit", "count", tooLarge)
	}
	if failed > 0 {
		a.logger.WarnContext(ctx, "cache skipped invite parts it could not read", "count", failed)
	}
	if len(found) > 0 {
		a.o.OnInvites(ctx, found)
	}
}

// readInvite streams one candidate's part on a background context,
// retrying while the pool is busy, reading at most calendar.MaxICSBytes + 1
// bytes, and counts the bytes read against the day's budget. A part whose
// decoded bytes pass the limit returns errInviteTooLarge. The charset of
// the part's info fills in one the header lacked, in the part returned.
func (a *Account) readInvite(ctx context.Context, c inviteCandidate) (mailstore.CalendarPart, []byte, error) {
	var data []byte
	err := a.retryBusy(ctx, func() error {
		return a.withMail(ctx, func(bg context.Context, st MailStore) error {
			info, rc, err := st.StreamPart(bg, c.ref, c.part.Part)
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
		if _, berr := a.o.Store.AddBudget(ctx, a.name, a.day(), int64(len(data))); berr != nil {
			a.noteErr(berr)
			a.logger.WarnContext(ctx, "cache could not count an invite part against the budget", "reason", reasonCode(berr))
		}
	}
	if err != nil {
		return c.part, nil, err
	}
	if len(data) > calendar.MaxICSBytes {
		return c.part, nil, errInviteTooLarge
	}
	return c.part, data, nil
}
