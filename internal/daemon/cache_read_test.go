package daemon_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// cacheReadWait bounds the wait for a test daemon's cache to warm. A fake
// store answers at once, so only a broken daemon reaches it.
const cacheReadWait = 10 * time.Second

// cacheReadConfig is one account with no pause between backfill batches.
const cacheReadConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[cache]
backfill_pause = "0s"
`

// awaitReadCacheReady polls GetCacheStatus until every account's cache is
// ready, and fails the test after cacheReadWait.
func awaitReadCacheReady(t *testing.T, h *daemontest.Harness) {
	t.Helper()
	deadline := time.Now().Add(cacheReadWait)
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
			t.Fatalf("cache not ready after %v: %v", cacheReadWait, resp.Msg.GetAccounts())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGetReadsTheWarmCacheWithoutTheServer runs a real daemon with its
// cache workers: once the cache is ready, a get of a message in the
// watched INBOX makes no FetchMessages or FetchSummaries call (done-when
// 2), says it came from the cache, and audits that.
func TestGetReadsTheWarmCacheWithoutTheServer(t *testing.T) {
	t.Parallel()
	host := fake.New("host", false)
	r := host.Add(fake.Message{Folder: "INBOX", Date: time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second), Subject: "cached", Body: "from the cache"})
	h := daemontest.Start(t, cacheReadConfig, daemontest.Options{
		Watch:    true,
		Cache:    true,
		Secrets:  map[string]string{"accounts/host": "hunter2"},
		NewStore: func(config.ResolvedAccount) mailstore.Store { return host },
	})
	awaitReadCacheReady(t, h)
	fetches, summaries := host.Calls("FetchMessages"), host.Calls("FetchSummaries")
	resp, err := h.Client.Message.Get(t.Context(), connect.NewRequest(&chainmailv1.GetRequest{Refs: []string{r.String()}}))
	if err != nil {
		t.Fatal(err)
	}
	if n, m := host.Calls("FetchMessages")-fetches, host.Calls("FetchSummaries")-summaries; n != 0 || m != 0 {
		t.Errorf("a cached get made %d FetchMessages and %d FetchSummaries calls, want none", n, m)
	}
	msg := resp.Msg.GetItems()[0].GetMessage()
	if msg.GetBody().GetUntrusted() != "from the cache" || msg.GetSource() != "cache" || resp.Msg.GetSource() != "cache" {
		t.Errorf("get = body %q, source %q, response source %q", msg.GetBody().GetUntrusted(), msg.GetSource(), resp.Msg.GetSource())
	}
	rows := h.AuditRows(t)
	if last := rows[len(rows)-1]; last.Action != "get" || !strings.HasSuffix(last.Targets, fmt.Sprintf(`%q,"messages=1","errors=0","source=cache"]`, r.String())) {
		t.Errorf("audit = %+v", last)
	}
}
