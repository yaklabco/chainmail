package calendar

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MailInvite is a parsed text/calendar part.
type MailInvite struct {
	UID      string
	Method   string // REQUEST, UPDATE or CANCEL
	Sequence int
	DTStamp  time.Time
	Events   []Event
}

// ParseInvite decodes data from charset ("" and us-ascii and utf-8 as
// UTF-8, iso-8859-1 and windows-1252 converted), parses it, and requires
// METHOD REQUEST, UPDATE or CANCEL (else ErrNotInvite) and exactly one UID.
//
// data over MaxICSBytes returns ErrTooLarge, before any conversion. Another
// charset, a VEVENT without a UID or VEVENTs with different UIDs return
// ErrInvalidICS; a calendar with no VEVENT returns ErrNotInvite. Sequence
// and DTStamp are the highest of the invite's VEVENTs.
func ParseInvite(data []byte, charset string) (MailInvite, error) {
	if len(data) > MaxICSBytes {
		return MailInvite{}, ErrTooLarge
	}
	text, err := toUTF8(data, charset)
	if err != nil {
		return MailInvite{}, err
	}
	// Conversion from a single-byte charset can at most triple the size.
	method, events, err := parseICS(text, 3*MaxICSBytes)
	if err != nil {
		return MailInvite{}, err
	}
	switch method {
	case MethodRequest, MethodUpdate, MethodCancel:
	default:
		return MailInvite{}, ErrNotInvite
	}
	if len(events) == 0 {
		return MailInvite{}, fmt.Errorf("%w: no VEVENT", ErrNotInvite)
	}
	inv := MailInvite{UID: events[0].UID, Method: method}
	for _, pe := range events {
		if pe.UID == "" {
			return MailInvite{}, fmt.Errorf("%w: VEVENT without UID", ErrInvalidICS)
		}
		if pe.UID != inv.UID {
			return MailInvite{}, fmt.Errorf("%w: more than one UID", ErrInvalidICS)
		}
		inv.Sequence = max(inv.Sequence, pe.Sequence)
		if pe.dtstamp.After(inv.DTStamp) {
			inv.DTStamp = pe.dtstamp
		}
		inv.Events = append(inv.Events, pe.Event)
	}
	return inv, nil
}

// cp1252 maps windows-1252 bytes 0x80 to 0x9F. The five bytes the code page
// leaves undefined map to the C1 control of the same value, as WHATWG does.
var cp1252 = [32]rune{
	0x20ac, 0x0081, 0x201a, 0x0192, 0x201e, 0x2026, 0x2020, 0x2021,
	0x02c6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008d, 0x017d, 0x008f,
	0x0090, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014,
	0x02dc, 0x2122, 0x0161, 0x203a, 0x0153, 0x009d, 0x017e, 0x0178,
}

// toUTF8 converts data from a MIME charset label. Invalid UTF-8 is left for
// the text cleaning in parseEvent to replace.
func toUTF8(data []byte, charset string) ([]byte, error) {
	label := strings.ToLower(strings.Trim(strings.TrimSpace(charset), `"`))
	var table *[32]rune
	switch label {
	case "", "us-ascii", "ascii", "utf-8", "utf8":
		return data, nil
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "latin1", "latin-1", "l1":
	case "windows-1252", "cp1252", "x-cp1252":
		table = &cp1252
	default:
		return nil, fmt.Errorf("%w: unsupported charset", ErrInvalidICS)
	}
	out := make([]byte, 0, len(data)+len(data)/4)
	for _, b := range data {
		r := rune(b)
		if table != nil && b >= 0x80 && b < 0xa0 {
			r = table[b-0x80]
		}
		out = utf8.AppendRune(out, r)
	}
	return out, nil
}
