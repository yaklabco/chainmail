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

	// Exactly query.AgeStart(newer_than:9999d) from compileNow.
	r5 := std(5, at(1999, 6, 14, 0, 0, 0), "old news: yo/yo", 100)
	r5.From = []mailstore.Address{{Name: "Gina", Email: "gina@example.com"}}

	return []compileRow{
		{row: r1, body: "Please pay the invoice by Friday."},
		{row: r2, body: "café at noon"},
		{row: r3},
		{row: r4, body: "the q3 report is attached"},
		{row: r5},
		{row: gm(11, "Inbox mail", []string{`\Inbox`, `\Important`}, []string{`\Seen`})},
		{row: gm(12, "Archived receipt", []string{"Receipts", "My Label"}, []string{`\Flagged`})},
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
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})
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
		// The cases share s, so they run one at a time.
		t.Run(tt.query, func(t *testing.T) {
			got, p := compileSearch(t, s, gmail, tt.query)
			if !slices.Equal(got, tt.want) {
				t.Errorf("%q = %v, want %v\nwhere %+v", tt.query, got, tt.want, p.Where)
			}
			if p.ServerOnly {
				t.Errorf("%q: ServerOnly (%q), want the cache to answer it", tt.query, p.Reason)
			}
		})
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

// TestCompileGmail checks in:, label: and is: on a Google account, where
// every message lives once in All Mail and in: names a label, and that
// in:trash, in:spam, in:junk and in:anywhere, whose folders the cache does
// not hold, are server-only.
func TestCompileGmail(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	runCompileCases(t, s, true, []compileCase{
		{"", []uint32{15, 14, 13, 12, 11}},
		{"in:inbox", []uint32{15, 11}},
		{"in:INBOX", []uint32{15, 11}},
		{"in:archive", []uint32{14, 13, 12}},
		{"in:sent", []uint32{13}},
		{"in:drafts", []uint32{14}},
		{"in:starred", []uint32{12}},
		{"in:important", []uint32{11}},
		{"in:receipts", []uint32{12}},
		{"in:Receipts", []uint32{12}},
		{"in:work/clients", []uint32{15}},
		{"label:receipts", []uint32{12}},
		{"label:work", nil},
		{"label:work/clients", []uint32{15}},
		{`label:\Inbox`, []uint32{15, 11}},
		{"is:unread", []uint32{15, 14, 12}},
		{"is:starred", []uint32{12}},
		{"in:inbox is:unread", []uint32{15}},
		{"in:archive label:receipts", []uint32{12}},
	})

	// is:important passes no validation, but a direct caller gets the label.
	got, p := compileSearchTree(t, s, true, query.Term(query.FieldIs, "important"), "")
	if !slices.Equal(got, []uint32{11}) || p.ServerOnly {
		t.Errorf("is:important on Google = %v, ServerOnly %v; want [11], false", got, p.ServerOnly)
	}
	_, p = compileSearchTree(t, s, false, query.Term(query.FieldIs, "important"), "")
	if !p.ServerOnly {
		t.Error("is:important off Google is not ServerOnly")
	}

	for _, in := range []string{"trash", "spam", "junk", "anywhere", "Trash"} {
		got, p := compileSearch(t, s, true, "in:"+in)
		if !slices.Equal(got, []uint32{15, 14, 13, 12, 11}) {
			t.Errorf("in:%s = %v, want every All Mail row", in, got)
		}
		if !p.ServerOnly || p.Reason != "folder_not_synced" {
			t.Errorf("in:%s: ServerOnly %v, Reason %q; want true, folder_not_synced", in, p.ServerOnly, p.Reason)
		}
	}

	// Off Google in: is ignored here: SearchFolders picks the folders.
	got, p = compileSearch(t, s, false, "in:trash")
	if !slices.Equal(got, []uint32{5, 4, 3, 2, 1}) || p.ServerOnly {
		t.Errorf("in:trash off Google = %v, ServerOnly %v; want every row, false", got, p.ServerOnly)
	}
}

