package cache_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

// openItemStore opens a cache holding threaded rows in both fixture
// folders. On the standard account: UIDs 1 and 2 in thread aaaa, 3 in
// bbbb, 4 in no thread and 5 in cccc; 1 and 5 are read. On the Google
// account: 11 and 12 in thread 1a2b and 13 in ff; 11 and 13 are in the
// inbox.
func openItemStore(t *testing.T) *cache.Store {
	t.Helper()
	s, err := cache.Open(t.Context(), filepath.Join(t.TempDir(), "cache.db"), cache.OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	when := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	std := func(uid uint32, thread, from string, flags ...string) cache.Row {
		return cache.Row{
			Account: compileStdAccount, Folder: compileStdFolder, UIDValidity: compileStdValidity, UID: uid,
			InternalDate: when, Date: when, Subject: "s", Size: 100, ThreadID: thread,
			From: []mailstore.Address{{Email: from}}, Flags: flags,
		}
	}
	gm := func(uid uint32, thread string, labels ...string) cache.Row {
		return cache.Row{
			Account: compileGmailAccount, Folder: compileGmailFolder, UIDValidity: compileGmailValidity, UID: uid,
			InternalDate: when, Date: when, Subject: "s", Size: 100, ThreadID: thread, Labels: labels,
		}
	}
	rows := []cache.Row{
		std(1, "aaaa", "ann@x.test", `\Seen`),
		std(2, "aaaa", "bob@x.test"),
		std(3, "bbbb", "ann@x.test"),
		std(4, "", "ann@x.test"),
		std(5, "cccc", "carol@x.test", `\Seen`),
		gm(11, "1a2b", `\Inbox`),
		gm(12, "1a2b"),
		gm(13, "ff", `\Inbox`),
	}
	if err := s.PutRows(t.Context(), rows); err != nil {
		t.Fatalf("PutRows: %v", err)
	}
	return s
}

// itemSearch parses q, splits off in: as the daemon does, compiles the
// rest with items and runs it on the fixture folder of the account kind,
// checking that the count-only search agrees.
func itemSearch(t *testing.T, s *cache.Store, gmail bool, q string, items map[string][]string) ([]uint32, cache.Plan) {
	t.Helper()
	n, err := query.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	scope, rest := query.Split(n)
	p, err := cache.Compile(rest, scope.In, gmail, compileNow, items)
	if err != nil {
		t.Fatalf("Compile(%q): %v", q, err)
	}
	account, src := compileSource(gmail)
	uids, _, err := s.SearchUIDs(t.Context(), account, src, p.Where, 0, false)
	if err != nil {
		t.Fatalf("SearchUIDs(%q): %v\nwhere %+v", q, err, p.Where)
	}
	_, count, err := s.SearchUIDs(t.Context(), account, src, p.Where, 0, true)
	if err != nil {
		t.Fatalf("SearchUIDs(%q, count only): %v", q, err)
	}
	if int(count) != len(uids) {
		t.Errorf("%q: count %d, but %d UIDs", q, count, len(uids))
	}
	return uids, p
}

// TestCompileItem checks that item:<id> matches exactly the rows whose
// thread key is listed, item:none the rows whose key is not (rows with no
// thread included), that both combine with other operators, and that the
// plan is one the cache answers from headers alone.
func TestCompileItem(t *testing.T) {
	t.Parallel()
	s := openItemStore(t)
	tests := []struct {
		name  string
		gmail bool
		query string
		items map[string][]string
		want  []uint32
	}{
		{name: "listed threads", query: "item:it_a", items: map[string][]string{"it_a": {"aaaa", "cccc"}}, want: []uint32{5, 2, 1}},
		{name: "empty list", query: "item:it_a", items: map[string][]string{"it_a": {}}, want: nil},
		{name: "nil list", query: "item:it_a", items: map[string][]string{"it_a": nil}, want: nil},
		{name: "none keeps rows with no thread", query: "item:none", items: map[string][]string{"none": {"aaaa"}}, want: []uint32{5, 4, 3}},
		{name: "none with nothing linked", query: "item:none", items: map[string][]string{"none": {}}, want: []uint32{5, 4, 3, 2, 1}},
		{name: "not none", query: "-item:none", items: map[string][]string{"none": {"aaaa", "bbbb"}}, want: []uint32{3, 2, 1}},
		{name: "with from", query: "item:it_a from:ann", items: map[string][]string{"it_a": {"aaaa"}}, want: []uint32{1}},
		{name: "with is:unread", query: "item:it_a is:unread", items: map[string][]string{"it_a": {"aaaa"}}, want: []uint32{2}},
		{name: "negated id", query: "-item:it_a", items: map[string][]string{"it_a": {"aaaa"}}, want: []uint32{5, 4, 3}},
		{name: "two ids under OR", query: "item:it_a OR item:it_b", items: map[string][]string{"it_a": {"aaaa"}, "it_b": {"bbbb"}}, want: []uint32{3, 2, 1}},
		{name: "none without a sender", query: "item:none -from:ann", items: map[string][]string{"none": {"aaaa"}}, want: []uint32{5}},
		{name: "grouped", query: "(item:it_a OR from:carol) -is:read", items: map[string][]string{"it_a": {"aaaa"}}, want: []uint32{2}},
		// A key is a value, never a LIKE pattern or SQL.
		{name: "keys are literal", query: "item:it_a", items: map[string][]string{"it_a": {"%", "a_aa", "aaaa'", "' OR 1=1 --"}}, want: nil},
		{name: "gmail listed thread", gmail: true, query: "item:it_g", items: map[string][]string{"it_g": {"1a2b"}}, want: []uint32{12, 11}},
		{name: "gmail with in:inbox", gmail: true, query: "item:it_g in:inbox", items: map[string][]string{"it_g": {"1a2b"}}, want: []uint32{11}},
		{name: "gmail none", gmail: true, query: "item:none", items: map[string][]string{"none": {"1a2b"}}, want: []uint32{13}},
	}
	for _, tt := range tests {
		// The cases share s, so they run one at a time.
		t.Run(tt.name, func(t *testing.T) {
			got, p := itemSearch(t, s, tt.gmail, tt.query, tt.items)
			if !slices.Equal(got, tt.want) {
				t.Errorf("%q = %v, want %v\nwhere %+v", tt.query, got, tt.want, p.Where)
			}
			if p.NeedsBodies || p.ServerOnly || p.Unsupported || p.Reason != "" {
				t.Errorf("%q: plan %+v, want one the cache answers from headers", tt.query, p)
			}
		})
	}
}

// TestCompileItemArgs pins the SQL shape: one JSON array argument per
// item: term, [] for an empty list, IN for an ID and NOT IN for none.
func TestCompileItemArgs(t *testing.T) {
	t.Parallel()
	n := query.Or(query.Term(query.FieldItem, "it_a"), query.Term(query.FieldItem, query.ItemNone))
	p, err := cache.Compile(n, "", false, compileNow, map[string][]string{"it_a": {"aaaa", "bbbb"}, "none": nil})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{`["aaaa","bbbb"]`, `[]`}
	if !slices.Equal(p.Where.Args, want) {
		t.Errorf("args = %v, want %v", p.Where.Args, want)
	}
	if strings.Count(p.Where.SQL, "?") != 2 ||
		!strings.Contains(p.Where.SQL, "m.thread_id IN (SELECT value FROM json_each(?))") ||
		!strings.Contains(p.Where.SQL, "m.thread_id NOT IN (SELECT value FROM json_each(?))") {
		t.Errorf("SQL = %q, want an IN and a NOT IN over json_each", p.Where.SQL)
	}
}

// TestCompileItemUnresolved checks that an item: value with no entry in the
// map is an error, never a silent match, and not ErrUnsupportedQuery,
// which would read as the user's mistake: the daemon resolves every value
// before it compiles.
func TestCompileItemUnresolved(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		value string
		items map[string][]string
	}{
		{value: "it_a", items: nil},
		{value: "it_a", items: map[string][]string{}},
		{value: "it_a", items: map[string][]string{"it_b": {"aaaa"}}},
		{value: query.ItemNone, items: map[string][]string{"it_a": {"aaaa"}}},
	} {
		for _, gmail := range []bool{false, true} {
			_, err := cache.Compile(query.Term(query.FieldItem, tt.value), "", gmail, compileNow, tt.items)
			if err == nil || errors.Is(err, mailstore.ErrUnsupportedQuery) {
				t.Errorf("item:%s with %v, gmail %v: err = %v, want an internal error", tt.value, tt.items, gmail, err)
			}
		}
	}
}
