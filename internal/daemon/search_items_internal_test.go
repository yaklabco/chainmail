package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// stubItemResolver resolves item: values from fixed maps, as *items.Store
// does from state.db. keys maps an item ID to its accounts' thread keys,
// and an ID missing from it is not found; linked maps an account to the
// keys linked to any live item. err, when set, fails every call.
type stubItemResolver struct {
	keys   map[string]map[string][]string
	linked map[string][]string
	err    error
}

var _ itemResolver = (*stubItemResolver)(nil)

func (s *stubItemResolver) ThreadKeys(_ context.Context, id string) (map[string][]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	k, ok := s.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w: item", items.ErrNotFound)
	}
	return k, nil
}

func (s *stubItemResolver) LinkedThreadKeys(_ context.Context, account string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.linked[account], nil
}

// planKey is the thread key of host's "old plan" thread in seedRouteMail.
var planKey = mailstore.ThreadKey("p@x")

// planThread is that thread's messages, newest first.
var planThread = []string{"re: re: old plan", "re: old plan", "old plan"}

// itemFixture is a route fixture with host's cache warm and installed and
// gmail's not, and a resolver that links host's plan thread to it_plan
// and has it_empty with no links.
func itemFixture(t *testing.T) *routeFixture {
	t.Helper()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.install(t, "host")
	f.s.itemThreads = &stubItemResolver{
		keys:   map[string]map[string][]string{"it_plan": {"host": {planKey}}, "it_empty": {}},
		linked: map[string][]string{"host": {planKey}},
	}
	return f
}

// serverSearches counts both fakes' Search calls, so a test can show that
// item: never reached a mail server.
func (f *routeFixture) serverSearches() int {
	return f.fakes["host"].Calls("Search") + f.fakes["gmail"].Calls("Search")
}

// clientErr is the error the Search handler sends for err.
func clientErr(err error) *connect.Error { return toConnectError(messageErr(err)) }

var itemHostOnly = []string{"host"}

// TestSearchItemFromTheCache checks that item: is answered from a fresh
// cache, combined with other operators and in:, and never by the server.
func TestSearchItemFromTheCache(t *testing.T) {
	t.Parallel()
	f := itemFixture(t)
	before := f.serverSearches()
	tests := []struct {
		query string
		want  []string
	}{
		{"item:it_plan", planThread},
		{"item:it_empty", nil},
		{"item:none", []string{"lunch", "invoice 42", "invoice 41"}},
		{"-item:none", planThread},
		{"item:none from:acme", []string{"invoice 42", "invoice 41"}},
		{"item:it_plan -from:alex", []string{"re: re: old plan", "re: old plan"}},
		{"item:it_plan OR subject:lunch", append([]string{"lunch"}, planThread...)},
		{"item:none newer_than:7d", []string{"lunch", "invoice 42"}},
		{"item:it_plan in:sent", []string{"re: re: old plan"}},
		{"item:none in:sent", nil},
	}
	for _, tt := range tests {
		resp := f.search(t, &chainmailv1.SearchRequest{QueryString: tt.query, Accounts: itemHostOnly})
		if got := rowSubjects(resp.GetRows()); !slices.Equal(got, tt.want) {
			t.Errorf("%s = %q, want %q", tt.query, got, tt.want)
		}
		if resp.GetSource() != cache.SourceCache || len(resp.GetPartial()) != 0 {
			t.Errorf("%s: source %q, partial %v; want the cache, complete", tt.query, resp.GetSource(), resp.GetPartial())
		}
	}
	count := f.search(t, &chainmailv1.SearchRequest{QueryString: "item:none", Accounts: itemHostOnly, CountOnly: true})
	if count.GetCount() != 3 {
		t.Errorf("count of item:none = %d, want 3", count.GetCount())
	}
	if got := f.serverSearches(); got != before {
		t.Errorf("server searches = %d, want %d: item: reached a mail server", got, before)
	}
}

