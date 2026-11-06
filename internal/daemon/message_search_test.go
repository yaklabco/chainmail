package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// addDays adds one message per day to folder, oldest first, so UIDs rise
// with arrival time as on a real server. Each subject is "d" and its day.
func addDays(s *fake.Store, folder string, days ...int) {
	slices.Sort(days)
	slices.Reverse(days)
	for _, d := range days {
		s.Add(fake.Message{Folder: folder, Date: daysAgo(d), Subject: fmt.Sprintf("d%d", d)})
	}
}

// pageAll follows next_cursor from req until the last page, with up to
// pages pages, and returns every page.
func pageAll(t *testing.T, m fakeMail, req *chainmailv1.SearchRequest, pages int) []*chainmailv1.SearchResponse {
	t.Helper()
	var out []*chainmailv1.SearchResponse
	for range pages {
		resp, err := m.search(t, req)
		if err != nil {
			t.Fatalf("page %d: %v", len(out)+1, err)
		}
		out = append(out, resp)
		if resp.GetNextCursor() == "" {
			return out
		}
		req.Cursor = resp.GetNextCursor()
	}
	t.Fatalf("still paging after %d pages", pages)
	return nil
}

func TestSearchCarriesFailedSourcesToLaterPages(t *testing.T) {
	t.Parallel()
	down := fmt.Errorf("%w: down", mailstore.ErrUnavailable)
	for name, tt := range map[string]struct {
		fail   func(s *fake.Store, err error)
		folder string
	}{
		"account search": {func(s *fake.Store, err error) { s.Fail("Search", err) }, ""},
		"folder search":  {func(s *fake.Store, err error) { s.FailFolder("INBOX", err) }, "INBOX"},
		"fetch":          {func(s *fake.Store, err error) { s.Fail("FetchSummaries", err) }, "INBOX"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := startFakeMail(t)
			addDays(m.host, "INBOX", 1, 3, 5)
			addDays(m.gmail, "[Gmail]/All Mail", 2, 4, 6)
			page1, err := m.search(t, &chainmailv1.SearchRequest{Limit: 2})
			if err != nil || !slices.Equal(subjects(page1.GetRows()), []string{"d1", "d2"}) {
				t.Fatalf("page 1 = %v, %v", subjects(page1.GetRows()), err)
			}
			tt.fail(m.host, down)
			page2, err := m.search(t, &chainmailv1.SearchRequest{Limit: 2, Cursor: page1.GetNextCursor()})
			if err != nil {
				t.Fatal(err)
			}
			// gmail pages on past the failed host, so host's d3 and d5 come back
			// on page 3, after newer rows. Partial on page 2 says why.
			if got := subjects(page2.GetRows()); !slices.Equal(got, []string{"d4", "d6"}) {
				t.Errorf("page 2 = %v", got)
			}
			if p := page2.GetPartial(); len(p) != 1 || p[0].GetAccount() != "host" || p[0].GetFolder() != tt.folder || p[0].GetReason() != api.ReasonServerUnavailable {
				t.Errorf("page 2 partial = %v", p)
			}
			if page2.GetNextCursor() == "" {
				t.Fatal("page 2 has no cursor, so host's d3 and d5 are lost")
			}
			tt.fail(m.host, nil)
			page3, err := m.search(t, &chainmailv1.SearchRequest{Limit: 2, Cursor: page2.GetNextCursor()})
			if err != nil {
				t.Fatal(err)
			}
			if got := subjects(page3.GetRows()); !slices.Equal(got, []string{"d3", "d5"}) || page3.GetNextCursor() != "" || len(page3.GetPartial()) != 0 {
				t.Errorf("page 3 = %v, cursor %q, partial %v", got, page3.GetNextCursor(), page3.GetPartial())
			}
		})
	}
}

