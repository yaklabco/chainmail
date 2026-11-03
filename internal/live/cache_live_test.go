//go:build live

package live

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
)

const (
	// cachePollEvery is how often the wait asks for cache status.
	cachePollEvery = 30 * time.Second
	// cacheSettle is how long a difference may last before it counts: the
	// default fresh_lag of 30 seconds plus a sync pass of slack.
	cacheSettle = 45 * time.Second
	// cacheTries is how often a query is compared before a lasting
	// difference fails the test.
	cacheTries = 3
	// cacheRows is the page size of the compared searches.
	cacheRows = 50
	// cacheMaxPages bounds the empty pages read per source and query
	// before the first row.
	cacheMaxPages = 10
)

// cacheQuery is one comparison query. name stands for it in the log, so an
// address in a query is never logged.
type cacheQuery struct{ name, q string }

// cacheQueries are 10 header queries per account whose meaning is the same
// on both paths (spec section 6.5). Google accounts get no date, size or
// has: terms, which X-GM-RAW reads its own way, and no text.
func cacheQueries(a *chainmailv1.Account) []cacheQuery {
	if isGoogle(a) {
		return []cacheQuery{
			{"default", ""},
			{"unread", "is:unread"},
			{"starred", "is:starred"},
			{"read", "is:read"},
			{"inbox", "in:inbox"},
			{"sent", "in:sent"},
			{"drafts", "in:drafts"},
			{"inbox unread", "in:inbox is:unread"},
			{"inbox not starred", "in:inbox -is:starred"},
			{"archive starred", "in:archive is:starred"},
		}
	}
	return []cacheQuery{
		{"default", ""},
		{"unread", "is:unread"},
		{"starred", "is:starred"},
		{"inbox", "in:inbox"},
		{"sent", "in:sent"},
		{"inbox unread", "in:inbox is:unread"},
		{"last 30 days", "newer_than:30d"},
		{"older than a year", "older_than:365d"},
		{"from self", "from:" + a.GetAddress()},
		{"over 100K", "larger:100K"},
	}
}

// TestCacheMatchesServer is the live comparison test of spec section 1
// (done when, items 1, 2 and 4) and section 11.3. It waits, bounded, for
// every account's cache to report ready, then per account requires that 10
// header queries forced to the cache and forced to the server return the
// same refs, that a default search goes to the cache by itself, and that
// a message read twice is answered from the cache the second time. It
// never changes mail, so it runs against a production daemon, which
// refuses expunge, without CHAINMAIL_LIVE_WRITES. An account whose cache
// is not ready after the wait is skipped with the reason. It logs refs and
// counts, never subjects, addresses or bodies.
func TestCacheMatchesServer(t *testing.T) {
	c := liveClient(t)
	accounts := liveAccounts(t, c)
	names := make([]string, len(accounts))
	for i, a := range accounts {
		names[i] = a.GetName()
	}
	ready := waitCacheReady(t, c, names)
	for _, a := range accounts {
		t.Run(a.GetName(), func(t *testing.T) {
			if why := cacheSkipReason(ready[a.GetName()]); why != "" {
				t.Skip(why)
			}
			for _, cq := range cacheQueries(a) {
				compareSources(t, c, a.GetName(), cq)
			}
			routesToCache(t, c, a.GetName())
			readsThroughCache(t, c, a.GetName())
		})
	}
}

// waitCacheReady polls GetCacheStatus until every account's cache is ready
// or disabled, or cacheReadyDeadline passes, logging progress, and returns
// the last status of each account. Every status call runs under the wait's
// deadline, so a stalled call cannot eat the time kept for the queries: one
// cut short by the deadline ends the wait with the last status, or fails
// the test when no call has answered yet.
func waitCacheReady(t *testing.T, c *client.Client, names []string) map[string]*chainmailv1.AccountCacheStatus {
	t.Helper()
	deadline, ok := t.Deadline()
	ctx, cancel := context.WithDeadline(t.Context(), cacheReadyDeadline(time.Now(), deadline, ok))
	defer cancel()
	var last map[string]*chainmailv1.AccountCacheStatus
	for {
		resp, err := c.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: names}))
		if err != nil {
			if last != nil && ctx.Err() != nil && t.Context().Err() == nil {
				return last
			}
			t.Fatalf("GetCacheStatus: %v (%s)", err, hint(err))
		}
		byName := make(map[string]*chainmailv1.AccountCacheStatus, len(names))
		pending := 0
		for _, a := range resp.Msg.GetAccounts() {
			byName[a.GetAccount()] = a
			var headers uint64
			done := 0
			for _, f := range a.GetFolders() {
				headers += f.GetHeaders()
				if f.GetBackfillDone() {
					done++
				}
			}
			t.Logf("%s: %s, %d of %d folders backfilled, %d headers, %d of %d budget bytes today",
				a.GetAccount(), a.GetState(), done, len(a.GetFolders()), headers, a.GetBudgetUsedBytes(), a.GetBudgetBytes())
			if s := a.GetState(); s != "ready" && s != "disabled" {
				pending++
			}
		}
		t.Logf("cache.db %d of %d bytes", resp.Msg.GetSizeBytes(), resp.Msg.GetMaxSizeBytes())
		last = byName
		if pending == 0 {
			return byName
		}
		select {
		case <-ctx.Done():
			if err := t.Context().Err(); err != nil {
				t.Fatalf("waiting for the caches: %v", err)
			}
			return byName
		case <-time.After(cachePollEvery):
		}
	}
}

