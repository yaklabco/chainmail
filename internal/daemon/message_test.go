package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

const fakeMailConfig = `
[daemon]
body_limit = "1KiB"

[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
`

var testNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return testNow.AddDate(0, 0, -n) }

type fakeMail struct {
	*daemontest.Harness
	host, gmail *fake.Store
}

func startFakeMail(t *testing.T) fakeMail {
	t.Helper()
	m := fakeMail{host: fake.New("host", false), gmail: fake.New("gmail", true)}
	m.Harness = daemontest.Start(t, fakeMailConfig, daemontest.Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			if a.Name == "gmail" {
				return m.gmail
			}
			return m.host
		},
		Now: func() time.Time { return testNow },
	})
	return m
}

func (m fakeMail) search(t *testing.T, req *chainmailv1.SearchRequest) (*chainmailv1.SearchResponse, error) {
	t.Helper()
	resp, err := m.Client.Message.Search(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func subjects(rows []*chainmailv1.MessageRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.GetSubject().GetUntrusted())
	}
	return out
}

func reasonOf(err error) string { return api.Detail(err).GetReason() }

func lastMailAudit(t *testing.T, m fakeMail) daemontest.AuditRow {
	t.Helper()
	rows := m.AuditRows(t)
	if len(rows) == 0 {
		t.Fatal("no audit rows")
	}
	return rows[len(rows)-1]
}

func TestListFolders(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Subject: "a"})
	resp, err := m.Client.Message.ListFolders(t.Context(), connect.NewRequest(&chainmailv1.ListFoldersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var accounts []string
	for _, a := range resp.Msg.GetAccounts() {
		accounts = append(accounts, a.GetAccount())
		if a.GetAccount() != "host" {
			continue
		}
		inbox := a.GetFolders()[0]
		if inbox.GetName() != "INBOX" || inbox.GetRole() != "inbox" || inbox.GetMessages() != 1 || inbox.GetUnseen() != 1 {
			t.Errorf("host INBOX = %v", inbox)
		}
	}
	if !slices.Equal(accounts, []string{"gmail", "host"}) {
		t.Errorf("accounts = %v", accounts)
	}
	if row := lastMailAudit(t, m); row.Action != "folders.list" || row.Account != "" || row.Targets != `["accounts=2","folders=11"]` || row.Result != "ok" {
		t.Errorf("audit = %+v", row)
	}
}

func TestListFoldersPartialAndFailures(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Fail("ListFolders", fmt.Errorf("%w: down", mailstore.ErrUnavailable))
	resp, err := m.Client.Message.ListFolders(t.Context(), connect.NewRequest(&chainmailv1.ListFoldersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.GetAccounts()) != 1 || len(resp.Msg.GetPartial()) != 1 || resp.Msg.GetPartial()[0].GetReason() != api.ReasonServerUnavailable {
		t.Errorf("partial response = %v", resp.Msg)
	}
	for accounts, want := range map[string]string{"host": api.ReasonServerUnavailable, "nope": api.ReasonNotFound, "Bad_Name": api.ReasonInvalidArgument} {
		_, err := m.Client.Message.ListFolders(t.Context(), connect.NewRequest(&chainmailv1.ListFoldersRequest{Accounts: []string{accounts}}))
		if reasonOf(err) != want {
			t.Errorf("accounts %q: %v, want %s", accounts, err, want)
		}
	}
}

func TestSearchMergesAccountsNewestFirst(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(3), Subject: "oldest"})
	m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: daysAgo(2), Subject: "middle"})
	m.host.Add(fake.Message{Folder: "Archive", Date: daysAgo(1), Subject: "newest"})
	m.host.Add(fake.Message{Folder: "Trash", Date: daysAgo(0), Subject: "trashed"})
	resp, err := m.search(t, &chainmailv1.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(resp.GetRows()); !slices.Equal(got, []string{"newest", "middle", "oldest"}) {
		t.Errorf("rows = %v", got)
	}
	if resp.GetSource() != "server" || !resp.GetFetchedAt().AsTime().Equal(testNow) || resp.GetCount() != 3 || resp.GetNextCursor() != "" {
		t.Errorf("response = source %q, fetched %v, count %d, cursor %q", resp.GetSource(), resp.GetFetchedAt().AsTime(), resp.GetCount(), resp.GetNextCursor())
	}
	var counts []string
	for _, c := range resp.GetAccountCounts() {
		counts = append(counts, fmt.Sprintf("%s=%d", c.GetAccount(), c.GetCount()))
	}
	if !slices.Equal(counts, []string{"gmail=1", "host=2"}) {
		t.Errorf("account counts = %v", counts)
	}
}