// TestCompileGmailLabels checks that label: matches labels the way Gmail
// search does: the system names mean the system labels, as in: reads them,
// and any other label matches with case, spaces and slashes folded, so
// label:work-clients finds "Work/Clients". A label is one whole element,
// never a substring.
func TestCompileGmailLabels(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	for _, tt := range []compileCase{
		{"inbox", []uint32{15, 11}},
		{"INBOX", []uint32{15, 11}},
		{"sent", []uint32{13}},
		{"draft", []uint32{14}},
		{"drafts", []uint32{14}},
		{"starred", []uint32{12}},
		{"important", []uint32{11}},
		{"work-clients", []uint32{15}},
		{"Work/Clients", []uint32{15}},
		{"my-label", []uint32{12}},
	} {
		label, lp := compileSearch(t, s, true, "label:"+tt.query)
		in, ip := compileSearch(t, s, true, "in:"+tt.query)
		if !slices.Equal(label, tt.want) || lp.ServerOnly {
			t.Errorf("label:%s = %v, ServerOnly %v; want %v, false\nwhere %+v", tt.query, label, lp.ServerOnly, tt.want, lp.Where)
		}
		if !slices.Equal(in, tt.want) || ip.ServerOnly {
			t.Errorf("in:%s = %v, ServerOnly %v; want %v, false\nwhere %+v", tt.query, in, ip.ServerOnly, tt.want, ip.Where)
		}
	}
	runCompileCases(t, s, true, []compileCase{
		{`label:"My Label"`, []uint32{12}},
		{"label:work", nil},
		{"label:clients", nil},
		{"in:work", nil},
	})

	for _, label := range []string{"trash", "spam", "Trash", "junk", "anywhere"} {
		_, p := compileSearch(t, s, true, "label:"+label)
		if !p.ServerOnly || p.Reason != "folder_not_synced" {
			t.Errorf("label:%s: ServerOnly %v, Reason %q; want true, folder_not_synced", label, p.ServerOnly, p.Reason)
		}
		_, p = compileSearch(t, s, true, "from:gail OR label:"+label)
		if !p.ServerOnly || p.Reason != "folder_not_synced" {
			t.Errorf("from:gail OR label:%s: ServerOnly %v, Reason %q; want true, folder_not_synced", label, p.ServerOnly, p.Reason)
		}
	}

	// IMAP keywords keep their exact names: no folding off Google.
	runCompileCases(t, s, false, []compileCase{
		{"label:receipts", []uint32{3}},
		{"label:inbox", nil},
	})
}

// TestCompileDatesAndSizes checks the day boundaries and size bounds of
// imapstore's compileStandard: SINCE is on or after the day, BEFORE is
// strictly before it, LARGER and SMALLER are strict, larger:0 is every
// message and smaller:0 none.
func TestCompileDatesAndSizes(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	runCompileCases(t, s, false, []compileCase{
		// compileNow is 29 October 15:04:05 UTC, so newer_than:1d is since
		// midnight UTC of 28 October: UID 1 sits on it, UID 2 a second before.
		{"newer_than:1d", []uint32{1}},
		{"newer_than:2d", []uint32{2, 1}},
		{"older_than:1d", []uint32{5, 4, 3, 2}},
		{"-newer_than:1d", []uint32{5, 4, 3, 2}},
		{"newer_than:1y", []uint32{4, 3, 2, 1}},
		{"after:2026-01-31", []uint32{4, 3, 2, 1}},
		{"after:2026-02-02", []uint32{2, 1}},
		{"before:2026-02-01", []uint32{5, 3}},
		{"after:2026-01-31 before:2026-02-01", []uint32{3}},
		{"newer_than:1d OR before:2000-01-01", []uint32{5, 1}},
		// 9999 days back is 14 June 1999, UID 5's arrival.
		{"newer_than:9999d", []uint32{5, 4, 3, 2, 1}},
		{"older_than:9999d", nil},
		// The earliest date and the longest age the standard dialect allows.
		{"after:1970-01-01", []uint32{5, 4, 3, 2, 1}},
		{"newer_than:56y", []uint32{5, 4, 3, 2, 1}},
		{"larger:5M", []uint32{4}},
		{"smaller:5M", []uint32{5, 2, 1}},
		{"larger:2K", []uint32{4, 3}},
		{"smaller:2K", []uint32{5, 2}},
		{"larger:2047 smaller:2049", []uint32{1}},
		{"larger:0", []uint32{5, 4, 3, 2, 1}},
		{"smaller:0", nil},
		{"-smaller:0", []uint32{5, 4, 3, 2, 1}},
		{"from:dee OR smaller:0", []uint32{2}},
		{"smaller:10M larger:5M smaller:1M", nil},
		// The largest sizes a 32-bit IMAP number holds.
		{"larger:4294967295 smaller:4095M", nil},
	})
}