func TestSearchFinishedFolderStaysSkipped(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	addDays(m.host, "Archive", 1)
	addDays(m.host, "INBOX", 2, 3, 4)
	page1, err := m.search(t, &chainmailv1.SearchRequest{Accounts: []string{"host"}, Limit: 2})
	if err != nil || !slices.Equal(subjects(page1.GetRows()), []string{"d1", "d2"}) {
		t.Fatalf("page 1 = %v, %v", subjects(page1.GetRows()), err)
	}
	// Archive finished with its one match used, and Sent with none, so Sent
	// has no UID bound and only being done keeps it out.
	m.host.Add(fake.Message{Folder: "Archive", Date: daysAgo(0), Subject: "arrived later"})
	m.host.Add(fake.Message{Folder: "Sent", Date: daysAgo(0), Subject: "sent later"})
	page2, err := m.search(t, &chainmailv1.SearchRequest{Accounts: []string{"host"}, Limit: 2, Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page2.GetRows()); !slices.Equal(got, []string{"d3", "d4"}) || page2.GetNextCursor() != "" {
		t.Errorf("page 2 = %v, cursor %q, want the finished folders left alone", got, page2.GetNextCursor())
	}
}

func TestSearchIgnoresMailArrivingInAnUnreachedSource(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	addDays(m.host, "INBOX", 1, 2)
	addDays(m.gmail, "[Gmail]/All Mail", 3, 4)
	page1, err := m.search(t, &chainmailv1.SearchRequest{Limit: 2})
	if err != nil || !slices.Equal(subjects(page1.GetRows()), []string{"d1", "d2"}) {
		t.Fatalf("page 1 = %v, %v", subjects(page1.GetRows()), err)
	}
	m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: daysAgo(0), Subject: "arrived later"})
	page2, err := m.search(t, &chainmailv1.SearchRequest{Limit: 2, Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page2.GetRows()); !slices.Equal(got, []string{"d3", "d4"}) || page2.GetNextCursor() != "" {
		t.Errorf("page 2 = %v, cursor %q, want only what matched on page 1", got, page2.GetNextCursor())
	}
}

func TestSearchPagesThreadsNewestFirst(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	// One host thread holds the four newest messages, so its rows use up
	// every row host fetches for a page.
	refs := []string{}
	for _, d := range []int{4, 3, 2, 1} {
		id := fmt.Sprintf("x%d@x", d)
		m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(d), Subject: fmt.Sprintf("d%d", d), MessageID: id, References: slices.Clone(refs)})
		refs = append(refs, id)
	}
	addDays(m.gmail, "[Gmail]/All Mail", 5, 6)
	var got []string
	var counts []uint32
	for _, page := range pageAll(t, m, &chainmailv1.SearchRequest{Limit: 2, Threads: true}, 5) {
		got = append(got, subjects(page.GetRows())...)
		for _, r := range page.GetRows() {
			counts = append(counts, r.GetThreadCount())
		}
	}
	if !slices.Equal(got, []string{"d1", "d3", "d5", "d6"}) || !slices.Equal(counts, []uint32{2, 2, 1, 1}) {
		t.Errorf("thread rows across pages = %v, counts %v, want newest first", got, counts)
	}
}

func TestSearchAuditNamesTheCallsAccountsOnEveryPage(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	addDays(m.host, "INBOX", 1)
	addDays(m.gmail, "[Gmail]/All Mail", 2, 3)
	page1, err := m.search(t, &chainmailv1.SearchRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	// host has nothing left, so page 2 searches only gmail.
	if _, err := m.search(t, &chainmailv1.SearchRequest{Limit: 1, Cursor: page1.GetNextCursor()}); err != nil {
		t.Fatal(err)
	}
	if row := lastMailAudit(t, m); row.Action != "search" || row.Account != "" || row.Targets != `["accounts=2","rows=1","source=server"]` {
		t.Errorf("page 2 audit = %+v, want both accounts", row)
	}
}

// hidingStore drops one folder from Search, as when it is deleted or
// renamed between pages.
type hidingStore struct {
	*fake.Store
	mu     sync.Mutex
	hidden string
}

func (h *hidingStore) hide(folder string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hidden = folder
}

func (h *hidingStore) Search(ctx context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	res, err := h.Store.Search(ctx, req)
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.DeleteFunc(res, func(r mailstore.SourceResult) bool { return r.Source.Folder == h.hidden }), err
}

func TestSearchCursorAfterAFolderIsRemoved(t *testing.T) {
	t.Parallel()
	host, gmail := &hidingStore{Store: fake.New("host", false)}, fake.New("gmail", true)
	h := daemontest.Start(t, fakeMailConfig, daemontest.Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			if a.Name == "gmail" {
				return gmail
			}
			return host
		},
		Now: func() time.Time { return testNow },
	})
	m := fakeMail{Harness: h, host: host.Store, gmail: gmail}
	addDays(host.Store, "INBOX", 1)
	addDays(host.Store, "Archive", 2)
	page1, err := m.search(t, &chainmailv1.SearchRequest{Accounts: []string{"host"}, Limit: 1})
	if err != nil || page1.GetNextCursor() == "" {
		t.Fatalf("page 1 = %v, cursor %q, %v", subjects(page1.GetRows()), page1.GetNextCursor(), err)
	}
	host.hide("Archive")
	_, err = m.search(t, &chainmailv1.SearchRequest{Accounts: []string{"host"}, Limit: 1, Cursor: page1.GetNextCursor()})
	hint := api.Detail(err).GetHint()
	if reasonOf(err) != api.ReasonInvalidArgument || !strings.Contains(err.Error(), "folder") || !strings.Contains(hint, "folders changed") || !strings.Contains(hint, "search again") {
		t.Errorf("err = %v (hint %q), want INVALID_ARGUMENT saying the folders changed", err, hint)
	}
}