func TestSearchPagesWithStableCursors(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	// Oldest first, so UIDs rise with arrival time, as on a real server.
	for _, d := range []int{7, 5, 3, 1} {
		m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(d), Subject: fmt.Sprintf("d%d", d)})
	}
	for _, d := range []int{6, 4, 2} {
		m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: daysAgo(d), Subject: fmt.Sprintf("d%d", d)})
	}
	page1, err := m.search(t, &chainmailv1.SearchRequest{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page1.GetRows()); !slices.Equal(got, []string{"d1", "d2", "d3"}) || page1.GetCount() != 7 || page1.GetNextCursor() == "" {
		t.Fatalf("page 1 = %v, count %d, cursor %q", got, page1.GetCount(), page1.GetNextCursor())
	}
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(0), Subject: "arrived later"})
	page2, err := m.search(t, &chainmailv1.SearchRequest{Limit: 3, Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page2.GetRows()); !slices.Equal(got, []string{"d4", "d5", "d6"}) {
		t.Fatalf("page 2 = %v", got)
	}
	page3, err := m.search(t, &chainmailv1.SearchRequest{Limit: 3, Cursor: page2.GetNextCursor()})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page3.GetRows()); !slices.Equal(got, []string{"d7"}) || page3.GetNextCursor() != "" {
		t.Errorf("page 3 = %v, cursor %q", got, page3.GetNextCursor())
	}
}

func TestSearchCursorChecks(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(2), Subject: "a"})
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), Subject: "b"})
	page1, err := m.search(t, &chainmailv1.SearchRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]*chainmailv1.SearchRequest{
		"other query":    {QueryString: "x", Cursor: page1.GetNextCursor()},
		"other accounts": {Accounts: []string{"host"}, Cursor: page1.GetNextCursor()},
		"garbage":        {Cursor: "c1.zzz"},
		"count":          {CountOnly: true, Cursor: page1.GetNextCursor()},
	} {
		if _, err := m.search(t, req); reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	m.host.SetUIDValidity("INBOX", 7)
	_, err = m.search(t, &chainmailv1.SearchRequest{Limit: 1, Cursor: page1.GetNextCursor()})
	if reasonOf(err) != api.ReasonStaleRef || api.Detail(err).GetHint() != "search again" {
		t.Errorf("stale cursor = %v (hint %q), want STALE_REF", err, api.Detail(err).GetHint())
	}
}

func TestSearchPartialAndUnsupportedOperators(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Subject: "with pdf", Attachments: []fake.Attachment{{Part: "2", Filename: "a.pdf", Data: []byte("x")}}})
	resp, err := m.search(t, &chainmailv1.SearchRequest{QueryString: "has:attachment"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetRows()) != 1 || len(resp.GetPartial()) != 1 {
		t.Fatalf("rows %d, partial %v", len(resp.GetRows()), resp.GetPartial())
	}
	if p := resp.GetPartial()[0]; p.GetAccount() != "host" || p.GetFolder() != "" || p.GetReason() != api.ReasonInvalidArgument {
		t.Errorf("partial = %v", p)
	}
	for _, req := range []*chainmailv1.SearchRequest{{QueryString: "has:attachment", Accounts: []string{"host"}}, {QueryString: "has:attachment account:host"}} {
		if _, err := m.search(t, req); reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%v on host only = %v, want INVALID_ARGUMENT", req, err)
		}
	}
	m.host.Add(fake.Message{Folder: "Sent", Subject: "fine"})
	m.host.FailFolder("INBOX", fmt.Errorf("%w: folder busy", mailstore.ErrUnavailable))
	resp, err = m.search(t, &chainmailv1.SearchRequest{Accounts: []string{"host"}})
	if err != nil || len(resp.GetRows()) != 1 || len(resp.GetPartial()) != 1 || resp.GetPartial()[0].GetFolder() != "INBOX" {
		t.Errorf("folder failure = %v, %v", resp, err)
	}
}

func TestSearchEverySourceFailingUsesMostSevereReason(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Fail("Search", &auth.RequiredError{Account: "host", Hint: "run: chainmail secret set accounts/host"})
	m.gmail.Fail("Search", fmt.Errorf("%w: down", mailstore.ErrUnavailable))
	_, err := m.search(t, &chainmailv1.SearchRequest{})
	if reasonOf(err) != api.ReasonAuthRequired || !strings.Contains(api.Detail(err).GetHint(), "secret set accounts/host") {
		t.Errorf("err = %v (hint %q), want AUTH_REQUIRED with its hint", err, api.Detail(err).GetHint())
	}
	if row := lastMailAudit(t, m); row.Result != "error" || row.Reason != api.ReasonAuthRequired {
		t.Errorf("audit = %+v", row)
	}
}

