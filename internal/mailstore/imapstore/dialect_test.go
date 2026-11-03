package imapstore

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

func split(t *testing.T, s string) (*query.Node, string) {
	t.Helper()
	n, err := query.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	scope, rest := query.Split(n)
	return rest, scope.In
}

func TestCompileGmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		query, want string
	}{
		{"invoice from:acme", "invoice from:acme"},
		{"in:inbox a OR b", "in:inbox (a OR b)"},
		{"in:inbox invoice", "in:inbox invoice"},
		{"after:2026-01-31 before:2026-02-01", "after:2026/01/31 before:2026/02/01"},
		{"in:anywhere has:attachment label:receipts", "has:attachment label:receipts"},
		{"in:trash", ""},
		{"in:spam -is:read", "-is:read"},
		{"in:junk x", "x"},
		{`in:"Sent Items" report`, `in:"Sent Items" report`},
		{`subject:"q3 report" newer_than:7d`, `subject:"q3 report" newer_than:7d`},
		// in:archive is All Mail without \Inbox, as the fake models it.
		{"in:archive", "-in:inbox"},
		{"in:Archive a OR b", "-in:inbox (a OR b)"},
		// Gmail does not document escaping inside phrases. This is the
		// backslash form String prints; a live test must confirm it.
		{`"say \"hi\""`, `"say \"hi\""`},
		{"", ""},
		// Braces are Gmail's OR group, so text and values with them are
		// quoted: {a b} is the words "{a" and "b}" here, not a OR b.
		{"{a b}", `"{a" "b}"`},
		{"from:{acme} subject:x}", `from:"{acme}" subject:"x}"`},
		{"in:{x} y", `in:"{x}" y`},
		// AND and AROUND are Gmail operators, in any case.
		{"a AND b", `a "AND" b`},
		{"a and b", `a "and" b`},
		{"a AROUND b", `a "AROUND" b`},
		{"Around", `"Around"`},
		{"from:AND", "from:AND"},
		// A double negation means the term itself.
		{"--a", "a"},
		{"---a", "-a"},
		{"x --from:y", "x from:y"},
		{"-(-(a OR b)) c", "a OR b c"},
		{"in:inbox --(a OR b)", "in:inbox (a OR b)"},
		// Gmail documents d, m and y ages, in lowercase.
		{"newer_than:2w", "newer_than:14d"},
		{"older_than:9999W", "older_than:69993d"},
		{"newer_than:3M older_than:1Y", "newer_than:3m older_than:1y"},
		{"newer_than:5D", "newer_than:5d"},
		// Sizes go as bytes: K and M are KiB and MiB here.
		{"larger:5M smaller:10k", "larger:5242880 smaller:10240"},
		{"larger:100", "larger:100"},
		{"-(larger:1K OR smaller:2m)", "-(larger:1024 OR smaller:2097152)"},
	}
	for _, tt := range tests {
		rest, in := split(t, tt.query)
		before := rest.String()
		if got := compileGmail(rest, in); got != tt.want {
			t.Errorf("compileGmail(%q) = %q, want %q", tt.query, got, tt.want)
		}
		if rest.String() != before {
			t.Errorf("compileGmail changed its input tree: %q", rest.String())
		}
	}
	if c := gmailCriteria(""); !reflect.DeepEqual(c, &imap.SearchCriteria{}) {
		t.Errorf("gmailCriteria(\"\") = %+v, want match-all", c)
	}
	if c := gmailCriteria("x"); !reflect.DeepEqual(c.GmailRaw, []string{"x"}) {
		t.Errorf("gmailCriteria(x) = %+v", c)
	}
}