// TestSearchItemOnGoogleFromTheCache checks item: on a Google account,
// whose thread keys are Gmail's thread IDs.
func TestSearchItemOnGoogleFromTheCache(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "gmail")
	f.install(t, "gmail")
	gmail := []string{"gmail"}
	trip := f.search(t, &chainmailv1.SearchRequest{QueryString: "subject:trip", Accounts: gmail}).GetRows()
	if len(trip) != 2 || trip[0].GetThreadId() == "" || trip[0].GetThreadId() != trip[1].GetThreadId() {
		t.Fatalf("trip rows = %v, want two in one thread", trip)
	}
	key := trip[0].GetThreadId()
	f.s.itemThreads = &stubItemResolver{
		keys:   map[string]map[string][]string{"it_trip": {"gmail": {key}}},
		linked: map[string][]string{"gmail": {key}},
	}
	before := f.serverSearches()
	for q, want := range map[string][]string{
		"item:it_trip": {"re: trip", "trip"},
		"item:none":    {"invoice 40"},
	} {
		resp := f.search(t, &chainmailv1.SearchRequest{QueryString: q, Accounts: gmail})
		if got := rowSubjects(resp.GetRows()); !slices.Equal(got, want) || resp.GetSource() != cache.SourceCache {
			t.Errorf("%s = %q from %q, want %q from the cache", q, got, resp.GetSource(), want)
		}
	}
	if got := f.serverSearches(); got != before {
		t.Errorf("server searches = %d, want %d", got, before)
	}
}

