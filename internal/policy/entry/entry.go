// Package entry validates and matches send allowlist entries. It is a leaf
// package so that both internal/config and internal/policy can use it
// without an import cycle, and the config check and the matcher never
// disagree.
package entry

import (
	"net/mail"
	"strings"
	"unicode"
)

// wildcardPrefix starts an entry that allows every address at a domain.
const wildcardPrefix = "*@"

// Valid reports whether s is an allowlist entry: a bare address such as
// someone@example.com, or *@domain.
func Valid(s string) bool {
	if domain, ok := strings.CutPrefix(s, wildcardPrefix); ok {
		return validDomain(domain)
	}
	if strings.Count(s, "@") != 1 || strings.Contains(s, "*") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Name == "" && addr.Address == s
}

// validDomain reports whether d can follow *@. It must be non-empty and hold
// no *, @, angle brackets, whitespace or control characters.
func validDomain(d string) bool {
	if d == "" {
		return false
	}
	for _, r := range d {
		if strings.ContainsRune("*@<>", r) || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Match reports whether the allowlist entry allows addr. Both must already be
// lowercased. A *@domain entry matches an address whose text after its last @
// equals the domain exactly, so subdomains do not match. Any other entry
// matches only the identical address.
func Match(entry, addr string) bool {
	if domain, ok := strings.CutPrefix(entry, wildcardPrefix); ok {
		i := strings.LastIndexByte(addr, '@')
		return i >= 0 && addr[i+1:] == domain
	}
	return entry == addr
}
