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

func TestAgeStart(t *testing.T) {
	t.Parallel()
	// The largest ages the parser accepts do not overflow, and the day is
	// taken in now's own zone.
	now := time.Date(2026, 10, 29, 23, 30, 0, 0, time.FixedZone("EST", -5*3600))
	tests := map[string]time.Time{
		"1d":    time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		"9999d": time.Date(1999, 6, 14, 0, 0, 0, 0, time.UTC),
		"9999w": time.Date(1835, 3, 12, 0, 0, 0, 0, time.UTC),
		"9999m": time.Date(1193, 7, 29, 0, 0, 0, 0, time.UTC),
		"9999y": time.Date(-7973, 10, 29, 0, 0, 0, 0, time.UTC),
	}
	for v, want := range tests {
		if got := ageStart(now, v); !got.Equal(want) {
			t.Errorf("ageStart(%s) = %v, want %v", v, got, want)
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
		"":         {"INBOX", "Sent", "Projects", "[Gmail]/All Mail"},
		"anywhere": {"INBOX", "Sent", "Trash", "Junk", "Projects", "[Gmail]/All Mail"},
		"trash":    {"Trash"},
		"junk":     {"Junk"},
		"spam":     {"Junk"},
		"INBOX":    {"INBOX"},
		"Projects": {"Projects"},
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