func TestCompileStandard(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 15, 4, 5, 0, time.UTC)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	text := func(s string) imap.SearchCriteria { return imap.SearchCriteria{Text: []string{s}} }
	nothing := imap.SearchCriteria{Not: []imap.SearchCriteria{{}}}
	tests := []struct {
		query string
		want  *imap.SearchCriteria
	}{
		{"", &imap.SearchCriteria{}},
		{"invoice", &imap.SearchCriteria{Text: []string{"invoice"}}},
		{"from:acme subject:bill", &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: "acme"}, {Key: "Subject", Value: "bill"}}}},
		{"to:a cc:b bcc:c", &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "To", Value: "a"}, {Key: "Cc", Value: "b"}, {Key: "Bcc", Value: "c"}}}},
		{"is:unread", &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}},
		{"is:read", &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}},
		{"is:starred", &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}},
		{"newer_than:1d", &imap.SearchCriteria{Since: day(2026, 10, 28)}},
		{"older_than:2w", &imap.SearchCriteria{Before: day(2026, 10, 15)}},
		{"newer_than:1m", &imap.SearchCriteria{Since: day(2026, 9, 29)}},
		{"newer_than:1y", &imap.SearchCriteria{Since: day(2025, 10, 29)}},
		{"after:2026-01-31 before:2026-02-01", &imap.SearchCriteria{Since: day(2026, 1, 31), Before: day(2026, 2, 1)}},
		{"newer_than:7d after:2026-10-25", &imap.SearchCriteria{Since: day(2026, 10, 25)}},
		{"larger:5M smaller:10M", &imap.SearchCriteria{Larger: 5 << 20, Smaller: 10 << 20}},
		{"smaller:10M larger:5M smaller:1M", &imap.SearchCriteria{Larger: 5 << 20, Smaller: 1 << 20}},
		{"a OR b", &imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{text("a"), text("b")}}}},
		{"a OR b OR c", &imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{{Or: [][2]imap.SearchCriteria{{text("a"), text("b")}}}, text("c")}}}},
		{"-a", &imap.SearchCriteria{Not: []imap.SearchCriteria{text("a")}}},
		{"x -(a OR b)", &imap.SearchCriteria{Text: []string{"x"}, Not: []imap.SearchCriteria{{Or: [][2]imap.SearchCriteria{{text("a"), text("b")}}}}}},
		// The largest sizes a 32-bit IMAP number holds.
		{"larger:4294967295 smaller:4095M", &imap.SearchCriteria{Larger: 4294967295, Smaller: 4095 << 20}},
		// Zero is go-imap's "no bound", so smaller:0, which no message
		// matches, becomes NOT ALL.
		{"smaller:0", &nothing},
		{"a OR smaller:0", &imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{text("a"), nothing}}}},
		// The earliest date allowed, and an age that reaches just past it.
		{"after:1970-01-01", &imap.SearchCriteria{Since: day(1970, 1, 1)}},
		{"newer_than:56y", &imap.SearchCriteria{Since: day(1970, 10, 29)}},
	}
	for _, tt := range tests {
		rest, _ := split(t, tt.query)
		got, err := compileStandard(rest, now)
		if err != nil {
			t.Errorf("compileStandard(%q): %v", tt.query, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("compileStandard(%q) = %+v, want %+v", tt.query, got, tt.want)
		}
	}
}

func TestCompileStandardRefuses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		query string
		// detail is part of the error message.
		detail string
	}{
		{"has:attachment", "has:"},
		{"filename:pdf", "filename:"},
		{"label:x", "label:"},
		{"a OR has:attachment", "has:"},
		{"-label:x", "label:"},
		// RFC 3501 sizes are 32-bit numbers.
		{"larger:4294967296", "4294967295"},
		{"smaller:4096M", "4294967295"},
		{"larger:999999999999M", "4294967295"},
		{"a OR -larger:5000M", "4294967295"},
		// Dates before 1970 are refused rather than sent as nonsense.
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
		rest, _ := split(t, tt.query)
		_, err := compileStandard(rest, now)
		if !errors.Is(err, mailstore.ErrUnsupportedQuery) {
			t.Errorf("compileStandard(%q) = %v, want ErrUnsupportedQuery", tt.query, err)
			continue
		}
		if !strings.Contains(err.Error(), tt.detail) {
			t.Errorf("compileStandard(%q) = %q, want it to mention %q", tt.query, err, tt.detail)
		}
	}
}

