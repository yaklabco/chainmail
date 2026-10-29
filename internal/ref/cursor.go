package ref

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidCursor means a string is not a cursor this daemon issued.
var ErrInvalidCursor = errors.New("invalid search cursor")

const (
	cursorPrefix = "c1."
	// maxCursorLen bounds a cursor. Hundreds of folders still fit.
	maxCursorLen = 64 << 10
)

// Cursor continues a search. Query fingerprints the search it belongs to,
// so a cursor cannot be replayed against a different query.
type Cursor struct {
	Query   string         `json:"q"`
	Sources []CursorSource `json:"s"`
}

// CursorSource is one searched folder: the UIDVALIDITY it had on the first
// page, and the UID the next page continues below, exclusive. Below 0 means
// no bound. Done means nothing is left in it.
type CursorSource struct {
	Account     string `json:"a"`
	Folder      string `json:"f"`
	UIDValidity uint32 `json:"v"`
	Below       uint32 `json:"b,omitempty"`
	Done        bool   `json:"d,omitempty"`
}

// EncodeCursor returns the opaque form of c.
func EncodeCursor(c Cursor) string {
	b, _ := json.Marshal(c) // strings, numbers and bools always marshal
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a cursor from EncodeCursor. Errors match
// ErrInvalidCursor.
func DecodeCursor(s string) (Cursor, error) {
	if len(s) > maxCursorLen {
		return Cursor{}, fmt.Errorf("%w: too long", ErrInvalidCursor)
	}
	body, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return Cursor{}, fmt.Errorf("%w: not a chainmail cursor", ErrInvalidCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: not base64url", ErrInvalidCursor)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Cursor
	if err := dec.Decode(&c); err != nil {
		return Cursor{}, fmt.Errorf("%w: malformed", ErrInvalidCursor)
	}
	if _, err := hex.DecodeString(c.Query); err != nil || len(c.Query) != 16 {
		return Cursor{}, fmt.Errorf("%w: bad query fingerprint", ErrInvalidCursor)
	}
	if len(c.Sources) == 0 {
		return Cursor{}, fmt.Errorf("%w: no sources", ErrInvalidCursor)
	}
	for _, src := range c.Sources {
		if !accountRE.MatchString(src.Account) || src.Folder == "" || !utf8.ValidString(src.Folder) || src.UIDValidity == 0 {
			return Cursor{}, fmt.Errorf("%w: bad source", ErrInvalidCursor)
		}
	}
	return c, nil
}

// Fingerprint hashes parts into 16 hex digits. Parts are separated, so
// ("ab", "c") and ("a", "bc") differ.
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