// TestCompileAgeStart runs query.TestAgeStart's cases through Plan.Since:
// the day is taken in now's own zone and returned at midnight UTC, the
// longest ages do not overflow, and months and years clamp to the last day
// of a shorter target month. It compiles for Google, which refuses no early
// date, so the 9999 ages compile.
func TestCompileAgeStart(t *testing.T) {
	t.Parallel()
	since := func(now time.Time, age string) time.Time {
		t.Helper()
		p, err := cache.Compile(query.Term(query.FieldNewerThan, age), "", true, now)
		if err != nil {
			t.Fatalf("Compile(newer_than:%s): %v", age, err)
		}
		return p.Since
	}
	now := time.Date(2026, 10, 29, 23, 30, 0, 0, time.FixedZone("EST", -5*3600))
	tests := map[string]time.Time{
		"1d":    time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		"9999d": time.Date(1999, 6, 14, 0, 0, 0, 0, time.UTC),
		"9999w": time.Date(1835, 3, 12, 0, 0, 0, 0, time.UTC),
		"9999m": time.Date(1193, 7, 29, 0, 0, 0, 0, time.UTC),
		"9999y": time.Date(-7973, 10, 29, 0, 0, 0, 0, time.UTC),
	}
	for v, want := range tests {
		if got := since(now, v); !got.Equal(want) {
			t.Errorf("newer_than:%s Since = %v, want %v", v, got, want)
		}
	}
	ends := []struct {
		now  time.Time
		age  string
		want time.Time
	}{
		{time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC), "1m", time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC), "13m", time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC), "1M", time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, 3, 31, 9, 0, 0, 0, time.UTC), "1m", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC), "1y", time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC), "4y", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC), "1m", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range ends {
		if got := since(tt.now, tt.age); !got.Equal(tt.want) {
			t.Errorf("newer_than:%s on %s: Since = %v, want %v", tt.age, tt.now.Format(time.DateOnly), got, tt.want)
		}
	}
}

// TestCompileRefuses copies imapstore's size and date refusals: off Google
// the cache refuses exactly what compileStandard refuses, with the same
// text, so a search fails alike on either route. Google refuses none.
func TestCompileRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		query string
		// detail is part of the error message.
		detail string
	}{
		{"larger:4294967296", "4294967295"},
		{"smaller:4096M", "4294967295"},
		{"larger:999999999999M", "4294967295"},
		{"a OR -larger:5000M", "4294967295"},
		{"older_than:9999y", "1970-01-01"},
		{"newer_than:9999y", "1970-01-01"},
		{"newer_than:9999m", "1970-01-01"},
		{"older_than:57y", "1970-01-01"},
		{"before:1969-12-31", "1970-01-01"},
		{"after:0000-01-01", "1970-01-01"},
		{"after:0001-01-01", "1970-01-01"},
		{"a OR -older_than:100y", "1970-01-01"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			t.Parallel()
			n, err := query.Parse(tt.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.query, err)
			}
			_, err = cache.Compile(n, "", false, compileNow)
			if !errors.Is(err, mailstore.ErrUnsupportedQuery) {
				t.Errorf("Compile(%q) = %v, want ErrUnsupportedQuery", tt.query, err)
				return
			}
			if !strings.Contains(err.Error(), tt.detail) {
				t.Errorf("Compile(%q) = %q, want it to mention %q", tt.query, err, tt.detail)
			}
			if _, err := cache.Compile(n, "", true, compileNow); err != nil {
				t.Errorf("Compile(%q) on Google = %v, want no error", tt.query, err)
			}
		})
	}
}

