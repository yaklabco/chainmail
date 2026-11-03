package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

const routeConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
`

// routeNow is the clock of the routing tests: the daemon's, the fakes' and
// the caches'.
var routeNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func routeDaysAgo(n int) time.Time { return routeNow.AddDate(0, 0, -n) }

// routeClock is a watch.Clock stopped at routeNow.
type routeClock struct{}

func (routeClock) Now() time.Time { return routeNow }

func (routeClock) After(time.Duration) <-chan time.Time { return nil }

// routeFixture is a daemon that is never served, a cache store, and a
// cache.Account per account over it, built by hand so routing can be
// tested without running workers. install hands accounts' caches to the
// daemon, as startWatch does.
type routeFixture struct {
	s        *Server
	m        *messageService
	stateDB  *sql.DB
	store    *cache.Store
	fakes    map[string]*fake.Store
	accounts map[string]*cache.Account
	// settled are the accounts settle has run.
	settled map[string]bool
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	fakes := map[string]*fake.Store{"host": fake.New("host", false), "gmail": fake.New("gmail", true)}
	for _, fk := range fakes {
		fk.SetNow(routeClock{}.Now)
	}
	s, stateDB := newTestServerOptions(t, routeConfig, Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store { return fakes[a.Name] },
		Now:      routeClock{}.Now,
	})
	cs, err := cache.Open(t.Context(), filepath.Join(t.TempDir(), "cache.db"), cache.OpenOptions{Now: routeClock{}.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	f := &routeFixture{s: s, m: &messageService{s: s}, stateDB: stateDB, store: cs, fakes: fakes, accounts: map[string]*cache.Account{}, settled: map[string]bool{}}
	for name, fk := range fakes {
		acct, err := s.opts.Config.Account(name)
		if err != nil {
			t.Fatal(err)
		}
		f.accounts[name] = cache.NewAccount(cache.AccountOptions{
			Account:  acct,
			Settings: func() config.CacheSettings { return s.opts.Config.CacheSettings(name) },
			MaxSize:  s.opts.Config.CacheMaxSize,
			Store:    cs,
			Log:      s.watchLog,
			State:    s.watchState,
			Mail:     func(context.Context) (cache.MailStore, func(), error) { return fk, func() {}, nil },
			Clock:    routeClock{},
			Logger:   s.opts.Logger,
		})
	}
	return f
}

// install makes exactly the named accounts' caches the daemon's, each
// brought up first by settle.
func (f *routeFixture) install(t *testing.T, names ...string) {
	t.Helper()
	cacheAccounts := map[string]*cache.Account{}
	for _, n := range names {
		f.settle(t, n)
		cacheAccounts[n] = f.accounts[n]
	}
	f.s.cacheAccounts = cacheAccounts
}

// routeSettleWait bounds settle's wait for an account's builder. Against a
// fake store and an empty log it opens its group at once, so only a broken
// cache reaches it.
const routeSettleWait = 10 * time.Second

// settle runs account's cache workers until Freshness finds every folder
// it tracks fresh, then stops them and waits for them to return. Freshness
// reports stale until the builder has opened its group, so a cache that
// never ran reads as stale. Stopping the workers leaves the tests' own
// edits (stale syncs, open windows, dropped folders) as the only writes
// after warm's. An account settles once per fixture.
func (f *routeFixture) settle(t *testing.T, account string) {
	t.Helper()
	if f.settled[account] {
		return
	}
	f.settled[account] = true
	a := f.accounts[account]
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	folders, err := a.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(routeSettleWait)
	for {
		ok, reason, _, err := a.Freshness(t.Context(), folders)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's cache is not fresh after %v: %s", account, routeSettleWait, reason)
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
}

// warm caches every folder account's default search covers, with bodies,
// as a finished backfill and prefetch leave them, and records each folder
// in sync_folders as synced now: watching for INBOX and Gmail's All Mail,
// polling for the rest.
func (f *routeFixture) warm(t *testing.T, account string) {
	t.Helper()
	ctx := t.Context()
	fk := f.fakes[account]
	results, err := fk.Search(ctx, mailstore.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		headers, err := fk.FetchHeaders(ctx, r.Source, r.UIDs)
		if err != nil {
			t.Fatal(err)
		}
		rows := make([]cache.Row, len(headers))
		refs := make([]ref.Ref, len(headers))
		for i, h := range headers {
			rows[i], refs[i] = cache.RowFromHeader(h), h.Summary.Ref
		}
		if err := f.store.PutRows(ctx, rows); err != nil {
			t.Fatal(err)
		}
		if len(refs) > 0 {
			for _, res := range fk.FetchMessages(ctx, refs) {
				if res.Err != nil {
					t.Fatal(res.Err)
				}
				if _, err := f.store.PutBody(ctx, res.Message.Summary.Ref, cache.Body{Raw: res.Message.Raw, TextClipped: res.Message.TextClipped}, false); err != nil {
					t.Fatal(err)
				}
			}
		}
		next := uint32(1)
		if len(r.UIDs) > 0 {
			next = slices.Max(r.UIDs) + 1
		}
		if err := f.store.PutProgress(ctx, cache.Progress{
			Account: account, Folder: r.Source.Folder, UIDValidity: r.Source.UIDValidity, BaselineUIDNext: next,
			BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: routeNow,
		}); err != nil {
			t.Fatal(err)
		}
		mode, state := watch.ModePoll, watch.StatePolling
		if r.Source.Folder == "INBOX" || account == "gmail" {
			mode, state = watch.ModeIdle, watch.StateWatching
		}
		f.putSync(t, watch.FolderRow{
			Account: account, Folder: r.Source.Folder, UIDValidity: r.Source.UIDValidity, UIDNext: next,
			Mode: mode, State: state, LastSyncAt: routeNow,
		})
	}
}

func (f *routeFixture) putSync(t *testing.T, row watch.FolderRow) {
	t.Helper()
	tx, err := f.stateDB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.PutFolder(t.Context(), tx, row); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// ageSync makes folder's last successful pass age old.
func (f *routeFixture) ageSync(t *testing.T, account, folder string, age time.Duration) {
	t.Helper()
	row, ok, err := f.s.watchState.Folder(t.Context(), account, folder)
	if err != nil || !ok {
		t.Fatalf("sync row %s/%s: %v, %t", account, folder, err, ok)
	}
	row.LastSyncAt = routeNow.Add(-age)
	f.putSync(t, row)
}

// editProgress rewrites folder's cache progress with edit.
func (f *routeFixture) editProgress(t *testing.T, account, folder string, edit func(*cache.Progress)) {
	t.Helper()
	p, ok, err := f.store.Progress(t.Context(), account, folder)
	if err != nil || !ok {
		t.Fatalf("progress %s/%s: %v, %t", account, folder, err, ok)
	}
	edit(&p)
	if err := f.store.PutProgress(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

func routeAddr(name, email string) []mailstore.Address {
	return []mailstore.Address{{Name: name, Email: email}}
}

// seedRouteMail adds the routing tests' mail, oldest first: on host a
// thread across INBOX and Sent, invoices from acme (one with a PDF) and a
// long message; on gmail two invoices and a thread in All Mail.
func seedRouteMail(f *routeFixture) {
	host, gmail := f.fakes["host"], f.fakes["gmail"]
	acme := routeAddr("Acme Billing", "billing@acme.example")
	host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(30), Subject: "old plan", MessageID: "p@x", Body: "notes about the plan", From: routeAddr("Alex", "alex@example.com")})
	host.Add(fake.Message{
		Folder: "Archive", Date: routeDaysAgo(20), Subject: "invoice 41", Body: "the invoice is attached", From: acme,
		Attachments: []fake.Attachment{{Part: "2", Filename: "invoice-41.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")}},
	})
	host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(10), Subject: "re: old plan", MessageID: "p1@x", References: []string{"p@x"}, Body: "agreed on the plan"})
	host.Add(fake.Message{Folder: "Sent", Date: routeDaysAgo(5), Subject: "re: re: old plan", MessageID: "p2@x", References: []string{"p@x", "p1@x"}, Body: "done"})
	host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(3), Subject: "invoice 42", Body: "please pay the invoice", From: acme})
	host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(1), Subject: "lunch", Body: strings.Repeat("long text ", 300)})
	gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: routeDaysAgo(25), Subject: "invoice 40", Body: "an old invoice", From: acme, Labels: []string{`\Inbox`}})
	gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: routeDaysAgo(4), Subject: "trip", MessageID: "t@x", Body: "the trip plan", Labels: []string{`\Inbox`}})
	gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: routeDaysAgo(2), Subject: "re: trip", MessageID: "t1@x", References: []string{"t@x"}, Body: "sounds good"})
}

// search runs one page as the handler does, without the audit write.
func (f *routeFixture) search(t *testing.T, req *chainmailv1.SearchRequest) *chainmailv1.SearchResponse {
	t.Helper()
	resp, _, err := f.m.search(t.Context(), req)
	if err != nil {
		t.Fatalf("search %v: %v", req, err)
	}
	return resp
}

func routeKind(src searchSource) string {
	switch src.(type) {
	case *cache.View:
		return "view"
	case noSource:
		return "none"
	case refusal:
		return "refusal"
	default:
		return "server"
	}
}

// TestSearcherRoutes checks spec 6.1 and 6.4 per account: the cache answers
// when every folder in scope is fresh and it can answer the whole query,
// the server otherwise, and a forced cache answers anyway with the first
// failing reason.
func TestSearcherRoutes(t *testing.T) {
	t.Parallel()
	server := routeInfo{Source: cache.SourceServer, Complete: true}
	complete := routeInfo{Source: cache.SourceCache, Complete: true}
	incomplete := func(reason string) routeInfo { return routeInfo{Source: cache.SourceCache, Reason: reason} }
	staleArchive := func(t *testing.T, f *routeFixture) { f.ageSync(t, "host", "Archive", time.Hour) }
	oldInbox := func(t *testing.T, f *routeFixture) { f.ageSync(t, "host", "INBOX", 24*time.Hour) }
	backfilling := func(t *testing.T, f *routeFixture) {
		f.editProgress(t, "host", "Archive", func(p *cache.Progress) { p.BackfillDone = false })
	}
	windowOpen := func(t *testing.T, f *routeFixture) {
		f.editProgress(t, "host", "Archive", func(p *cache.Progress) { p.WindowDone = false })
	}
	unsynced := func(t *testing.T, f *routeFixture) {
		if err := f.s.watchState.DropFolder(t.Context(), "host", "Archive"); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name    string
		query   string
		source  string
		noCache bool
		spoil   func(*testing.T, *routeFixture)
		want    string
		route   routeInfo
		// syncAge is how long before routeNow the oldest folder searched last
		// synced, which a complete cache answer reports as synced_at.
		syncAge time.Duration
	}{
		{name: "fresh header query", query: "from:acme", want: "view", route: complete},
		{name: "text with a bound in the window", query: "invoice newer_than:30d", want: "view", route: complete},
		{name: "text without a bound", query: "invoice", want: "server", route: server},
		{name: "forced text without a bound", query: "invoice", source: "cache", want: "view", route: incomplete("text_outside_window")},
		{name: "window not done", query: "invoice newer_than:30d", spoil: windowOpen, want: "server", route: server},
		{name: "forced window not done", query: "invoice newer_than:30d", source: "cache", spoil: windowOpen, want: "view", route: incomplete("text_outside_window")},
		{name: "stale polled folder", query: "from:acme", spoil: staleArchive, want: "server", route: server},
		{name: "forced stale", query: "from:acme", source: "cache", spoil: staleArchive, want: "view", route: incomplete("stale")},
		{name: "watched folder at any age", query: "from:acme", spoil: oldInbox, want: "view", route: complete, syncAge: 24 * time.Hour},
		{name: "backfilling", query: "from:acme", spoil: backfilling, want: "server", route: server},
		{name: "forced backfilling", query: "from:acme", source: "cache", spoil: backfilling, want: "view", route: incomplete("backfilling")},
		{name: "folder not synced", query: "from:acme", spoil: unsynced, want: "server", route: server},
		{name: "forced folder not synced", query: "from:acme", source: "cache", spoil: unsynced, want: "view", route: incomplete("folder_not_synced")},
		{name: "no cache", query: "from:acme", noCache: true, want: "server", route: server},
		{name: "forced no cache", query: "from:acme", source: "cache", noCache: true, want: "none", route: incomplete("cache_disabled")},
		{name: "forced server", query: "from:acme", source: "server", want: "server", route: server},
		{name: "explicit auto", query: "from:acme", source: "auto", want: "view", route: complete},
		{name: "has:attachment fresh", query: "has:attachment", want: "view", route: complete},
		{name: "has:attachment stale", query: "has:attachment", spoil: staleArchive, want: "refusal", route: server},
		{name: "label: without a cache", query: "label:todo", noCache: true, want: "refusal", route: server},
		// A plan the cache cannot evaluate (label:\Seen off Google) goes to
		// the server even when source is cache, whatever the freshness
		// (integration note 15), so its refusal reaches the caller.
		{name: "forced unsupported term fresh", query: `label:\Seen`, source: "cache", want: "refusal", route: server},
		{name: "forced unsupported term stale", query: `label:\Seen`, source: "cache", spoil: staleArchive, want: "refusal", route: server},
		{name: "forced unsupported term backfilling", query: `label:\Seen`, source: "cache", spoil: backfilling, want: "refusal", route: server},
		{name: "forced unsupported term not synced", query: `label:\Seen`, source: "cache", spoil: unsynced, want: "refusal", route: server},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRouteFixture(t)
			seedRouteMail(f)
			f.warm(t, "host")
			if !tt.noCache {
				f.install(t, "host")
			}
			if tt.spoil != nil {
				tt.spoil(t, f)
			}
			p, err := f.m.plan(&chainmailv1.SearchRequest{QueryString: tt.query, Accounts: []string{"host"}, Source: tt.source})
			if err != nil {
				t.Fatal(err)
			}
			src, route := f.m.searcher(t.Context(), p.accounts[0], p, p.source)
			if got := routeKind(src); got != tt.want {
				t.Errorf("routed to %s, want %s", got, tt.want)
			}
			if tt.route.Complete && tt.route.Source == cache.SourceCache && !route.SyncedAt.Equal(routeNow.Add(-tt.syncAge)) {
				t.Errorf("synced_at = %v, want %v", route.SyncedAt, routeNow.Add(-tt.syncAge))
			}
			route.SyncedAt = time.Time{}
			if route != tt.route {
				t.Errorf("route = %+v, want %+v", route, tt.route)
			}
		})
	}
}

// TestSearcherGmailTrash checks that Gmail's Trash, which the cache never
// holds, goes to the server, and a forced cache says why.
func TestSearcherGmailTrash(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "gmail")
	f.install(t, "gmail")
	for source, want := range map[string]routeInfo{
		"":      {Source: cache.SourceServer, Complete: true},
		"cache": {Source: cache.SourceCache, Reason: "folder_not_synced"},
	} {
		p, err := f.m.plan(&chainmailv1.SearchRequest{QueryString: "in:trash", Accounts: []string{"gmail"}, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		_, route := f.m.searcher(t.Context(), p.accounts[0], p, p.source)
		route.SyncedAt = time.Time{}
		if route != want {
			t.Errorf("source %q: route = %+v, want %+v", source, route, want)
		}
	}
}

// TestSearchSourceIsValidated checks that source takes only auto, cache,
// server or nothing, and the hint lists them.
func TestSearchSourceIsValidated(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	for _, source := range []string{"bogus", "Cache", "mixed"} {
		_, _, err := f.m.search(t.Context(), &chainmailv1.SearchRequest{Source: source})
		hint := api.Detail(toConnectError(err)).GetHint()
		if apiReason(err) != api.ReasonInvalidArgument || !strings.Contains(hint, "auto") || !strings.Contains(hint, "cache or server") {
			t.Errorf("source %q: %v (hint %q), want INVALID_ARGUMENT naming the values", source, err, hint)
		}
	}
}

// sameRows reports whether two pages hold the same rows, in order.
func sameRows(a, b []*chainmailv1.MessageRow) bool {
	return slices.EqualFunc(a, b, func(x, y *chainmailv1.MessageRow) bool { return proto.Equal(x, y) })
}

func rowSubjects(rows []*chainmailv1.MessageRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.GetSubject().GetUntrusted())
	}
	return out
}

// bothWays runs req as given and forced to the server.
func (f *routeFixture) bothWays(t *testing.T, req *chainmailv1.SearchRequest) (fromCache, fromServer *chainmailv1.SearchResponse) {
	t.Helper()
	s := proto.Clone(req).(*chainmailv1.SearchRequest)
	s.Source = cache.SourceServer
	return f.search(t, req), f.search(t, s)
}

// storeCalls is how often the fakes ran the calls a cached page avoids.
func (f *routeFixture) storeCalls() int {
	n := 0
	for _, fk := range f.fakes {
		n += fk.Calls("Search") + fk.Calls("FetchSummaries") + fk.Calls("FetchMessages")
	}
	return n
}

// TestSearchFromCacheMatchesServer checks that a warm, fresh cache answers
// header queries, text inside the window, count_only, threads and snippets
// with exactly the server's rows, counts and cursor, without asking the
// server, and says so per account.
func TestSearchFromCacheMatchesServer(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.warm(t, "gmail")
	f.install(t, "host", "gmail")
	reqs := []*chainmailv1.SearchRequest{
		{QueryString: "from:acme"},
		{QueryString: "invoice newer_than:30d"},
		{Limit: 2},
		{QueryString: "newer_than:60d", CountOnly: true},
		{Threads: true},
		{Snippets: true},
		{QueryString: "in:inbox"},
		{QueryString: "-subject:invoice older_than:2d", Limit: 3},
	}
	for _, req := range reqs {
		before := f.storeCalls()
		c := f.search(t, req)
		if n := f.storeCalls() - before; n != 0 {
			t.Errorf("%v: the cached page made %d server calls", req, n)
		}
		_, s := f.bothWays(t, req)
		if c.GetSource() != cache.SourceCache || s.GetSource() != cache.SourceServer {
			t.Errorf("%v: sources %q and %q, want cache and server", req, c.GetSource(), s.GetSource())
		}
		if !sameRows(c.GetRows(), s.GetRows()) {
			t.Errorf("%v: cache rows %v, server rows %v", req, rowSubjects(c.GetRows()), rowSubjects(s.GetRows()))
		}
		if c.GetCount() != s.GetCount() || c.GetNextCursor() != s.GetNextCursor() {
			t.Errorf("%v: cache count %d cursor %q, server count %d cursor %q", req, c.GetCount(), c.GetNextCursor(), s.GetCount(), s.GetNextCursor())
		}
		if len(c.GetAccountCounts()) != len(s.GetAccountCounts()) {
			t.Fatalf("%v: account counts %v vs %v", req, c.GetAccountCounts(), s.GetAccountCounts())
		}
		for i, ac := range c.GetAccountCounts() {
			sc := s.GetAccountCounts()[i]
			if ac.GetAccount() != sc.GetAccount() || ac.GetCount() != sc.GetCount() {
				t.Errorf("%v: count %v, server %v", req, ac, sc)
			}
			if ac.GetSource() != cache.SourceCache || !ac.GetComplete() || ac.GetIncompleteReason() != "" || !ac.GetSyncedAt().AsTime().Equal(routeNow) {
				t.Errorf("%v: cache account count %v, want complete from the cache, synced %v", req, ac, routeNow)
			}
			if sc.GetSource() != cache.SourceServer || !sc.GetComplete() || sc.GetSyncedAt() != nil {
				t.Errorf("%v: server account count %v, want complete from the server", req, sc)
			}
		}
	}
}

// TestSearchCursorCrossesSources checks spec 6.2: a cursor from a cache page
// continues on the server, and the reverse, with the rows an all-server run
// gives, none missed or repeated.
func TestSearchCursorCrossesSources(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.warm(t, "gmail")
	f.install(t, "host", "gmail")
	page1Cache, page1Server := f.bothWays(t, &chainmailv1.SearchRequest{Limit: 2})
	if page1Cache.GetNextCursor() == "" || page1Cache.GetNextCursor() != page1Server.GetNextCursor() {
		t.Fatalf("page 1 cursors: cache %q, server %q", page1Cache.GetNextCursor(), page1Server.GetNextCursor())
	}
	want := f.search(t, &chainmailv1.SearchRequest{Limit: 2, Cursor: page1Server.GetNextCursor(), Source: cache.SourceServer})
	onServer := f.search(t, &chainmailv1.SearchRequest{Limit: 2, Cursor: page1Cache.GetNextCursor(), Source: cache.SourceServer})
	fromCache := f.search(t, &chainmailv1.SearchRequest{Limit: 2, Cursor: page1Server.GetNextCursor()})
	if !sameRows(onServer.GetRows(), want.GetRows()) || !sameRows(fromCache.GetRows(), want.GetRows()) {
		t.Errorf("page 2: cache cursor on the server %v, server cursor from the cache %v, want %v",
			rowSubjects(onServer.GetRows()), rowSubjects(fromCache.GetRows()), rowSubjects(want.GetRows()))
	}
	if fromCache.GetSource() != cache.SourceCache {
		t.Errorf("page 2 from the cache: source %q", fromCache.GetSource())
	}
	pageAll := func(sources ...string) []string {
		var got []string
		req := &chainmailv1.SearchRequest{Limit: 2}
		for i := 0; i < 20; i++ {
			req.Source = sources[i%len(sources)]
			resp := f.search(t, req)
			got = append(got, rowSubjects(resp.GetRows())...)
			if resp.GetNextCursor() == "" {
				return got
			}
			req.Cursor = resp.GetNextCursor()
		}
		t.Fatal("still paging after 20 pages")
		return nil
	}
	all := pageAll(cache.SourceServer)
	if got := pageAll("", cache.SourceServer); !slices.Equal(got, all) {
		t.Errorf("alternating cache and server = %v, want %v", got, all)
	}
	if got := pageAll(cache.SourceServer, ""); !slices.Equal(got, all) {
		t.Errorf("alternating server and cache = %v, want %v", got, all)
	}
}

// TestSearchMixesSourcesPerAccount checks that accounts are routed on
// their own and the response says mixed when both sources answered.
func TestSearchMixesSourcesPerAccount(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.install(t, "host")
	resp, entry, err := f.m.search(t.Context(), &chainmailv1.SearchRequest{QueryString: "invoice newer_than:30d"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSource() != cache.SourceMixed {
		t.Errorf("source = %q, want mixed", resp.GetSource())
	}
	sources := map[string]string{}
	for _, c := range resp.GetAccountCounts() {
		sources[c.GetAccount()] = c.GetSource()
	}
	if sources["host"] != cache.SourceCache || sources["gmail"] != cache.SourceServer {
		t.Errorf("account sources = %v, want host from the cache and gmail from the server", sources)
	}
	// host's invoices 42 and 41 from the cache, gmail's invoice 40 from the server.
	if want := []string{"accounts=2", "rows=3", "source=mixed"}; !slices.Equal(entry.Targets, want) {
		t.Errorf("audit targets = %v, want %v", entry.Targets, want)
	}
}

// TestSearchForcedCacheIsIncomplete checks spec 6.4: a forced cache answers
// what it holds, flags the account incomplete with its reason, and an
// account without a cache returns no rows and cache_disabled, not an error.
func TestSearchForcedCacheIsIncomplete(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.install(t, "host")
	resp, entry, err := f.m.search(t.Context(), &chainmailv1.SearchRequest{QueryString: "invoice", Source: cache.SourceCache})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowSubjects(resp.GetRows()); !slices.Equal(got, []string{"invoice 42", "invoice 41"}) {
		t.Errorf("rows = %v, want host's invoices from cached bodies", got)
	}
	if len(resp.GetPartial()) != 0 || resp.GetSource() != cache.SourceCache {
		t.Errorf("partial %v, source %q", resp.GetPartial(), resp.GetSource())
	}
	byAccount := map[string]*chainmailv1.AccountCount{}
	for _, c := range resp.GetAccountCounts() {
		byAccount[c.GetAccount()] = c
	}
	if c := byAccount["host"]; c.GetComplete() || c.GetIncompleteReason() != "text_outside_window" || c.GetCount() != 2 {
		t.Errorf("host = %v, want 2, incomplete: text_outside_window", c)
	}
	if c := byAccount["gmail"]; c == nil || c.GetComplete() || c.GetIncompleteReason() != "cache_disabled" || c.GetCount() != 0 || c.GetSource() != cache.SourceCache {
		t.Errorf("gmail = %v, want 0 from the cache, incomplete: cache_disabled", c)
	}
	if entry.Targets[len(entry.Targets)-1] != "source=cache" {
		t.Errorf("audit targets = %v", entry.Targets)
	}
}

// TestSearchForcedCacheCarriesFoldersItDoesNotHold checks that a forced
// cache page continuing a server cursor carries the cursor sources of
// folders the cache does not hold, so a later server page still returns
// their rows.
func TestSearchForcedCacheCarriesFoldersItDoesNotHold(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.install(t, "host")
	host := []string{"host"}
	all := []string{}
	req := &chainmailv1.SearchRequest{Accounts: host, Limit: 1, Source: cache.SourceServer}
	for {
		resp := f.search(t, req)
		all = append(all, rowSubjects(resp.GetRows())...)
		if resp.GetNextCursor() == "" {
			break
		}
		req.Cursor = resp.GetNextCursor()
	}
	page1 := f.search(t, &chainmailv1.SearchRequest{Accounts: host, Limit: 1, Source: cache.SourceServer})
	if err := f.store.DropFolder(t.Context(), "host", "Archive", false); err != nil {
		t.Fatal(err)
	}
	got := rowSubjects(page1.GetRows())
	req = &chainmailv1.SearchRequest{Accounts: host, Limit: 1, Cursor: page1.GetNextCursor(), Source: cache.SourceCache}
	resp := f.search(t, req)
	got = append(got, rowSubjects(resp.GetRows())...)
	if !slices.ContainsFunc(cursorSources(t, resp.GetNextCursor()), func(s ref.CursorSource) bool { return s.Folder == "Archive" }) {
		t.Fatalf("forced cache cursor %+v lost Archive", cursorSources(t, resp.GetNextCursor()))
	}
	req = &chainmailv1.SearchRequest{Accounts: host, Limit: 1, Cursor: resp.GetNextCursor(), Source: cache.SourceServer}
	for req.Cursor != "" {
		resp := f.search(t, req)
		got = append(got, rowSubjects(resp.GetRows())...)
		req.Cursor = resp.GetNextCursor()
	}
	slices.Sort(got)
	slices.Sort(all)
	if !slices.Equal(got, all) {
		t.Errorf("rows across the pages = %v, want %v", got, all)
	}
}

// TestSearchCacheOnlyOperatorsOnStandardAccounts checks spec 6.6: has: and
// filename: on an account other than Google come from its cache when it
// can answer, and are refused with a hint naming cache status when not.
func TestSearchCacheOnlyOperatorsOnStandardAccounts(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	seedRouteMail(f)
	f.warm(t, "host")
	f.install(t, "host")
	for _, q := range []string{"has:attachment", "filename:pdf"} {
		resp := f.search(t, &chainmailv1.SearchRequest{QueryString: q, Accounts: []string{"host"}})
		if got := rowSubjects(resp.GetRows()); !slices.Equal(got, []string{"invoice 41"}) || resp.GetSource() != cache.SourceCache {
			t.Errorf("%s from the cache = %v, source %q", q, got, resp.GetSource())
		}
	}
	f.install(t)
	_, _, err := f.m.search(t.Context(), &chainmailv1.SearchRequest{QueryString: "has:attachment", Accounts: []string{"host"}})
	hint := api.Detail(toConnectError(err)).GetHint()
	if apiReason(err) != api.ReasonInvalidArgument || !strings.Contains(hint, "chainmail cache status") {
		t.Errorf("without a cache: %v (hint %q), want INVALID_ARGUMENT pointing at cache status", err, hint)
	}
}