func TestSearchCountOnly(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Subject: "invoice 1"})
	m.host.Add(fake.Message{Folder: "Archive", Subject: "invoice 2"})
	m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Subject: "invoice 3"})
	resp, err := m.search(t, &chainmailv1.SearchRequest{QueryString: "invoice", CountOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCount() != 3 || len(resp.GetRows()) != 0 || resp.GetNextCursor() != "" || len(resp.GetAccountCounts()) != 2 {
		t.Errorf("count response = %v", resp)
	}
	if row := lastMailAudit(t, m); row.Targets != `["accounts=2","matches=3"]` {
		t.Errorf("audit targets = %s", row.Targets)
	}
}

func TestSearchThreadsCollapse(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(4), Subject: "other", MessageID: "o@x"})
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(3), Subject: "plan", MessageID: "r@x"})
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(2), Subject: "re: plan", MessageID: "r1@x", References: []string{"r@x"}})
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), Subject: "re: re: plan", MessageID: "r2@x", References: []string{"r@x", "r1@x"}})
	resp, err := m.search(t, &chainmailv1.SearchRequest{Threads: true})
	if err != nil {
		t.Fatal(err)
	}
	rows := resp.GetRows()
	if got := subjects(rows); !slices.Equal(got, []string{"re: re: plan", "other"}) || rows[0].GetThreadCount() != 3 || rows[1].GetThreadCount() != 1 {
		t.Errorf("threads = %v, counts %d %d", got, rows[0].GetThreadCount(), rows[1].GetThreadCount())
	}
}

func TestSearchSnippetsAndUntrustedRows(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{
		Folder: "INBOX", Subject: "zebra stripes", Body: strings.Repeat("stripes ", 100),
		From: []mailstore.Address{{Name: "Private Person", Email: "pp@example.com"}},
	})
	resp, err := m.search(t, &chainmailv1.SearchRequest{QueryString: `zebra from:"Private Person"`, Snippets: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetRows()) != 1 {
		t.Fatalf("rows = %v", resp.GetRows())
	}
	if s := resp.GetRows()[0].GetSnippet().GetUntrusted(); len([]rune(s)) != 200 || !strings.HasPrefix(s, "stripes stripes") {
		t.Errorf("snippet = %q", s)
	}
	js, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `"subject":{"untrusted":"zebra stripes"}`) || !strings.Contains(string(js), `"name":{"untrusted":"Private Person"}`) {
		t.Errorf("rows are not wrapped as untrusted: %s", js)
	}
	for _, row := range m.AuditRows(t) {
		for _, secret := range []string{"zebra", "Private", "stripes", "pp@example.com"} {
			if strings.Contains(row.Targets+row.Account+row.Action, secret) {
				t.Errorf("audit row %+v contains %q", row, secret)
			}
		}
	}
	if row := lastMailAudit(t, m); row.Action != "search" || row.Targets != `["accounts=2","rows=1"]` {
		t.Errorf("audit = %+v", row)
	}
}

func TestSearchQueryErrors(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	notIn := &chainmailv1.Query{Node: &chainmailv1.Query_Not{Not: &chainmailv1.Query{Node: &chainmailv1.Query_Term{Term: &chainmailv1.QueryTerm{Field: "in", Value: "inbox"}}}}}
	tests := []struct {
		name   string
		req    *chainmailv1.SearchRequest
		reason string
	}{
		{"unknown operator", &chainmailv1.SearchRequest{QueryString: "foo:bar"}, api.ReasonInvalidArgument},
		{"negated in", &chainmailv1.SearchRequest{Query: notIn}, api.ReasonInvalidArgument},
		{"both forms", &chainmailv1.SearchRequest{QueryString: "a", Query: notIn}, api.ReasonInvalidArgument},
		{"empty tree node", &chainmailv1.SearchRequest{Query: &chainmailv1.Query{}}, api.ReasonInvalidArgument},
		{"limit", &chainmailv1.SearchRequest{Limit: 501}, api.ReasonInvalidArgument},
		{"unknown account", &chainmailv1.SearchRequest{QueryString: "account:nope"}, api.ReasonNotFound},
		{"bad account name", &chainmailv1.SearchRequest{Accounts: []string{"Bad_Name"}}, api.ReasonInvalidArgument},
	}
	for _, tt := range tests {
		_, err := m.search(t, tt.req)
		if reasonOf(err) != tt.reason {
			t.Errorf("%s: %v, want %s", tt.name, err, tt.reason)
		}
	}
	_, err := m.search(t, &chainmailv1.SearchRequest{QueryString: "foo:bar"})
	if err == nil || !strings.Contains(err.Error(), "column 1") {
		t.Errorf("parse error = %v, want the column", err)
	}
}