// A server search leaves out the matches whose summaries the server's
// answer could not be parsed for, keeps the rest of the source's rows, and
// reports the source in partial with MALFORMED_RESPONSE and how many it
// left out. A match is reported on the page that passes it, once: one the
// cursor still has ahead waits for the page that reaches it.
func TestSearchLeavesOutUnreadableMessages(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	addDays(m.host, "INBOX", 1, 4, 5) // UIDs 1 to 3: d5, d4, d1
	addDays(m.gmail, "[Gmail]/All Mail", 2, 3)
	m.host.Malform("INBOX", 1, fake.BadEnvelope)
	unreadable := func(resp *chainmailv1.SearchResponse) []string {
		var out []string
		for _, p := range resp.GetPartial() {
			out = append(out, fmt.Sprintf("%s/%s %s %d", p.GetAccount(), p.GetFolder(), p.GetReason(), p.GetUnreadable()))
		}
		return out
	}
	// Page 1 fetches d1, d4 and d5 from host but uses only d1, so d5 is
	// still ahead of the cursor.
	page1, err := m.search(t, &chainmailv1.SearchRequest{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page1.GetRows()); !slices.Equal(got, []string{"d1", "d2", "d3"}) || len(page1.GetPartial()) != 0 {
		t.Errorf("page 1 = %v, partial %v; want d1 to d3 and none", got, unreadable(page1))
	}
	page2, err := m.search(t, &chainmailv1.SearchRequest{Limit: 3, Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(page2.GetRows()); !slices.Equal(got, []string{"d4"}) || page2.GetNextCursor() != "" {
		t.Errorf("page 2 = %v, cursor %q; want d4 and the end", got, page2.GetNextCursor())
	}
	if got, want := unreadable(page2), []string{"host/INBOX MALFORMED_RESPONSE 1"}; !slices.Equal(got, want) {
		t.Errorf("page 2 partial = %v, want %v", got, want)
	}
	if p := page2.GetPartial(); len(p) == 1 && (!strings.Contains(p[0].GetMessage(), "left out 1 of the folder's matches") || p[0].GetHint() == "") {
		t.Errorf("page 2 partial message %q, hint %q", p[0].GetMessage(), p[0].GetHint())
	}

	// A page that fetches only the unreadable match passes it, and the
	// source is done.
	host, err := m.search(t, &chainmailv1.SearchRequest{Accounts: []string{"host"}, QueryString: "subject:d5"})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.GetRows()) != 0 || host.GetNextCursor() != "" || host.GetCount() != 1 {
		t.Errorf("d5 alone = rows %v, cursor %q, count %d; want no rows, no cursor, count 1", subjects(host.GetRows()), host.GetNextCursor(), host.GetCount())
	}
	if got, want := unreadable(host), []string{"host/INBOX MALFORMED_RESPONSE 1"}; !slices.Equal(got, want) {
		t.Errorf("d5 alone partial = %v, want %v", got, want)
	}
}
