package cache_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

// FuzzCompile checks that every tree query.Parse accepts, split as the
// daemon splits it, either compiles on both dialects to SQL the cache runs
// without error, both as a search and as a count, or is refused off Google
// with ErrUnsupportedQuery, as imapstore refuses it. Compile must never
// panic, every ? must have its argument, and only a server-only plan has a
// reason. Plain go test runs the seed corpus, which starts with
// internal/query's own. Explore further by hand only with James's approval
// (it is long fuzzing): go test -run '^$' -fuzz FuzzCompile -fuzztime 60s ./internal/cache/
func FuzzCompile(f *testing.F) {
	seeds := []string{
		// internal/query's FuzzParse corpus.
		"", "invoice", `from:alex subject:"q3 report"`, "a OR b c", "-(a OR b)",
		"in:inbox account:gmail newer_than:7d", `"unclosed`, "from:", "(", ")", "a OR", "OR a", "-",
		"foo:bar", "is:maybe", "larger:5M smaller:10K", "after:2026-01-31 before:2026-02-01",
		`subject:"say \"hi\""`, "((((a))))", "-in:inbox", "a OR in:inbox", "label:x has:attachment filename:pdf",
		"café ü", "\x00", strings.Repeat("(", 40) + "a" + strings.Repeat(")", 40),
		`a\b "c\\d"`, "e-mail -x --y", `from:a"b"`, " a ", "is:UNREAD", "in:\"Sent Items\"",
		// The compiler's own cases.
		"newer_than:7d invoice", "newer_than:7d -invoice -(a OR b)", "(newer_than:7d invoice) OR from:acme",
		"in:trash invoice", "in:anywhere", "in:archive is:unread", `label:\Seen`, "from:% subject:_",
		`subject:\o/`, "larger:0 smaller:0", "larger:4294967296", "older_than:9999y", "after:0001-01-01",
		"a -b -c", "a (b OR -c)", "!!!", `"a""b"`, "-(-(-a))", "a OR -b OR (c d)",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	s := openCompileStore(f)
	f.Fuzz(func(t *testing.T, q string) {
		n, err := query.Parse(q)
		if err != nil {
			return
		}
		scope, rest := query.Split(n)
		for _, gmail := range []bool{false, true} {
			p, err := cache.Compile(rest, scope.In, gmail, compileNow)
			if err != nil {
				if gmail || !errors.Is(err, mailstore.ErrUnsupportedQuery) {
					t.Fatalf("Compile(%q, gmail %v): %v", q, gmail, err)
				}
				continue
			}
			if got, want := strings.Count(p.Where.SQL, "?"), len(p.Where.Args); got != want {
				t.Fatalf("Compile(%q, gmail %v): %d placeholders, %d args in %q", q, gmail, got, want, p.Where.SQL)
			}
			if p.Reason != "" && !p.ServerOnly {
				t.Fatalf("Compile(%q, gmail %v): reason %q without ServerOnly", q, gmail, p.Reason)
			}
			account, src := compileSource(gmail)
			for _, countOnly := range []bool{false, true} {
				if _, _, err := s.SearchUIDs(t.Context(), account, src, p.Where, 0, countOnly); err != nil {
					t.Fatalf("Compile(%q, gmail %v) runs with an error: %v\nwhere %+v", q, gmail, err, p.Where)
				}
			}
		}
	})
}
