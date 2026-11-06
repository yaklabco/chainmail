package query

import (
	"errors"
	"strings"
	"testing"
)

// FuzzParse checks that Parse never panics, that every error is a
// *SyntaxError, and that a parsed tree survives printing and reparsing. Plain
// `go test` runs the seed corpus below, so CI exercises it on every run.
// Explore further by hand with: go test -run '^$' -fuzz FuzzParse -fuzztime 60s ./internal/query/
func FuzzParse(f *testing.F) {
	seeds := []string{
		"", "invoice", `from:alex subject:"q3 report"`, "a OR b c", "-(a OR b)",
		"in:inbox account:gmail newer_than:7d", `"unclosed`, "from:", "(", ")", "a OR", "OR a", "-",
		"foo:bar", "is:maybe", "larger:5M smaller:10K", "after:2026-01-31 before:2026-02-01",
		`subject:"say \"hi\""`, "((((a))))", "-in:inbox", "a OR in:inbox", "label:x has:attachment filename:pdf",
		"café ü", "\x00", strings.Repeat("(", 40) + "a" + strings.Repeat(")", 40),
		`a\b "c\\d"`, "e-mail -x --y", `from:a"b"`, "\u00a0a\u00a0", "is:UNREAD", "in:\"Sent Items\"",
		"item:none", "item:it_admin_work", "-item:none OR item:it_x", "item:", "item:NONE", "(item:it_a OR from:x) -item:none", "item:none newer_than:7d invoice",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := Parse(s)
		if err != nil {
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("Parse(%q) error %T, want *SyntaxError", s, err)
			}
			return
		}
		if err := Validate(n); err != nil {
			t.Fatalf("Parse(%q) returned a tree that fails Validate: %v", s, err)
		}
		printed := n.String()
		again, err := Parse(printed)
		if err != nil {
			t.Fatalf("Parse(%q) printed as %q, which does not parse: %v", s, printed, err)
		}
		if !n.Equal(again) {
			t.Fatalf("Parse(%q) printed as %q reparses differently: %s", s, printed, again)
		}
	})
}
