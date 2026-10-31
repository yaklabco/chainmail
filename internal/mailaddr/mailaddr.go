// Package mailaddr decides which addresses can be written into an SMTP
// envelope as they are. It is a leaf package so that the send policy and the
// SMTP sender share one grammar and never disagree.
package mailaddr

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits from RFC 5321 section 4.5.3.1.
const (
	maxLocal  = 64
	maxDomain = 253
	maxLabel  = 63
	maxTotal  = 254
)

// atextSymbols are the RFC 5322 atext characters other than letters and
// digits.
const atextSymbols = "!#$%&'*+-/=?^_`{|}~"

// Safe reports whether addr can be written into an SMTP MAIL FROM or RCPT
// TO path as is: local@domain with no quoting, no whitespace, no control
// characters and no SMTP path or parameter delimiters.
//
// The local part is an RFC 5322 dot-atom and the domain a hostname, either
// of which may hold printable non-ASCII UTF-8 for SMTPUTF8. Quoted local
// parts and address literals are not safe: the decoded form of a quoted
// local part can carry delimiters that SMTP would read as syntax.
func Safe(addr string) bool {
	if len(addr) > maxTotal || strings.Count(addr, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(addr, "@")
	return safeLocal(local) && safeDomain(domain)
}

// safeLocal reports whether s is a dot-atom of at most maxLocal bytes.
func safeLocal(s string) bool {
	if len(s) > maxLocal {
		return false
	}
	for atom := range strings.SplitSeq(s, ".") {
		if !allRunes(atom, isAtext) {
			return false
		}
	}
	return true
}

// safeDomain reports whether s is dot-separated labels of letters, digits
// and inner hyphens, at most maxDomain bytes.
func safeDomain(s string) bool {
	if len(s) > maxDomain {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if len(label) > maxLabel || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		if !allRunes(label, isLabelRune) {
			return false
		}
	}
	return true
}

// allRunes reports whether s is non-empty valid UTF-8 and ok accepts each
// of its runes.
func allRunes(s string, ok func(rune) bool) bool {
	if s == "" {
		return false
	}
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && n <= 1 {
			return false
		}
		if !ok(r) {
			return false
		}
		s = s[n:]
	}
	return true
}

func isAtext(r rune) bool {
	return isAlnum(r) || strings.ContainsRune(atextSymbols, r) || isPrintableNonASCII(r)
}

func isLabelRune(r rune) bool {
	return isAlnum(r) || r == '-' || isPrintableNonASCII(r)
}

func isAlnum(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// isPrintableNonASCII accepts a non-ASCII rune that is printable and not a
// space, so no control, format or space character gets through.
func isPrintableNonASCII(r rune) bool {
	return r >= utf8.RuneSelf && unicode.IsPrint(r)
}