// cacheSettleWait waits cacheSettle, or fails the test when it ends first.
func cacheSettleWait(t *testing.T) {
	t.Helper()
	select {
	case <-t.Context().Done():
		t.Fatalf("waiting to compare again: %v", t.Context().Err())
	case <-time.After(cacheSettle):
	}
}

// sourcePage runs q on account forced to source, from cursor, and returns
// the page, failing the test on an error. A partial page is returned for
// the caller to judge: it may pass on a later try.
func sourcePage(t *testing.T, c *client.Client, account, q, source, cursor string) *chainmailv1.SearchResponse {
	t.Helper()
	resp, err := c.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{
		QueryString: q, Accounts: []string{account}, Limit: cacheRows, Source: source, Cursor: cursor,
	}))
	if err != nil {
		t.Fatalf("Search (source %q): %v (%s)", source, err, hint(err))
	}
	return resp.Msg
}

// readHead reads q on account forced to source until a page has a row or
// the search ends, up to cacheMaxPages pages: a page can be empty while
// its cursor is set.
func readHead(t *testing.T, c *client.Client, account, q, source string) sourcePages {
	t.Helper()
	var p sourcePages
	for len(p) < cacheMaxPages && (len(p) == 0 || (p.more() && len(p.rows()) == 0)) {
		cursor := ""
		if len(p) > 0 {
			cursor = p[len(p)-1].GetNextCursor()
		}
		p = append(p, sourcePage(t, c, account, q, source, cursor))
	}
	return p
}

// compareSources compares cq on account from the cache and from the
// server. Each side is read until it has a row, and what both sides'
// pages cover, by their cursors, is compared (rowsDiff), whatever the
// rows' arrival times. A difference can be the cache trailing by up to
// fresh_lag, a flag that changed between the two searches, or a partial
// page, so it is compared again after cacheSettle, and only a difference
// that lasts cacheTries tries fails the test.
func compareSources(t *testing.T, c *client.Client, account string, cq cacheQuery) {
	t.Helper()
	diff := ""
	for try := 1; try <= cacheTries; try++ {
		if try > 1 {
			t.Logf("query %s differs (%s); comparing again after %v", cq.name, diff, cacheSettle)
			cacheSettleWait(t)
		}
		start := time.Now()
		cache := readHead(t, c, account, cq.q, "cache")
		server := readHead(t, c, account, cq.q, "server")
		if diff = sourcesDiff(cache, server, account, start); diff == "" {
			t.Logf("query %s: %d and %d rows read, the covered ones agree", cq.name, len(cache.rows()), len(server.rows()))
			return
		}
	}
	t.Errorf("query %s: the cache and the server still differ after %d tries: %s", cq.name, cacheTries, diff)
}

// routesToCache requires a default search with no source to be answered by
// the cache: done when item 1. A partial answer counts against the try
// that got it.
func routesToCache(t *testing.T, c *client.Client, account string) {
	t.Helper()
	for try := 1; ; try++ {
		resp := sourcePage(t, c, account, "", "", "")
		var got string
		for _, ac := range resp.GetAccountCounts() {
			if ac.GetAccount() == account {
				got = ac.GetSource()
			}
		}
		partial := partialDiff(resp.GetPartial())
		if partial == "" && got == "cache" && resp.GetSource() == "cache" {
			return
		}
		if try == cacheTries {
			t.Errorf("a default search was answered by %q (response %q, partial %q), want cache and every folder read", got, resp.GetSource(), partial)
			return
		}
		t.Logf("a default search was answered by %q (partial %q); asking again after %v", got, partial, cacheSettle)
		cacheSettleWait(t)
	}
}

// readsThroughCache reads the newest cached message twice. The first get
// may go to the server, when its body is not cached yet; it keeps the
// body, so the second must come from the cache: done when item 2, as far
// as a client can see it.
func readsThroughCache(t *testing.T, c *client.Client, account string) {
	t.Helper()
	resp := sourcePage(t, c, account, "", "cache", "")
	reportPartial(t, "Search", resp.GetPartial())
	rows := resp.GetRows()
	if len(rows) == 0 {
		t.Fatal("the cache holds no message to read")
	}
	r := rows[0].GetRef()
	first := getOne(t, c, r, false).GetSource()
	second := getOne(t, c, r, false).GetSource()
	t.Logf("get %s: first from %s, then from %s", r, first, second)
	if second != "cache" {
		t.Errorf("the second get of %s came from %q, want cache", r, second)
	}
}
