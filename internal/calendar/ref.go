package calendar

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

const (
	refPrefix = "ce_"
	// refBodyLen is the base32 length of 16 bytes without padding.
	refBodyLen = 26
	ridLayout  = "20060102T150405Z"
	// maxRefLen is the longest valid ref: prefix, body, "@" and a recurrence ID.
	maxRefLen = len(refPrefix) + refBodyLen + 1 + len(ridLayout)
	// mailCalendarID is the calendar ID of mail-only invites. It starts with
	// a NUL, which no provider calendar ID contains.
	mailCalendarID = "\x00mail"
)

// refEncoding is lowercase Crockford base32: no i, l, o or u.
var refEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// EventRef is "ce_" + 26 lowercase Crockford base32 characters of
// sha256(account + "\x00" + calendarID + "\x00" + providerID)[:16].
func EventRef(account, calendarID, providerID string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + calendarID + "\x00" + providerID))
	return refPrefix + refEncoding.EncodeToString(sum[:16])
}

// InstanceRef appends "@" + rid in UTC as 20060102T150405Z.
func InstanceRef(base string, rid time.Time) string {
	return base + "@" + rid.UTC().Format(ridLayout)
}

// MailInviteRef is EventRef(account, "\x00mail", uid): the ref of an invite
// known only from mail.
func MailInviteRef(account, uid string) string {
	return EventRef(account, mailCalendarID, uid)
}

// ParseEventRef splits a ref into its base and its recurrence ID, which is
// zero for a ref without one. It accepts only the forms EventRef and
// InstanceRef produce, so each event has exactly one ref. Errors wrap
// ErrInvalidRef and never repeat s.
func ParseEventRef(s string) (base string, rid time.Time, err error) {
	if len(s) > maxRefLen {
		return "", time.Time{}, fmt.Errorf("%w: too long", ErrInvalidRef)
	}
	base, ridText, hasRID := strings.Cut(s, "@")
	body, ok := strings.CutPrefix(base, refPrefix)
	if !ok || len(body) != refBodyLen {
		return "", time.Time{}, fmt.Errorf("%w: want ce_ and 26 characters", ErrInvalidRef)
	}
	raw, decErr := refEncoding.DecodeString(body)
	if decErr != nil || refEncoding.EncodeToString(raw) != body {
		return "", time.Time{}, fmt.Errorf("%w: bad characters", ErrInvalidRef)
	}
	if !hasRID {
		return base, time.Time{}, nil
	}
	rid, perr := time.Parse(ridLayout, ridText)
	if perr != nil || rid.Format(ridLayout) != ridText {
		return "", time.Time{}, fmt.Errorf("%w: bad recurrence ID", ErrInvalidRef)
	}
	return base, rid.UTC(), nil
}
