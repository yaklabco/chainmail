package cache_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

// compileNow is the clock the compile tests run at, the one imapstore's
// dialect tests use, so their date cases carry over unchanged.
var compileNow = time.Date(2026, 10, 29, 15, 4, 5, 0, time.UTC)

// The fixture's two folders: a standard account's INBOX and a Google
// account's All Mail.
const (
	compileStdAccount    = "work"
	compileStdFolder     = "INBOX"
	compileStdValidity   = 7
	compileGmailAccount  = "gm"
	compileGmailFolder   = "[Gmail]/All Mail"
	compileGmailValidity = 9
)

// compileRow is one fixture message and the text of its cached body, ""
// when the cache holds none.
type compileRow struct {
	row  cache.Row
	body string
}

// compileFixture returns the rows every compile test searches. UIDs 1 to 5
// are the standard account's, 11 to 15 the Google account's.
func compileFixture() []compileRow {
	at := func(y int, m time.Month, d, hh, mm, ss int) time.Time {
		return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
	}
	std := func(uid uint32, when time.Time, subject string, size int64) cache.Row {
		return cache.Row{
			Account: compileStdAccount, Folder: compileStdFolder, UIDValidity: compileStdValidity, UID: uid,
			InternalDate: when, Date: when, Subject: subject, Size: size,
		}
	}
	gm := func(uid uint32, subject string, labels, flags []string) cache.Row {
		when := at(2026, 10, 29, 10, 0, 0)
		return cache.Row{
			Account: compileGmailAccount, Folder: compileGmailFolder, UIDValidity: compileGmailValidity, UID: uid,
			InternalDate: when, Date: when, Subject: subject, Size: 1000,
			From:   []mailstore.Address{{Name: "Gail", Email: "gail@gmail.test"}},
			Labels: labels, Flags: flags,
		}
	}

	// On the newer_than:1d boundary: midnight UTC of 28 October.
	r1 := std(1, at(2026, 10, 28, 0, 0, 0), "Invoice 42 for October", 2048)
	r1.From = []mailstore.Address{{Name: "Ann Lee", Email: "ann@acme.test"}}
	r1.To = []mailstore.Address{{Name: "Bob", Email: "bob@example.com"}}
	r1.Cc = []mailstore.Address{{Name: "Carol", Email: "carol@example.com"}}
	r1.Flags = []string{`\Seen`}
	r1.HasAttachment = true
	r1.Attachments = []mailstore.Attachment{{Part: "2", Filename: "invoice-42.pdf", ContentType: "application/pdf", Size: 1000}}

	// One second before that boundary.
	r2 := std(2, at(2026, 10, 27, 23, 59, 59), "Lunch?", 512)
	r2.From = []mailstore.Address{{Name: "Dee", Email: "dee@example.org"}}
	r2.To = []mailstore.Address{{Email: "ann@acme.test"}}
	r2.Bcc = []mailstore.Address{{Name: "Eve", Email: "eve@secret.test"}}
	r2.Flags = []string{`\Flagged`, "$Work"}

	// LIKE's wildcards and escape character in a name and the subject.
	r3 := std(3, at(2026, 1, 31, 0, 0, 0), `50% off_now \o/`, 5<<20)
	r3.From = []mailstore.Address{{Name: "100% Sure_Bot", Email: "bot@percent.test"}}
	r3.Flags = []string{"Receipts"}

	r4 := std(4, at(2026, 2, 1, 0, 0, 0), "Re: Invoice 42 for October", 10<<20)
	r4.From = []mailstore.Address{{Name: "Frank", Email: "frank@acme.test"}}
	r4.Flags = []string{`\Seen`, `\Answered`}
	r4.HasAttachment = true
	r4.Attachments = []mailstore.Attachment{{Part: "2", Filename: "Report Q3.XLSX", ContentType: "application/vnd.ms-excel", Size: 4000}}

	// Exactly ageStart(newer_than:9999d) from compileNow.
	r5 := std(5, at(1999, 6, 14, 0, 0, 0), "old news: yo/yo", 100)
	r5.From = []mailstore.Address{{Name: "Gina", Email: "gina@example.com"}}

	return []compileRow{
		{row: r1, body: "Please pay the invoice by Friday."},
		{row: r2, body: "café at noon"},
		{row: r3},
		{row: r4, body: "the q3 report is attached"},
		{row: r5},
		{row: gm(11, "Inbox mail", []string{`\Inbox`, `\Important`}, []string{`\Seen`})},
		{row: gm(12, "Archived receipt", []string{"Receipts"}, []string{`\Flagged`})},
		{row: gm(13, "Sent one", []string{`\Sent`}, []string{`\Seen`})},
		{row: gm(14, "Draft", []string{`\Draft`}, []string{`\Draft`})},
		{row: gm(15, "Client note", []string{"Work/Clients", `\Inbox`}, nil)},
	}
}