// TestCompileBadValues covers trees that skipped validation. A value that
// does not parse is ErrUnsupportedQuery on both dialects; a scope term left
// in the tree, or a malformed tree, is an error; nothing panics.
func TestCompileBadValues(t *testing.T) {
	t.Parallel()
	for _, n := range []*query.Node{
		query.Term(query.FieldNewerThan, "7x"),
		query.Term(query.FieldOlderThan, "0d"),
		query.Term(query.FieldAfter, "yesterday"),
		query.Term(query.FieldBefore, "2026-13-01"),
		query.Term(query.FieldLarger, "big"),
		query.Term(query.FieldSmaller, "5G"),
		query.Or(query.Term(query.FieldText, "a"), query.Not(query.Term(query.FieldNewerThan, "x"))),
	} {
		t.Run(n.String(), func(t *testing.T) {
			t.Parallel()
			for _, gmail := range []bool{false, true} {
				if _, err := cache.Compile(n, "", gmail, compileNow); !errors.Is(err, mailstore.ErrUnsupportedQuery) {
					t.Errorf("Compile(%s, gmail %v) = %v, want ErrUnsupportedQuery", n, gmail, err)
				}
			}
		})
	}
	tests := []struct {
		name   string
		n      *query.Node
		detail string
	}{
		{"in: left in the tree", query.Term(query.FieldIn, "inbox"), "split off"},
		{"account: left in the tree", query.And(query.Term(query.FieldFrom, "a"), query.Term(query.FieldAccount, "work")), "split off"},
		{"and without children", query.And(), "malformed"},
		{"or without children", query.Or(), "malformed"},
		{"not without a child", &query.Node{Op: query.OpNot}, "malformed"},
		{"not with two children", &query.Node{Op: query.OpNot, Children: []*query.Node{query.Term(query.FieldText, "a"), query.Term(query.FieldText, "b")}}, "malformed"},
		{"nil child", query.And(query.Term(query.FieldFrom, "a"), nil), "malformed"},
		{"unknown kind", &query.Node{Op: 99}, "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, gmail := range []bool{false, true} {
				_, err := cache.Compile(tt.n, "", gmail, compileNow)
				if err == nil || !strings.Contains(err.Error(), tt.detail) {
					t.Errorf("%s (gmail %v): err = %v, want one mentioning %q", tt.name, gmail, err, tt.detail)
				}
			}
		})
	}
}

// TestCompileText checks bare words and phrases as FTS5 token matches over
// subject, names, addresses and the cached body, case and accents folded,
// with AND, OR and NOT, and mixed with header terms. Header terms of a row
// without a body stay searchable.
func TestCompileText(t *testing.T) {
	t.Parallel()
	s := openCompileStore(t)
	tests := []compileCase{
		{"newer_than:400d invoice", []uint32{4, 1}},
		{"newer_than:400d INVOICE", []uint32{4, 1}},
		{"newer_than:400d cafe", []uint32{2}},
		{"newer_than:400d café", []uint32{2}},
		{`newer_than:400d "q3 report"`, []uint32{4}},
		{`newer_than:400d "report q3"`, nil},
		{"newer_than:400d acme", []uint32{4, 2, 1}},
		{"newer_than:400d percent", []uint32{3}},
		{"newer_than:400d invoice OR noon", []uint32{4, 2, 1}},
		{"newer_than:400d noon OR friday", []uint32{2, 1}},
		{"newer_than:400d invoice friday", []uint32{1}},
		{"newer_than:400d invoice -friday", []uint32{4}},
		{"newer_than:400d -invoice", []uint32{3, 2}},
		{"newer_than:400d -(noon OR friday)", []uint32{4, 3}},
		{"newer_than:400d (invoice OR from:dee)", []uint32{4, 2, 1}},
	}
	for _, tt := range tests {
		got, p := compileSearch(t, s, false, tt.query)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q = %v, want %v\nwhere %+v", tt.query, got, tt.want, p.Where)
		}
	}

	// Without a date bound the plan is server-only, but a forced search
	// still runs and finds header terms on rows of any age.
	for _, tt := range []compileCase{
		{"gina", []uint32{5}},
		{"invoice OR from:gina", []uint32{5, 4, 1}},
		// A term with no tokens matches nothing, without an FTS5 error.
		{"!!!", nil},
	} {
		got, p := compileSearch(t, s, false, tt.query)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q = %v, want %v\nwhere %+v", tt.query, got, tt.want, p.Where)
		}
	}
	got, _ := compileSearchTree(t, s, false, query.Term(query.FieldText, `say "hi"`), "")
	if len(got) != 0 {
		t.Errorf(`text say "hi" = %v, want none`, got)
	}
	// A NUL from a caller that skipped validation would end the FTS5
	// string early; it reads as a space instead.
	got, _ = compileSearchTree(t, s, false, query.Term(query.FieldText, "a\x00b"), "")
	if len(got) != 0 {
		t.Errorf(`text "a\x00b" = %v, want none`, got)
	}
}