// TestSearchItemRefusals checks every way an item: search fails as a
// whole: the code, reason, hint and fixed message the client sees, that
// no message names the item or the store's own text, and that no mail
// server was searched.
func TestSearchItemRefusals(t *testing.T) {
	t.Parallel()
	const notReadyHint = "the cache has not caught up; run: chainmail cache status"
	uninstall := func(t *testing.T, f *routeFixture) { f.install(t) }
	staleArchive := func(t *testing.T, f *routeFixture) { f.ageSync(t, "host", "Archive", time.Hour) }
	gmailCache := func(t *testing.T, f *routeFixture) {
		f.warm(t, "gmail")
		f.install(t, "host", "gmail")
	}
	noStore := func(_ *testing.T, f *routeFixture) { f.s.itemThreads = nil }
	failing := func(_ *testing.T, f *routeFixture) {
		f.s.itemThreads = &stubItemResolver{err: errors.New("database is locked near it_secret")}
	}
	notReady := errItemCacheNotReady.Error()
	const (
		boundMsg    = "item: is answered from the local cache, which holds message text only for mail inside its body window"
		boundHint   = "add a top-level newer_than: or after: inside the account's prefetch_bodies window, or drop the text terms; waiting does not change this; see the window with: chainmail cache status"
		failedMsg   = "item: is answered from the local cache, which failed reading this account"
		failedHint  = "the daemon log has the cause; see the cache with: chainmail cache status"
		failedQuery = "item:none invoice newer_than:30d"
	)
	readFailure := func(t *testing.T, f *routeFixture) {
		if got := f.search(t, &chainmailv1.SearchRequest{QueryString: failedQuery, Accounts: itemHostOnly}); got.GetSource() != cache.SourceCache {
			t.Fatalf("before the damage: source %q, want cache", got.GetSource())
		}
		f.damage(t, "DROP TABLE messages_fts")
	}
	tests := []struct {
		name     string
		query    string
		source   string
		accounts []string
		setup    func(*testing.T, *routeFixture)
		code     connect.Code
		reason   string
		hint     string
		msg      string
	}{
		{name: "no cache", query: "item:none", setup: uninstall, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: notReadyHint, msg: notReady},
		{name: "stale cache", query: "item:it_plan", setup: staleArchive, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: notReadyHint, msg: notReady},
		{name: "text outside the window", query: "item:none invoice", code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: boundHint, msg: boundMsg},
		{name: "cache read failure", query: failedQuery, setup: readFailure, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: failedHint, msg: failedMsg},
		{name: "Google account without a cache", query: "item:none", accounts: []string{"gmail"}, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: notReadyHint, msg: notReady},
		{name: "Google trash", query: "item:none in:trash", accounts: []string{"gmail"}, setup: gmailCache, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: notReadyHint, msg: notReady},
		{name: "source server", query: "item:none", source: cache.SourceServer, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "item: is answered from the cache; drop source=server", msg: errItemSourceServer.Error()},
		{name: "unknown item", query: "item:it_gone", code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list", msg: "an item: term names no live item"},
		{name: "unknown item under OR", query: "from:acme OR item:it_gone", code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list", msg: "an item: term names no live item"},
		{name: "no item store", query: "item:none", setup: noStore, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: errItemsOff.hint, msg: errItemsOff.Error()},
		{name: "value the cache cannot evaluate", query: `item:none label:\Seen`, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: errItemUnsupported.hint, msg: errItemUnsupported.Error()},
		{name: "item store failure", query: "item:none", setup: failing, code: connect.CodeInternal, reason: api.ReasonInternal, hint: "", msg: "the item store failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := itemFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			accounts := tt.accounts
			if accounts == nil {
				accounts = itemHostOnly
			}
			before := f.serverSearches()
			_, _, err := f.m.search(t.Context(), &chainmailv1.SearchRequest{QueryString: tt.query, Accounts: accounts, Source: tt.source})
			if err == nil {
				t.Fatal("search succeeded, want a refusal")
			}
			cerr := clientErr(err)
			d := api.Detail(cerr)
			if cerr.Code() != tt.code || d.GetReason() != tt.reason || d.GetHint() != tt.hint || cerr.Message() != tt.msg {
				t.Errorf("error = %v %s %q (hint %q), want %v %s %q (hint %q)", cerr.Code(), d.GetReason(), cerr.Message(), d.GetHint(), tt.code, tt.reason, tt.msg, tt.hint)
			}
			for _, leak := range []string{"it_gone", "it_plan", "it_secret", "locked"} {
				if strings.Contains(cerr.Message(), leak) || strings.Contains(d.GetHint(), leak) {
					t.Errorf("error %q (hint %q) names %q", cerr.Message(), d.GetHint(), leak)
				}
			}
			if tt.reason == api.ReasonCacheNotReady && api.ExitCode(d.GetReason()) != api.ExitUnavailable {
				t.Errorf("exit code %d, want %d", api.ExitCode(d.GetReason()), api.ExitUnavailable)
			}
			if got := f.serverSearches(); got != before {
				t.Errorf("server searches = %d, want %d", got, before)
			}
		})
	}
}

// TestSearchItemPartialAccount checks spec amendment 3: an account whose
// cache cannot answer is a partial entry with CACHE_NOT_READY, and the call
// succeeds with the accounts that could.
func TestSearchItemPartialAccount(t *testing.T) {
	t.Parallel()
	f := itemFixture(t)
	before := f.serverSearches()
	resp := f.search(t, &chainmailv1.SearchRequest{QueryString: "item:none"})
	if got, want := rowSubjects(resp.GetRows()), []string{"lunch", "invoice 42", "invoice 41"}; !slices.Equal(got, want) {
		t.Errorf("rows = %q, want host's %q", got, want)
	}
	partial := resp.GetPartial()
	if len(partial) != 1 {
		t.Fatalf("partial = %v, want gmail alone", partial)
	}
	p := partial[0]
	if p.GetAccount() != "gmail" || p.GetReason() != api.ReasonCacheNotReady || p.GetMessage() != errItemCacheNotReady.Error() ||
		p.GetHint() != "the cache has not caught up; run: chainmail cache status" {
		t.Errorf("partial = %v, want gmail with CACHE_NOT_READY and the cache status hint", p)
	}
	if got := f.serverSearches(); got != before {
		t.Errorf("server searches = %d, want %d", got, before)
	}
}

// TestSearchItemForcedCacheIsIncomplete checks that source cache answers
// item: from whatever the cache holds, marked incomplete with the usual
// reason, instead of refusing.
func TestSearchItemForcedCacheIsIncomplete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		spoil  func(*testing.T, *routeFixture)
		reason string
		rows   []string
	}{
		{name: "stale", spoil: func(t *testing.T, f *routeFixture) { f.ageSync(t, "host", "Archive", time.Hour) }, reason: "stale", rows: []string{"lunch", "invoice 42", "invoice 41"}},
		{name: "no cache", spoil: func(t *testing.T, f *routeFixture) { f.install(t) }, reason: "cache_disabled", rows: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := itemFixture(t)
			tt.spoil(t, f)
			resp := f.search(t, &chainmailv1.SearchRequest{QueryString: "item:none", Accounts: itemHostOnly, Source: cache.SourceCache})
			if got := rowSubjects(resp.GetRows()); !slices.Equal(got, tt.rows) {
				t.Errorf("rows = %q, want %q", got, tt.rows)
			}
			counts := resp.GetAccountCounts()
			if len(counts) != 1 || counts[0].GetComplete() || counts[0].GetIncompleteReason() != tt.reason || counts[0].GetSource() != cache.SourceCache {
				t.Errorf("account counts = %v, want host from the cache, incomplete, %s", counts, tt.reason)
			}
		})
	}
}