// compileMIME is a one-part text message, the shape of Message.Raw.
func compileMIME(subject, body string) []byte {
	return []byte("Subject: " + subject + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

// openCompileStore opens a cache in the test's temp dir and seeds it with
// the fixture rows and bodies.
func openCompileStore(tb testing.TB) *cache.Store {
	tb.Helper()
	s, err := cache.Open(tb.Context(), filepath.Join(tb.TempDir(), "cache.db"), cache.OpenOptions{})
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	fixture := compileFixture()
	rows := make([]cache.Row, 0, len(fixture))
	for _, r := range fixture {
		rows = append(rows, r.row)
	}
	if err := s.PutRows(tb.Context(), rows); err != nil {
		tb.Fatalf("PutRows: %v", err)
	}
	for _, r := range fixture {
		if r.body == "" {
			continue
		}
		ok, err := s.PutBody(tb.Context(), r.row.Ref(), cache.Body{Raw: compileMIME(r.row.Subject, r.body)}, false)
		if err != nil || !ok {
			tb.Fatalf("PutBody(UID %d) = %v, %v", r.row.UID, ok, err)
		}
	}
	return s
}

// compileSource is the fixture folder of the account kind.
func compileSource(gmail bool) (string, mailstore.Source) {
	if gmail {
		return compileGmailAccount, mailstore.Source{Folder: compileGmailFolder, UIDValidity: compileGmailValidity}
	}
	return compileStdAccount, mailstore.Source{Folder: compileStdFolder, UIDValidity: compileStdValidity}
}

// compileSearch parses q, splits off in: as the daemon does, compiles the
// rest and runs it on the fixture folder. It returns the UIDs, highest
// first, and the plan.
func compileSearch(t *testing.T, s *cache.Store, gmail bool, q string) ([]uint32, cache.Plan) {
	t.Helper()
	n, err := query.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	scope, rest := query.Split(n)
	return compileSearchTree(t, s, gmail, rest, scope.In)
}

// compileSearchTree compiles n with in and runs it, and checks that the
// count-only search of the same plan agrees with the UIDs.
func compileSearchTree(t *testing.T, s *cache.Store, gmail bool, n *query.Node, in string) ([]uint32, cache.Plan) {
	t.Helper()
	p, err := cache.Compile(n, in, gmail, compileNow)
	if err != nil {
		t.Fatalf("Compile(%s, in %q): %v", n, in, err)
	}
	account, src := compileSource(gmail)
	uids, _, err := s.SearchUIDs(t.Context(), account, src, p.Where, 0, false)
	if err != nil {
		t.Fatalf("SearchUIDs(%s): %v\nwhere %+v", n, err, p.Where)
	}
	_, count, err := s.SearchUIDs(t.Context(), account, src, p.Where, 0, true)
	if err != nil {
		t.Fatalf("SearchUIDs(%s, count only): %v", n, err)
	}
	if int(count) != len(uids) {
		t.Errorf("%s: count %d, but %d UIDs", n, count, len(uids))
	}
	return uids, p
}

// compileCase is one query and the fixture UIDs it matches, highest first.
type compileCase struct {
	query string
	want  []uint32
}

// runCompileCases runs each case on the fixture of the account kind and
// expects the cache to be able to answer it.
func runCompileCases(t *testing.T, s *cache.Store, gmail bool, tests []compileCase) {
	t.Helper()
	for _, tt := range tests {
		got, p := compileSearch(t, s, gmail, tt.query)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q = %v, want %v\nwhere %+v", tt.query, got, tt.want, p.Where)
		}
		if p.ServerOnly {
			t.Errorf("%q: ServerOnly (%q), want the cache to answer it", tt.query, p.Reason)
		}
	}
}

// TestCompileHeaderOperators checks from, to, cc, bcc and subject as
// case-insensitive substrings of their role, LIKE's wildcards and escape
// character taken literally, and AND, OR and NOT around them.
func TestCompileHeaderOperators(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	runCompileCases(t, s, false, []compileCase{
		{"", []uint32{5, 4, 3, 2, 1}},
		{"from:acme", []uint32{4, 1}},
		{"from:ANN", []uint32{1}},
		{`from:"ann lee"`, []uint32{1}},
		{"from:example", []uint32{5, 2}},
		{"to:acme", []uint32{2}},
		{"to:example", []uint32{1}},
		{"cc:carol", []uint32{1}},
		{"bcc:eve", []uint32{2}},
		{"bcc:carol", nil},
		{"subject:invoice", []uint32{4, 1}},
		{`subject:"re: invoice"`, []uint32{4}},
		{"subject:LUNCH", []uint32{2}},
		// Unescaped, % and _ would match every row and \o would match "o/".
		{"from:%", []uint32{3}},
		{"from:_", []uint32{3}},
		{"subject:_", []uint32{3}},
		{`subject:\o/`, []uint32{3}},
		{"-from:acme", []uint32{5, 3, 2}},
		{"from:acme OR from:dee", []uint32{4, 2, 1}},
		{"-(from:acme OR is:starred)", []uint32{5, 3}},
		{"from:acme -subject:re", []uint32{1}},
	})
}

// TestCompileFlagOperators checks is:, label: (IMAP keywords here, never
// system flags), has:attachment and filename: on a standard account, where
// the cache answers what the server path refuses.
func TestCompileFlagOperators(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	runCompileCases(t, s, false, []compileCase{
		{"is:unread", []uint32{5, 3, 2}},
		{"is:UNREAD", []uint32{5, 3, 2}},
		{"is:read", []uint32{4, 1}},
		{"is:starred", []uint32{2}},
		{"label:$work", []uint32{2}},
		{"label:work", nil},
		{"label:receipts", []uint32{3}},
		{"label:RECEIPTS", []uint32{3}},
		{"has:attachment", []uint32{4, 1}},
		{"has:ATTACHMENT", []uint32{4, 1}},
		{"-has:attachment", []uint32{5, 3, 2}},
		{"filename:pdf", []uint32{1}},
		{`filename:"report q3"`, []uint32{4}},
		{"filename:XLSX", []uint32{4}},
		{"filename:_", nil},
	})

	// A system flag is not a label: the server path refuses label: here,
	// so the plan is server-only and matches nothing if forced.
	got, p := compileSearch(t, s, false, `label:\Seen`)
	if len(got) != 0 || !p.ServerOnly || p.Reason != "" {
		t.Errorf(`label:\Seen = %v, ServerOnly %v, Reason %q; want none, true, ""`, got, p.ServerOnly, p.Reason)
	}
}

// TestCompileNil checks that a nil tree, what the daemon's split leaves
// when the query held only scope terms, compiles on both dialects to the
// zero Plan, which matches every row of the folder.
func TestCompileNil(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	for _, gmail := range []bool{false, true} {
		p, err := cache.Compile(nil, "", gmail, compileNow)
		if err != nil {
			t.Fatalf("Compile(nil, gmail %v): %v", gmail, err)
		}
		if p.Where.SQL != "" || p.Where.Args != nil || p.Where.FTS != "" || p.NeedsBodies || !p.Since.IsZero() || p.ServerOnly || p.Reason != "" {
			t.Errorf("Compile(nil, gmail %v) = %+v, want the zero Plan", gmail, p)
		}
		want := []uint32{5, 4, 3, 2, 1}
		if gmail {
			want = []uint32{15, 14, 13, 12, 11}
		}
		if got, _ := compileSearchTree(t, s, gmail, nil, ""); !slices.Equal(got, want) {
			t.Errorf("Compile(nil, gmail %v) matches %v, want %v", gmail, got, want)
		}
	}
}
