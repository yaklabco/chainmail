package daemon_test

import (
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// cacheSearchWait bounds the wait for a test daemon's caches to warm. A
// fake store answers at once, so only a broken daemon reaches it.
const cacheSearchWait = 10 * time.Second

// cacheSearchConfig is fakeMailConfig without the body limit, with no pause
// between backfill batches.
const cacheSearchConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"

[cache]
backfill_pause = "0s"
`

// awaitSearchCacheReady polls GetCacheStatus until every account's cache
// is ready, and fails the test after cacheSearchWait.
func awaitSearchCacheReady(t *testing.T, h *daemontest.Harness) {
	t.Helper()
	deadline := time.Now().Add(cacheSearchWait)
	for {
		resp, err := h.Client.Cache.GetCacheStatus(t.Context(), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{}))
		if err != nil {
			t.Fatalf("GetCacheStatus: %v", err)
		}
		ready := len(resp.Msg.GetAccounts()) > 0
		for _, a := range resp.Msg.GetAccounts() {
			ready = ready && a.GetState() == "ready"
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("caches not ready after %v: %v", cacheSearchWait, resp.Msg.GetAccounts())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSearchAnswersFromTheWarmCache runs a real daemon with its cache
// workers: once both caches are ready, header queries and text inside the
// window come from the cache with the server's refs, and text without a
// date bound still goes to the server.
func TestSearchAnswersFromTheWarmCache(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	acme := []mailstore.Address{{Name: "Acme Billing", Email: "billing@acme.example"}}
	host, gmail := fake.New("host", false), fake.New("gmail", true)
	host.Add(fake.Message{Folder: "Archive", Date: now.Add(-72 * time.Hour), Subject: "plan", Body: "about the plan"})
	host.Add(fake.Message{Folder: "INBOX", Date: now.Add(-48 * time.Hour), Subject: "invoice from acme", From: acme, Body: "the invoice is attached"})
	gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: now.Add(-24 * time.Hour), Subject: "invoice reminder", From: acme, Labels: []string{`\Inbox`}, Body: "please pay the invoice"})
	h := daemontest.Start(t, cacheSearchConfig, daemontest.Options{
		Watch:   true,
		Cache:   true,
		Secrets: map[string]string{"accounts/host": "hunter2", "accounts/gmail": "hunter2"},
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			if a.Name == "gmail" {
				return gmail
			}
			return host
		},
	})
	awaitSearchCacheReady(t, h)
	search := func(q, source string) *chainmailv1.SearchResponse {
		t.Helper()
		resp, err := h.Client.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{QueryString: q, Source: source}))
		if err != nil {
			t.Fatalf("search %q (%s): %v", q, source, err)
		}
		return resp.Msg
	}
	refs := func(resp *chainmailv1.SearchResponse) []string {
		var out []string
		for _, r := range resp.GetRows() {
			out = append(out, r.GetRef())
		}
		return out
	}
	// Each query matches one message per account: host's INBOX invoice and
	// Gmail's reminder, so an empty page from both sources cannot pass.
	for _, tt := range []struct {
		q    string
		rows int
	}{{"from:acme", 2}, {"invoice newer_than:30d", 2}, {"in:inbox", 2}} {
		q := tt.q
		c, s := search(q, ""), search(q, "server")
		if n := len(c.GetRows()); n != tt.rows {
			t.Errorf("%q: %d rows from the cache, want %d", q, n, tt.rows)
		}
		if c.GetSource() != "cache" {
			t.Errorf("%q: source %q, want cache", q, c.GetSource())
		}
		for _, ac := range c.GetAccountCounts() {
			if ac.GetSource() != "cache" || !ac.GetComplete() || ac.GetSyncedAt() == nil {
				t.Errorf("%q: account count %v, want complete from the cache with synced_at", q, ac)
			}
		}
		if !slices.Equal(refs(c), refs(s)) {
			t.Errorf("%q: cache refs %v, server refs %v", q, refs(c), refs(s))
		}
	}
	if got := search("invoice", "").GetSource(); got != "server" {
		t.Errorf("text without a date bound: source %q, want server", got)
	}
}