// TestCompileStandardBadValues covers trees that skipped validation: every
// error is ErrUnsupportedQuery, never an internal error.
func TestCompileStandardBadValues(t *testing.T) {
	t.Parallel()
	for _, n := range []*query.Node{
		query.Term(query.FieldIs, "important"),
		query.Term(query.FieldNewerThan, "7x"),
		query.Term(query.FieldOlderThan, "0d"),
		query.Term(query.FieldAfter, "yesterday"),
		query.Term(query.FieldBefore, "2026-13-01"),
		query.Term(query.FieldLarger, "big"),
		query.Term(query.FieldSmaller, "5G"),
		query.Or(query.Term(query.FieldText, "a"), query.Not(query.Term(query.FieldIs, "x"))),
	} {
		if _, err := compileStandard(n, time.Now()); !errors.Is(err, mailstore.ErrUnsupportedQuery) {
			t.Errorf("compileStandard(%s) = %v, want ErrUnsupportedQuery", n, err)
		}
	}
}

func TestIntersect(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	var uids imap.UIDSet
	uids.AddRange(1, 9)
	dst := &imap.SearchCriteria{Smaller: 10, Since: day(1), SentBefore: day(20), GmailMsgID: []uint64{1}}
	intersect(dst, &imap.SearchCriteria{
		Larger: 5, Since: day(3), Before: day(9), SentSince: day(2), SentBefore: day(10),
		UID: []imap.UIDSet{uids}, GmailMsgID: []uint64{2}, GmailThreadID: []uint64{3},
	})
	intersect(dst, &imap.SearchCriteria{Smaller: 20, Larger: 2, Since: day(2), SentSince: day(4)})
	want := &imap.SearchCriteria{
		Larger: 5, Smaller: 10, Since: day(3), Before: day(9), SentSince: day(4), SentBefore: day(10),
		UID: []imap.UIDSet{uids}, GmailMsgID: []uint64{1, 2}, GmailThreadID: []uint64{3},
	}
	if !reflect.DeepEqual(dst, want) {
		t.Errorf("intersect = %+v, want %+v", dst, want)
	}

	// Only one MODSEQ key fits, so a second one is kept as NOT NOT.
	m1, m2 := &imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 1}}, &imap.SearchCriteriaModSeq{ModSeq: 2}
	got := &imap.SearchCriteria{}
	intersect(got, m1)
	intersect(got, &imap.SearchCriteria{ModSeq: m2})
	wantModSeq := &imap.SearchCriteria{ModSeq: m1.ModSeq, Not: []imap.SearchCriteria{{Not: []imap.SearchCriteria{{ModSeq: m2}}}}}
	if !reflect.DeepEqual(got, wantModSeq) {
		t.Errorf("intersect(modseq) = %+v, want %+v", got, wantModSeq)
	}
}