// TestCompileFTSExpression pins how text reaches FTS5: text alone at the
// top, or beside other terms under the top-level AND, becomes Where.FTS;
// text under OR or NOT next to other terms becomes an inline FTS subquery.
// Every ? in the SQL has its argument.
func TestCompileFTSExpression(t *testing.T) {
	t.Parallel()
	const ftsSubquery = "m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)"
	tests := []struct {
		name  string
		query string
		tree  *query.Node // used instead of query when set
		fts   string
		// sqlHas is part of Where.SQL; "" means Where.SQL must be empty.
		sqlHas string
	}{
		{name: "one word", query: "invoice", fts: `"invoice"`},
		{name: "phrase", query: `"q3 report"`, fts: `"q3 report"`},
		{name: "quote inside", tree: query.Term(query.FieldText, `say "hi"`), fts: `"say ""hi"""`},
		{name: "and", query: "a b", fts: `("a" AND "b")`},
		{name: "and not", query: "a -b -c", fts: `(("a" NOT "b") NOT "c")`},
		{name: "or", query: "a OR b", fts: `("a" OR "b")`},
		{name: "nested or", query: "a (b OR c)", fts: `("a" AND ("b" OR "c"))`},
		{name: "beside a bound", query: "newer_than:7d invoice", fts: `"invoice"`, sqlHas: "m.internal_date >= ?"},
		{name: "beside a header term", query: "from:x a", fts: `"a"`, sqlHas: "m.from_norm LIKE ?"},
		{name: "negated alone", query: "-a", sqlHas: "NOT (" + ftsSubquery + ")"},
		{name: "or with a header term", query: "from:x OR a", sqlHas: ftsSubquery},
		{name: "not under or", query: "a (b OR -c)", fts: `"a"`, sqlHas: "NOT (" + ftsSubquery + ")"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := tt.tree
			if n == nil {
				var err error
				if n, err = query.Parse(tt.query); err != nil {
					t.Fatalf("%s: Parse: %v", tt.name, err)
				}
			}
			p, err := cache.Compile(n, "", false, compileNow)
			if err != nil {
				t.Errorf("%s: Compile: %v", tt.name, err)
				return
			}
			if p.Where.FTS != tt.fts {
				t.Errorf("%s: FTS = %q, want %q", tt.name, p.Where.FTS, tt.fts)
			}
			if tt.sqlHas == "" && p.Where.SQL != "" || !strings.Contains(p.Where.SQL, tt.sqlHas) {
				t.Errorf("%s: SQL = %q, want it to contain %q", tt.name, p.Where.SQL, tt.sqlHas)
			}
			if got, want := strings.Count(p.Where.SQL, "?"), len(p.Where.Args); got != want {
				t.Errorf("%s: %d placeholders, %d args", tt.name, got, want)
			}
		})
	}
}

