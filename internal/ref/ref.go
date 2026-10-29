// Package ref encodes and decodes chainmail's message references and search
// cursors. Both are opaque to agents: they pass them back unchanged.
package ref

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrInvalid means a string is not a canonical chainmail message reference.
var ErrInvalid = errors.New("invalid message reference")

const (
	prefix = "cm1"
	// maxRefLen bounds a ref. A long IMAP folder name, percent-encoded, still fits.
	maxRefLen = 2048
)

// accountRE matches account names, as internal/config does.
var accountRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Ref points at one message on one account.
type Ref struct {
	Account     string
	Folder      string
	UIDValidity uint32
	UID         uint32
	// GmailMsgID is X-GM-MSGID on Google accounts and 0 elsewhere. It finds
	// the message again when All Mail's UIDVALIDITY changes.
	GmailMsgID uint64
}

// String returns the canonical form of r.
func (r Ref) String() string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte(':')
	b.WriteString(r.Account)
	b.WriteByte(':')
	b.WriteString(escapeFolder(r.Folder))
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(uint64(r.UIDValidity), 10))
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(uint64(r.UID), 10))
	if r.GmailMsgID != 0 {
		b.WriteString(":g")
		b.WriteString(strconv.FormatUint(r.GmailMsgID, 10))
	}
	return b.String()
}

// Parse decodes a ref. It accepts only the form String produces, so every
// message has exactly one ref. Errors match ErrInvalid and never repeat s,
// which may be anything a client sent.
func Parse(s string) (Ref, error) {
	if len(s) > maxRefLen {
		return Ref{}, fmt.Errorf("%w: longer than %d bytes", ErrInvalid, maxRefLen)
	}
	parts := strings.Split(s, ":")
	if (len(parts) != 5 && len(parts) != 6) || parts[0] != prefix {
		return Ref{}, fmt.Errorf("%w: want cm1:<account>:<folder>:<uidvalidity>:<uid>", ErrInvalid)
	}
	if !accountRE.MatchString(parts[1]) {
		return Ref{}, fmt.Errorf("%w: bad account name", ErrInvalid)
	}
	folder, err := unescapeFolder(parts[2])
	if err != nil {
		return Ref{}, err
	}
	v, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil || v == 0 {
		return Ref{}, fmt.Errorf("%w: bad UIDVALIDITY", ErrInvalid)
	}
	u, err := strconv.ParseUint(parts[4], 10, 32)
	if err != nil || u == 0 {
		return Ref{}, fmt.Errorf("%w: bad UID", ErrInvalid)
	}
	r := Ref{Account: parts[1], Folder: folder, UIDValidity: uint32(v), UID: uint32(u)}
	if len(parts) == 6 {
		id, ok := strings.CutPrefix(parts[5], "g")
		n, err := strconv.ParseUint(id, 10, 64)
		if !ok || err != nil || n == 0 {
			return Ref{}, fmt.Errorf("%w: bad Gmail message ID", ErrInvalid)
		}
		r.GmailMsgID = n
	}
	if r.String() != s {
		return Ref{}, fmt.Errorf("%w: not in canonical form", ErrInvalid)
	}
	return r, nil
}

// unreserved reports the bytes a folder keeps as they are (RFC 3986).
func unreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func escapeFolder(folder string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(folder); i++ {
		c := folder[i]
		if unreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

func unescapeFolder(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case unreserved(c):
			b.WriteByte(c)
		case c == '%' && i+2 < len(s):
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("%w: bad folder escape", ErrInvalid)
			}
			b.WriteByte(byte(n))
			i += 2
		default:
			return "", fmt.Errorf("%w: bad character in folder", ErrInvalid)
		}
	}
	folder := b.String()
	if folder == "" || !utf8.ValidString(folder) {
		return "", fmt.Errorf("%w: bad folder", ErrInvalid)
	}
	return folder, nil
}
