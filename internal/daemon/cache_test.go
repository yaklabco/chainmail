package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// cacheRunConfig has one account with the cache and one with it off.
const cacheRunConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.off]
provider = "hostinger"
address = "off@example.com"

[accounts.off.cache]
enabled = false
`

// cacheRunWait bounds every wait on the cache workers. A fake store
// answers at once, so only a broken daemon reaches it.
const cacheRunWait = 15 * time.Second

// cacheStates are the account states C5 allows.
var cacheStates = []string{"backfilling", "prefetching", "ready", "budget_exhausted", "paused", "disabled", "error", "over_cap"}

// startCacheRun runs a watching daemon with the cache on, each account on
// its store from stores, with passwords stored so no supervisor parks an
// account as needing authentication. reuse, when set, restarts that
// harness's daemon on the same state.
func startCacheRun(t *testing.T, configTOML string, stores map[string]mailstore.Store, reuse *daemontest.Harness) *daemontest.Harness {
	t.Helper()
	return daemontest.Start(t, configTOML, daemontest.Options{
		Watch:    true,
		Cache:    true,
		Reuse:    reuse,
		Secrets:  map[string]string{"accounts/host": "hunter2", "accounts/off": "hunter2"},
		NewStore: func(a config.ResolvedAccount) mailstore.Store { return stores[a.Name] },
	})
}

// getCacheStatus returns the cache status of accounts, or of every account.
func getCacheStatus(t *testing.T, h *daemontest.Harness, accounts ...string) *chainmailv1.GetCacheStatusResponse {
	t.Helper()
	resp, err := h.Client.Cache.GetCacheStatus(t.Context(), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: accounts}))
	if err != nil {
		t.Fatalf("GetCacheStatus: %v", err)
	}
	return resp.Msg
}

// accountStatus picks one account from a status response.
func accountStatus(t *testing.T, resp *chainmailv1.GetCacheStatusResponse, account string) *chainmailv1.AccountCacheStatus {
	t.Helper()
	for _, a := range resp.GetAccounts() {
		if a.GetAccount() == account {
			return a
		}
	}
	t.Fatalf("no cache status for %s in %v", account, resp)
	return nil
}

// awaitCacheFolder polls the cache status until folder of account
// satisfies ok, and returns it.
func awaitCacheFolder(t *testing.T, h *daemontest.Harness, account, folder string, ok func(*chainmailv1.FolderCacheStatus) bool) *chainmailv1.FolderCacheStatus {
	t.Helper()
	deadline := time.Now().Add(cacheRunWait)
	var last *chainmailv1.GetCacheStatusResponse
	for time.Now().Before(deadline) {
		last = getCacheStatus(t, h, account)
		for _, a := range last.GetAccounts() {
			for _, f := range a.GetFolders() {
				if f.GetFolder() == folder && ok(f) {
					return f
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the cache of %s %s never reached the wanted state; last status %v", account, folder, last)
	return nil
}

// cached reports whether a folder holds want headers and its backfill is done.
func cached(want uint64) func(*chainmailv1.FolderCacheStatus) bool {
	return func(f *chainmailv1.FolderCacheStatus) bool { return f.GetHeaders() == want && f.GetBackfillDone() }
}

// cacheAudit returns the audit rows of cache actions.
func cacheAudit(t *testing.T, h *daemontest.Harness) []daemontest.AuditRow {
	t.Helper()
	var out []daemontest.AuditRow
	for _, r := range h.AuditRows(t) {
		if strings.HasPrefix(r.Action, "cache.") {
			out = append(out, r)
		}
	}
	return out
}

// startTwoMessages runs startCacheRun with two messages already in host's
// INBOX and waits until the cache holds both.
func startTwoMessages(t *testing.T) (*daemontest.Harness, *fake.Store) {
	t.Helper()
	host, off := fake.New("host", false), fake.New("off", false)
	now := time.Now()
	host.Add(fake.Message{Folder: "INBOX", Subject: "one", Date: now.Add(-2 * time.Hour)})
	host.Add(fake.Message{Folder: "INBOX", Subject: "two", Date: now.Add(-time.Hour)})
	h := startCacheRun(t, cacheRunConfig, map[string]mailstore.Store{"host": host, "off": off}, nil)
	awaitCacheFolder(t, h, "host", "INBOX", cached(2))
	return h, host
}

// The workers fill the cache of an account with the cache on, the status
// shows it, the account with the cache off says disabled, admin status
// carries the summary without folders, and nothing is audited.
func TestCacheStatusFollowsTheWorkers(t *testing.T) {
	t.Parallel()
	h, _ := startTwoMessages(t)
	resp := getCacheStatus(t, h)
	if resp.GetSizeBytes() <= 0 || resp.GetMaxSizeBytes() != 2<<30 {
		t.Errorf("size = %d of %d, want above 0 of 2 GiB", resp.GetSizeBytes(), resp.GetMaxSizeBytes())
	}
	host := accountStatus(t, resp, "host")
	if !host.GetEnabled() || !slices.Contains(cacheStates, host.GetState()) || host.GetBudgetBytes() != 1<<30 {
		t.Errorf("host = %v, want enabled, a C5 state and a 1 GiB budget", host)
	}
	off := accountStatus(t, resp, "off")
	if off.GetEnabled() || off.GetState() != "disabled" || len(off.GetFolders()) != 0 {
		t.Errorf("off = %v, want disabled without folders", off)
	}
	for _, tt := range []struct {
		account, reason string
	}{
		{account: "nope", reason: api.ReasonNotFound},
		{account: "Bad Name", reason: api.ReasonInvalidArgument},
	} {
		_, err := h.Client.Cache.GetCacheStatus(t.Context(), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: []string{tt.account}}))
		if got := reasonOf(err); got != tt.reason {
			t.Errorf("GetCacheStatus(%q) reason = %q, want %q", tt.account, got, tt.reason)
		}
	}

	adm, err := h.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	summary := map[string]*chainmailv1.AccountCacheStatus{}
	for _, a := range adm.Msg.GetCache() {
		summary[a.GetAccount()] = a
	}
	if a := summary["host"]; a == nil || !a.GetEnabled() || len(a.GetFolders()) != 0 {
		t.Errorf("admin status host = %v, want enabled without folders", a)
	}
	if a := summary["off"]; a == nil || a.GetState() != "disabled" {
		t.Errorf("admin status off = %v, want disabled", a)
	}
	if rows := cacheAudit(t, h); len(rows) != 0 {
		t.Errorf("cache audit rows = %+v, want none", rows)
	}
}

// RebuildCache drops one folder or every tracked folder and the workers
// fill it again; a folder the cache does not track is NOT_FOUND, an account
// without a cache worker INVALID_ARGUMENT, and every call is audited.
func TestRebuildCache(t *testing.T) {
	t.Parallel()
	h, _ := startTwoMessages(t)
	rebuild := func(account, folder string) ([]string, error) {
		resp, err := h.Client.Cache.RebuildCache(t.Context(), connect.NewRequest(&chainmailv1.RebuildCacheRequest{Account: account, Folder: folder}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetFolders(), nil
	}

	got, err := rebuild("host", "INBOX")
	if err != nil || !slices.Equal(got, []string{"INBOX"}) {
		t.Fatalf("rebuild INBOX = %v, %v; want [INBOX]", got, err)
	}
	awaitCacheFolder(t, h, "host", "INBOX", cached(2))

	if _, err := rebuild("host", "Nope"); reasonOf(err) != api.ReasonNotFound {
		t.Errorf("rebuild of an untracked folder: %v, want NOT_FOUND", err)
	}
	if _, err := rebuild("off", ""); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("rebuild of an account without a cache: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := rebuild("nope", ""); reasonOf(err) != api.ReasonNotFound {
		t.Errorf("rebuild of an unknown account: %v, want NOT_FOUND", err)
	}

	all, err := rebuild("host", "")
	if err != nil || !slices.Contains(all, "INBOX") {
		t.Fatalf("rebuild of every folder = %v, %v; want INBOX among them", all, err)
	}
	awaitCacheFolder(t, h, "host", "INBOX", cached(2))

	want := []daemontest.AuditRow{
		{Client: "test", Account: "host", Action: "cache.rebuild", Targets: `["folder=INBOX","rebuilt=1"]`, Result: "ok"},
		{Client: "test", Account: "host", Action: "cache.rebuild", Targets: `["folder=Nope","rebuilt=0"]`, Result: "error", Reason: api.ReasonNotFound},
		{Client: "test", Account: "off", Action: "cache.rebuild", Targets: `["folder=(all)","rebuilt=0"]`, Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Account: "nope", Action: "cache.rebuild", Targets: `["folder=(all)","rebuilt=0"]`, Result: "error", Reason: api.ReasonNotFound},
		{Client: "test", Account: "host", Action: "cache.rebuild", Targets: fmt.Sprintf(`["folder=(all)","rebuilt=%d"]`, len(all)), Result: "ok"},
	}
	if got := cacheAudit(t, h); !slices.Equal(got, want) {
		t.Errorf("audit rows =\n%+v\nwant\n%+v", got, want)
	}
}