func TestAssignRoles(t *testing.T) {
	t.Parallel()
	attrs := func(a ...imap.MailboxAttr) []imap.MailboxAttr { return a }
	list := []*imap.ListData{
		{Mailbox: "INBOX"},
		{Mailbox: "[Gmail]", Attrs: attrs(imap.MailboxAttrNoSelect)},
		{Mailbox: "[Gmail]/All Mail", Attrs: attrs(imap.MailboxAttrAll)},
		{Mailbox: "[Gmail]/Spam", Attrs: attrs(imap.MailboxAttrJunk)},
		{Mailbox: "Sent Items", Delim: '/'},
		{Mailbox: "Junk"},
		{Mailbox: "Work/Trash", Delim: '/'},
		{Mailbox: "Deleted Items"},
		{Mailbox: "Archive", Attrs: attrs(imap.MailboxAttrArchive)},
		{Mailbox: "Drafts", Attrs: attrs(imap.MailboxAttrHasNoChildren, imap.MailboxAttrDrafts)},
	}
	want := []mailstore.Role{
		mailstore.RoleInbox, mailstore.RoleNone, mailstore.RoleAll, mailstore.RoleSpam, mailstore.RoleSent,
		mailstore.RoleNone, mailstore.RoleTrash, mailstore.RoleNone, mailstore.RoleArchive, mailstore.RoleDrafts,
	}
	if got := assignRoles(list); !slices.Equal(got, want) {
		t.Errorf("assignRoles = %v, want %v", got, want)
	}

	// A folder that cannot be selected does not take a role by its name,
	// so the selectable one after it does.
	list = []*imap.ListData{
		{Mailbox: "Trash", Delim: '.', Attrs: attrs(imap.MailboxAttrNoSelect, imap.MailboxAttrHasChildren)},
		{Mailbox: "Gone/Sent", Delim: '/', Attrs: attrs(imap.MailboxAttrNonExistent)},
		{Mailbox: "Old.Trash", Delim: '.'},
		{Mailbox: "Sent"},
	}
	want = []mailstore.Role{mailstore.RoleNone, mailstore.RoleNone, mailstore.RoleTrash, mailstore.RoleSent}
	if got := assignRoles(list); !slices.Equal(got, want) {
		t.Errorf("assignRoles(unselectable first) = %v, want %v", got, want)
	}
}

func TestSources(t *testing.T) {
	t.Parallel()
	folders := []mailstore.Folder{
		{Name: "INBOX", Role: mailstore.RoleInbox, Selectable: true},
		{Name: "Sent", Role: mailstore.RoleSent, Selectable: true},
		{Name: "Trash", Role: mailstore.RoleTrash, Selectable: true},
		{Name: "Junk", Role: mailstore.RoleSpam, Selectable: true},
		{Name: "Projects", Role: mailstore.RoleNone, Selectable: true},
		{Name: "[Gmail]", Role: mailstore.RoleNone},
		{Name: "[Gmail]/All Mail", Role: mailstore.RoleAll, Selectable: true},
	}
	standard := map[string][]string{
		// A \All folder, such as a Dovecot virtual one, holds a copy of
		// every message, so it is searched only when in: names it.
		"":                 {"INBOX", "Sent", "Projects"},
		"anywhere":         {"INBOX", "Sent", "Trash", "Junk", "Projects"},
		"[Gmail]/All Mail": {"[Gmail]/All Mail"},
		"trash":            {"Trash"},
		"junk":             {"Junk"},
		"spam":             {"Junk"},
		"INBOX":            {"INBOX"},
		"Projects":         {"Projects"},
	}
	for in, want := range standard {
		got, err := standardSources(folders, in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("standardSources(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"nope", "drafts", "[Gmail]"} {
		if _, err := standardSources(folders, in); !errors.Is(err, mailstore.ErrNotFound) {
			t.Errorf("standardSources(%q) = %v, want ErrNotFound", in, err)
		}
	}
	gmail := map[string][]string{
		"":         {"[Gmail]/All Mail"},
		"inbox":    {"[Gmail]/All Mail"},
		"archive":  {"[Gmail]/All Mail"},
		"trash":    {"Trash"},
		"spam":     {"Junk"},
		"anywhere": {"[Gmail]/All Mail", "Trash", "Junk"},
	}
	for in, want := range gmail {
		got, err := gmailSources(folders, in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("gmailSources(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := gmailSources(folders[:5], ""); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("gmailSources without All Mail = %v, want ErrNotFound", err)
	}
	if got := byRole(folders, mailstore.RoleAll); got != "[Gmail]/All Mail" {
		t.Errorf("byRole(all) = %q", got)
	}
	if got := byRole(folders, mailstore.RoleDrafts); got != "" {
		t.Errorf("byRole(drafts) = %q, want empty", got)
	}
}