// TestSearchItemAuditTarget checks that a search with an item: term, done
// or failed, is audited with item_op=1 and never the item's ID, and one
// without has no such target.
func TestSearchItemAuditTarget(t *testing.T) {
	t.Parallel()
	f := itemFixture(t)
	tests := []struct {
		query  string
		itemOp bool
		ok     bool
	}{
		{"item:none", true, true},
		{"item:it_plan from:alex", true, true},
		{"item:it_gone", true, false},
		{"from:acme", false, true},
	}
	for _, tt := range tests {
		for _, countOnly := range []bool{false, true} {
			_, entry, err := f.m.search(t.Context(), &chainmailv1.SearchRequest{QueryString: tt.query, Accounts: itemHostOnly, CountOnly: countOnly})
			if (err == nil) != tt.ok {
				t.Errorf("%s: err = %v, want ok %t", tt.query, err, tt.ok)
			}
			if got := slices.Contains(entry.Targets, "item_op=1"); got != tt.itemOp {
				t.Errorf("%s (count only %t): targets %q, item_op=1 present %t, want %t", tt.query, countOnly, entry.Targets, got, tt.itemOp)
			}
			for _, target := range entry.Targets {
				if strings.Contains(target, "it_") {
					t.Errorf("%s: target %q names an item", tt.query, target)
				}
			}
		}
	}
}

// TestCacheNotReadySeverity checks CACHE_NOT_READY's place next to
// SERVER_UNAVAILABLE when every account failed: above a timeout, below an
// unavailable server.
func TestCacheNotReadySeverity(t *testing.T) {
	t.Parallel()
	unavailable := fmt.Errorf("%w: down", mailstore.ErrUnavailable)
	if got := mostSevere([]error{context.DeadlineExceeded, errItemCacheNotReady}); !errors.Is(got, errNotReady) {
		t.Errorf("mostSevere(timeout, not ready) = %v, want not ready", got)
	}
	if got := mostSevere([]error{errItemCacheNotReady, unavailable}); !errors.Is(got, mailstore.ErrUnavailable) {
		t.Errorf("mostSevere(not ready, unavailable) = %v, want unavailable", got)
	}
}

// TestCacheNotReadyIsFailedPrecondition pins the pairing the CLI and MCP
// rely on: CACHE_NOT_READY travels as Connect FailedPrecondition with the
// cache status hint, however the error is wrapped, text outside the body
// window and a failed cache read with hints of their own, and an unknown
// item is NotFound with the item list hint.
func TestCacheNotReadyIsFailedPrecondition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   connect.Code
		reason string
		hint   string
	}{
		{name: "bare", err: errNotReady, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheNotReadyHint},
		{name: "wrapped", err: fmt.Errorf("host: %w", errNotReady), code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheNotReadyHint},
		{name: "item refusal", err: errItemCacheNotReady, code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheNotReadyHint},
		{name: "item refusal through messageErr", err: messageErr(errItemCacheNotReady), code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheNotReadyHint},
		{name: "text outside the window", err: messageErr(errItemCacheBound), code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheBoundHint},
		{name: "wrapped text outside the window", err: fmt.Errorf("host: %w", errItemCacheBound), code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheBoundHint},
		{name: "cache read failure", err: messageErr(errItemCacheFailed), code: connect.CodeFailedPrecondition, reason: api.ReasonCacheNotReady, hint: cacheFailedHint},
		{name: "unknown item", err: itemLookupErr(fmt.Errorf("%w: item", items.ErrNotFound)), code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toConnectError(tt.err)
			d := api.Detail(got)
			if got.Code() != tt.code || d.GetReason() != tt.reason || d.GetHint() != tt.hint {
				t.Errorf("toConnectError = %v %s (hint %q), want %v %s (hint %q)", got.Code(), d.GetReason(), d.GetHint(), tt.code, tt.reason, tt.hint)
			}
		})
	}
	partials := []struct {
		name string
		err  *fixedTextError
		hint string
	}{
		{name: "not ready", err: errItemCacheNotReady, hint: cacheNotReadyHint},
		{name: "text outside the window", err: errItemCacheBound, hint: cacheBoundHint},
		{name: "cache read failure", err: errItemCacheFailed, hint: cacheFailedHint},
	}
	for _, tt := range partials {
		t.Run("partial "+tt.name, func(t *testing.T) {
			t.Parallel()
			if got := partialSource("host", "", tt.err); got.GetReason() != api.ReasonCacheNotReady || got.GetMessage() != tt.err.Error() || got.GetHint() != tt.hint {
				t.Errorf("partialSource = %v, want CACHE_NOT_READY with %q and hint %q", got, tt.err.Error(), tt.hint)
			}
		})
	}
}