func TestSearchTreeMatchesString(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Subject: "bill", From: []mailstore.Address{{Email: "billing@acme.example"}}})
	m.host.Add(fake.Message{Folder: "INBOX", Subject: "other"})
	fromString, err := m.search(t, &chainmailv1.SearchRequest{QueryString: "from:acme"})
	if err != nil {
		t.Fatal(err)
	}
	tree := &chainmailv1.Query{Node: &chainmailv1.Query_Term{Term: &chainmailv1.QueryTerm{Field: "from", Value: "acme"}}}
	fromTree, err := m.search(t, &chainmailv1.SearchRequest{Query: tree})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(subjects(fromString.GetRows()), []string{"bill"}) || !slices.Equal(subjects(fromTree.GetRows()), []string{"bill"}) {
		t.Errorf("string %v, tree %v", subjects(fromString.GetRows()), subjects(fromTree.GetRows()))
	}
}

func TestSearchMergesByInternalDate(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	// The Date headers claim the opposite order to the server's arrival times.
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), InternalDate: daysAgo(5), Subject: "arrived first"})
	m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: daysAgo(5), InternalDate: daysAgo(1), Subject: "arrived last"})
	resp, err := m.search(t, &chainmailv1.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(resp.GetRows()); !slices.Equal(got, []string{"arrived last", "arrived first"}) {
		t.Errorf("rows = %v, want newest arrival first", got)
	}
}

func TestSearchRefusesForgedCursorSources(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(2), Subject: "a"})
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), Subject: "b"})
	m.host.Add(fake.Message{Folder: "Trash", Date: daysAgo(1), Subject: "binned"})
	m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: daysAgo(1), Subject: "elsewhere"})
	hostOnly := []string{"host"}
	page1, err := m.search(t, &chainmailv1.SearchRequest{Accounts: hostOnly, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	genuine, err := ref.DecodeCursor(page1.GetNextCursor())
	if err != nil {
		t.Fatal(err)
	}
	forge := func(extra ...ref.CursorSource) string {
		c := genuine
		c.Sources = append(slices.Clone(c.Sources), extra...)
		return ref.EncodeCursor(c)
	}
	inbox := slices.IndexFunc(genuine.Sources, func(s ref.CursorSource) bool { return s.Folder == "INBOX" })
	if inbox < 0 {
		t.Fatalf("cursor sources = %+v, want INBOX", genuine.Sources)
	}
	for name, cursor := range map[string]string{
		"extra account":      forge(ref.CursorSource{Account: "gmail", Folder: "[Gmail]/All Mail", UIDValidity: 1}),
		"extra done account": forge(ref.CursorSource{Account: "gmail", Folder: "[Gmail]/All Mail", UIDValidity: 1, Done: true}),
		"excluded folder":    forge(ref.CursorSource{Account: "host", Folder: "Trash", UIDValidity: 1}),
		"unknown folder":     forge(ref.CursorSource{Account: "host", Folder: "Secret", UIDValidity: 1}),
		"repeated source":    forge(genuine.Sources[inbox]),
	} {
		resp, err := m.search(t, &chainmailv1.SearchRequest{Accounts: hostOnly, Limit: 1, Cursor: cursor})
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%s: rows %v, err %v, want INVALID_ARGUMENT", name, subjects(resp.GetRows()), err)
		}
	}
	if n := m.gmail.Calls("Search"); n != 0 {
		t.Errorf("gmail searched %d times, want never", n)
	}
	page2, err := m.search(t, &chainmailv1.SearchRequest{Accounts: hostOnly, Limit: 1, Cursor: page1.GetNextCursor()})
	if err != nil || !slices.Equal(subjects(page2.GetRows()), []string{"a"}) {
		t.Errorf("genuine cursor = %v, %v", subjects(page2.GetRows()), err)
	}
}

func TestShutdownClosesStores(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	if _, err := m.search(t, &chainmailv1.SearchRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if !m.host.Closed() || !m.gmail.Closed() {
		t.Errorf("closed: host %v, gmail %v, want both", m.host.Closed(), m.gmail.Closed())
	}
}