// TestCompileClassifier checks what the router reads from a plan: whether
// it needs bodies, its top-level lower date bound, and whether the cache
// can never answer it completely, and why.
func TestCompileClassifier(t *testing.T) {
	t.Parallel()
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }
	const (
		text   = "text_outside_window"
		folder = "folder_not_synced"
	)
	tests := []struct {
		name        string
		query       string      // parsed and split, unless tree is set
		tree        *query.Node // compiled as is, with no in:
		gmail       bool
		needsBodies bool
		since       time.Time
		serverOnly  bool
		reason      string
	}{
		{name: "nothing", query: ""},
		{name: "header terms", query: "from:acme is:unread larger:1M"},
		{name: "a bound alone", query: "newer_than:7d", since: day(10, 22)},
		{name: "text with an age bound", query: "newer_than:7d invoice", needsBodies: true, since: day(10, 22)},
		{name: "phrase with a date bound", query: `after:2026-10-01 "q3 report"`, needsBodies: true, since: day(10, 1)},
		{name: "the later of two bounds", query: "newer_than:7d after:2026-10-25 invoice", needsBodies: true, since: day(10, 25)},
		// Text with no bound reached only through ANDs is not server-only: its
		// zero Since already keeps it off the cache under automatic routing,
		// and a forced cache search answers it from the cache, incomplete.
		{name: "text without a bound", query: "invoice", needsBodies: true},
		{name: "text under OR", query: "invoice OR from:acme", needsBodies: true, serverOnly: true, reason: text},
		{name: "text under NOT", query: "-invoice", needsBodies: true, serverOnly: true, reason: text},
		{name: "only an upper bound", query: "older_than:7d invoice", needsBodies: true},
		{name: "a negated bound is no bound", query: "-newer_than:7d invoice", needsBodies: true},
		{name: "text under OR in an unbounded AND", query: "from:acme (invoice OR lunch)", needsBodies: true, serverOnly: true, reason: text},
		{name: "text under NOT in an unbounded AND", query: "from:acme -invoice", needsBodies: true, serverOnly: true, reason: text},
		{name: "text in an AND under OR without a bound", query: "(from:acme invoice) OR from:gail", needsBodies: true, serverOnly: true, reason: text},
		{name: "OR inside a bounded AND", query: "newer_than:7d (invoice OR from:acme)", needsBodies: true, since: day(10, 22)},
		{name: "NOT inside a bounded AND", query: "newer_than:7d -invoice", needsBodies: true, since: day(10, 22)},
		// The branch bounds its own text, but the top level has none, so
		// the router finds no Since inside the window.
		{name: "text bounded in its OR branch", query: "(newer_than:7d invoice) OR from:acme", needsBodies: true},
		{name: "bounded text under NOT", query: "-(newer_than:7d invoice)", needsBodies: true},
		{name: "a bound under OR is no top-level bound", query: "newer_than:7d OR from:acme"},
		{name: "system flag as a label", query: `label:\Seen`, serverOnly: true},
		{name: "system flag as a Google label", query: `label:\Seen`, gmail: true},
		{name: "has, filename and label off Google", query: "has:attachment filename:pdf label:x"},
		{name: "Google trash", query: "in:trash from:acme", gmail: true, serverOnly: true, reason: folder},
		{name: "trash off Google", query: "in:trash from:acme"},
		{name: "folder reason first", query: "in:anywhere invoice", gmail: true, needsBodies: true, serverOnly: true, reason: folder},
		{name: "Google archive with bounded text", query: "in:archive newer_than:7d invoice", gmail: true, needsBodies: true, since: day(10, 22)},
		{name: "text reason before an unsupported term", query: `(invoice OR from:acme) label:\Seen`, needsBodies: true, serverOnly: true, reason: text},
		{name: "top-level text with an unsupported term", query: `invoice label:\Seen`, needsBodies: true, serverOnly: true},
		{name: "important off Google", tree: query.Term(query.FieldIs, "important"), serverOnly: true},
		{name: "important on Google", tree: query.Term(query.FieldIs, "important"), gmail: true},
		{name: "has other than attachment", tree: query.Term(query.FieldHas, "pdf"), serverOnly: true},
		{name: "unknown field", tree: query.Term("color", "red"), serverOnly: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, in := tt.tree, ""
			if n == nil {
				parsed, err := query.Parse(tt.query)
				if err != nil {
					t.Fatalf("%s: Parse: %v", tt.name, err)
				}
				var scope query.Scope
				scope, n = query.Split(parsed)
				in = scope.In
			}
			p, err := cache.Compile(n, in, tt.gmail, compileNow)
			if err != nil {
				t.Errorf("%s: Compile: %v", tt.name, err)
				return
			}
			if p.NeedsBodies != tt.needsBodies || !p.Since.Equal(tt.since) || p.ServerOnly != tt.serverOnly || p.Reason != tt.reason {
				t.Errorf("%s: NeedsBodies %v, Since %v, ServerOnly %v, Reason %q; want %v, %v, %v, %q", tt.name,
					p.NeedsBodies, p.Since, p.ServerOnly, p.Reason, tt.needsBodies, tt.since, tt.serverOnly, tt.reason)
			}
		})
	}
}
