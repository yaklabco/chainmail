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

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/watch"
)

const getConfig = `
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

// getNow is the clock of the read-through tests: the daemon's, the fakes'
// and the caches'.
var getNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func getDaysAgo(n int) time.Time { return getNow.AddDate(0, 0, -n) }

// getClock is a watch.Clock stopped at getNow.
type getClock struct{}

func (getClock) Now() time.Time { return getNow }

func (getClock) After(time.Duration) <-chan time.Time { return nil }

// getFixture is a daemon that is never served, a cache store, and a
// settled cache.Account per account over it, built by hand. install hands
// accounts' caches to the daemon, as startWatch does.
type getFixture struct {
	s        *Server
	m        *messageService
	stateDB  *sql.DB
	store    *cache.Store
	fakes    map[string]*fake.Store
	accounts map[string]*cache.Account
}

func newGetFixture(t *testing.T) *getFixture {
	t.Helper()
	fakes := map[string]*fake.Store{"host": fake.New("host", false), "gmail": fake.New("gmail", true)}
	for _, fk := range fakes {
		fk.SetNow(getClock{}.Now)
	}
	s, stateDB := newTestServerOptions(t, getConfig, Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store { return fakes[a.Name] },
		Now:      getClock{}.Now,
	})
	cs, err := cache.Open(t.Context(), filepath.Join(t.TempDir(), "cache.db"), cache.OpenOptions{Now: getClock{}.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	f := &getFixture{s: s, m: &messageService{s: s}, stateDB: stateDB, store: cs, fakes: fakes, accounts: map[string]*cache.Account{}}
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
			Clock:    getClock{},
			Logger:   s.opts.Logger,
		})
	}
	f.settleRead(t)
	return f
}

// settleReadWait bounds the wait for the fixture's cache accounts to open
// their groups, and settleReadPoll spaces the checks. Nothing else is
// running, so only a broken account reaches the bound.
const (
	settleReadWait = 10 * time.Second
	settleReadPoll = 5 * time.Millisecond
)

// settleRead runs every account's cache workers, as startCacheAccounts
// does, until each account reads as fresh, then stops them and waits for
// them to return. Freshness reports stale until the builder has opened its
// group, which only a running account does. The builder opens it before
// any mail or sync row exists, so it tracks nothing, and with no worker
// left running nothing undoes the rows and progress the tests write, and
// the tests' server call counts see only their own reads.
func (f *getFixture) settleRead(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for _, a := range f.accounts {
		wg.Go(func() { a.Run(ctx) })
	}
	deadline := time.Now().Add(settleReadWait)
	for name, a := range f.accounts {
		for {
			ok, reason, _, err := a.Freshness(ctx, nil)
			if err != nil {
				t.Fatalf("freshness of %s: %v", name, err)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the cache of %s is not fresh after %v: %s", name, settleReadWait, reason)
			}
			time.Sleep(settleReadPoll)
		}
	}
}

// install makes exactly the named accounts' caches the daemon's.
func (f *getFixture) install(names ...string) {
	f.s.cacheAccounts = map[string]*cache.Account{}
	for _, n := range names {
		f.s.cacheAccounts[n] = f.accounts[n]
	}
}

// warm caches every folder account's default search covers, with bodies.
func (f *getFixture) warm(t *testing.T, account string) {
	t.Helper()
	results, err := f.fakes[account].Search(t.Context(), mailstore.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		f.cacheFolder(t, account, r.Source, r.UIDs, true)
	}
}

// cacheFolder caches uids of src, with bodies when bodies is set, marks the
// folder's backfill and window done, and records it in sync_folders as
// synced now: watching for INBOX and Gmail's All Mail, polling otherwise.
func (f *getFixture) cacheFolder(t *testing.T, account string, src mailstore.Source, uids []uint32, bodies bool) {
	t.Helper()
	ctx := t.Context()
	fk := f.fakes[account]
	headers, err := fk.FetchHeaders(ctx, src, uids)
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
	if bodies && len(refs) > 0 {
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
	if len(uids) > 0 {
		next = slices.Max(uids) + 1
	}
	if err := f.store.PutProgress(ctx, cache.Progress{
		Account: account, Folder: src.Folder, UIDValidity: src.UIDValidity, BaselineUIDNext: next,
		BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: getNow,
	}); err != nil {
		t.Fatal(err)
	}
	mode, state := watch.ModePoll, watch.StatePolling
	if src.Folder == "INBOX" || account == "gmail" {
		mode, state = watch.ModeIdle, watch.StateWatching
	}
	tx, err := f.stateDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.PutFolder(ctx, tx, watch.FolderRow{
		Account: account, Folder: src.Folder, UIDValidity: src.UIDValidity, UIDNext: next,
		Mode: mode, State: state, LastSyncAt: getNow,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// ageSync makes a polled folder stale: its last pass an hour old.
func (f *getFixture) ageSync(t *testing.T, account, folder string) {
	t.Helper()
	row, ok, err := f.s.watchState.Folder(t.Context(), account, folder)
	if err != nil || !ok {
		t.Fatalf("sync row %s/%s: %v, %t", account, folder, err, ok)
	}
	row.LastSyncAt = getNow.Add(-time.Hour)
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

const getQuotedReply = "Thanks.\n\nOn Mon, 26 Oct 2026 at 09:00, Alex Doe <alex@example.com> wrote:\n> are we on?"

// seedGetMail adds the read-through tests' mail and returns refs by
// subject: on host a thread rooted at p@x across INBOX and Sent, a quoted
// reply, a body past the 1 KiB limit and a message with an attachment; on
// gmail one message in All Mail.
func seedGetMail(f *getFixture) map[string]ref.Ref {
	host, gmail := f.fakes["host"], f.fakes["gmail"]
	refs := map[string]ref.Ref{}
	add := func(s *fake.Store, m fake.Message) { refs[m.Subject] = s.Add(m) }
	add(host, fake.Message{Folder: "INBOX", Date: getDaysAgo(30), Subject: "plan", MessageID: "p@x", Body: "plan?"})
	add(host, fake.Message{Folder: "Sent", Date: getDaysAgo(20), Subject: "re: plan", MessageID: "p1@x", References: []string{"p@x"}, Body: getQuotedReply, Flags: []string{`\Seen`}})
	add(host, fake.Message{Folder: "INBOX", Date: getDaysAgo(10), Subject: "re: re: plan", MessageID: "p2@x", References: []string{"p@x", "p1@x"}, Body: "ok"})
	add(host, fake.Message{Folder: "Archive", Date: getDaysAgo(5), Subject: "long", MessageID: "l@x", Body: strings.Repeat("x", 3000)})
	add(host, fake.Message{
		Folder: "INBOX", Date: getDaysAgo(2), Subject: "with pdf", MessageID: "a@x", Body: "see attached",
		Attachments: []fake.Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")}},
	})
	add(gmail, fake.Message{Folder: "[Gmail]/All Mail", Date: getDaysAgo(3), Subject: "gmail note", MessageID: "g@x", Body: "from gmail", Labels: []string{`\Inbox`}})
	return refs
}

func getRefs(refs map[string]ref.Ref, subjects ...string) []string {
	out := make([]string, len(subjects))
	for i, s := range subjects {
		out[i] = refs[s].String()
	}
	return out
}

// withoutSource returns a copy of items with each message's source cleared,
// to compare a cached answer with the server's.
func withoutSource(items []*chainmailv1.GetItem) []*chainmailv1.GetItem {
	out := make([]*chainmailv1.GetItem, len(items))
	for i, it := range items {
		c := proto.Clone(it).(*chainmailv1.GetItem)
		if msg := c.GetMessage(); msg != nil {
			msg.Source = ""
		}
		out[i] = c
	}
	return out
}

// TestGetFromCacheMatchesServer checks spec 7.2 and done-when 2: a get of
// cached messages in fresh folders makes no server call and returns, with
// and without full, exactly what the server path returns: body, body_limit
// truncation, quote trimming, headers and attachments.
func TestGetFromCacheMatchesServer(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	list := getRefs(refs, "plan", "re: plan", "long", "with pdf", "re: plan")
	host := f.fakes["host"]
	for _, full := range []bool{false, true} {
		f.install("host")
		fetches, summaries := host.Calls("FetchMessages"), host.Calls("FetchSummaries")
		cached, entry, err := f.m.get(t.Context(), &chainmailv1.GetRequest{Refs: list, Full: full})
		if err != nil {
			t.Fatal(err)
		}
		if n, m := host.Calls("FetchMessages")-fetches, host.Calls("FetchSummaries")-summaries; n != 0 || m != 0 {
			t.Errorf("full=%t: the cached get made %d FetchMessages and %d FetchSummaries calls", full, n, m)
		}
		f.install()
		server, _, err := f.m.get(t.Context(), &chainmailv1.GetRequest{Refs: list, Full: full})
		if err != nil {
			t.Fatal(err)
		}
		got, want := withoutSource(cached.GetItems()), withoutSource(server.GetItems())
		if len(got) != len(want) {
			t.Fatalf("full=%t: %d cached items, %d from the server", full, len(got), len(want))
		}
		for i := range want {
			if !proto.Equal(got[i], want[i]) {
				t.Errorf("full=%t item %d:\ncache  %v\nserver %v", full, i, got[i], want[i])
			}
			if s := cached.GetItems()[i].GetMessage().GetSource(); s != cache.SourceCache {
				t.Errorf("full=%t item %d source = %q, want cache", full, i, s)
			}
			if s := server.GetItems()[i].GetMessage().GetSource(); s != cache.SourceServer {
				t.Errorf("full=%t item %d server source = %q", full, i, s)
			}
		}
		if cached.GetSource() != cache.SourceCache || server.GetSource() != cache.SourceServer {
			t.Errorf("full=%t: sources %q and %q", full, cached.GetSource(), server.GetSource())
		}
		if last := entry.Targets[len(entry.Targets)-1]; last != "source=cache" {
			t.Errorf("full=%t audit targets = %v", full, entry.Targets)
		}
	}
}

// TestGetMixesSources checks that messages of an account without a cache
// come from the server in the same call, so the call reports mixed.
func TestGetMixesSources(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	f.install("host")
	resp, entry, err := f.m.get(t.Context(), &chainmailv1.GetRequest{Refs: getRefs(refs, "plan", "gmail note")})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := resp.GetItems()[0].GetMessage().GetSource(), resp.GetItems()[1].GetMessage().GetSource(); a != cache.SourceCache || b != cache.SourceServer {
		t.Errorf("sources = %q, %q, want cache then server", a, b)
	}
	if resp.GetSource() != cache.SourceMixed {
		t.Errorf("source = %q, want mixed", resp.GetSource())
	}
	if want := []string{refs["plan"].String(), refs["gmail note"].String(), "messages=2", "errors=0", "source=mixed"}; !slices.Equal(entry.Targets, want) {
		t.Errorf("audit targets = %v, want %v", entry.Targets, want)
	}
}

// TestGetMissIsCachedForTheNextGet checks that a message the cache lacks
// comes from the server once and from the cache after.
func TestGetMissIsCachedForTheNextGet(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	seedGetMail(f)
	f.warm(t, "host")
	f.install("host")
	host := f.fakes["host"]
	late := host.Add(fake.Message{Folder: "INBOX", Date: getDaysAgo(0), Subject: "late", Body: "arrived after the prefetch"})
	for i, want := range []string{cache.SourceServer, cache.SourceCache} {
		fetches := host.Calls("FetchMessages")
		resp, _, err := f.m.get(t.Context(), &chainmailv1.GetRequest{Refs: []string{late.String()}})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.GetItems()[0].GetMessage().GetSource(); got != want {
			t.Errorf("get %d source = %q, want %q", i+1, got, want)
		}
		wantFetches := 0
		if want == cache.SourceServer {
			wantFetches = 1
		}
		if n := host.Calls("FetchMessages") - fetches; n != wantFetches {
			t.Errorf("get %d made %d FetchMessages calls, want %d", i+1, n, wantFetches)
		}
	}
}

// TestGetRecoversAStaleRefByMessageID checks spec 7.3 through get: the
// item keeps the ref asked for, and its message carries the new ref.
func TestGetRecoversAStaleRefByMessageID(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	f.install("host")
	old := refs["with pdf"]
	moved := f.renumber(t, "host", "INBOX", "a@x")
	resp, _, err := f.m.get(t.Context(), &chainmailv1.GetRequest{Refs: []string{old.String()}})
	if err != nil {
		t.Fatal(err)
	}
	it := resp.GetItems()[0]
	if it.GetRef() != old.String() || it.GetMessage().GetRef() != moved.String() || it.GetMessage().GetSubject().GetUntrusted() != "with pdf" {
		t.Errorf("item = ref %q, message ref %q, error %v; want %q recovered to %q", it.GetRef(), it.GetMessage().GetRef(), it.GetError(), old, moved)
	}
}

// renumber gives folder of account a new UIDVALIDITY on the server, as a
// server rebuild does, and has the cache drop the folder, keeping its
// Message-IDs for recovery, then cache its rows under the new UIDVALIDITY
// without bodies. It returns the new ref of the message with messageID.
func (f *getFixture) renumber(t *testing.T, account, folder, messageID string) ref.Ref {
	t.Helper()
	fk := f.fakes[account]
	fk.NewUIDValidity(folder)
	if err := f.store.DropFolder(t.Context(), account, folder, true); err != nil {
		t.Fatal(err)
	}
	res, err := fk.Search(t.Context(), mailstore.SearchRequest{In: folder})
	if err != nil {
		t.Fatal(err)
	}
	f.cacheFolder(t, account, res[0].Source, res[0].UIDs, false)
	found, err := fk.FindByMessageID(t.Context(), folder, messageID)
	if err != nil || len(found) != 1 {
		t.Fatalf("server lookup of %s: %v, %v", messageID, found, err)
	}
	return found[0]
}

// withoutSources returns copies of msgs with their source cleared.
func withoutSources(msgs []*chainmailv1.Message) []*chainmailv1.Message {
	out := make([]*chainmailv1.Message, len(msgs))
	for i, msg := range msgs {
		c := proto.Clone(msg).(*chainmailv1.Message)
		c.Source = ""
		out[i] = c
	}
	return out
}

// TestGetThreadFromCacheMatchesServer checks spec 7.2: with every folder
// fresh, a thread's membership and messages come from the cache, with no
// server call, in the server path's order and rendering.
func TestGetThreadFromCacheMatchesServer(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	host := f.fakes["host"]
	req := &chainmailv1.GetThreadRequest{Ref: refs["re: plan"].String()}
	f.install("host")
	threads, fetches := host.Calls("Thread"), host.Calls("FetchMessages")
	var entry audit.Entry
	cached, err := f.m.thread(t.Context(), req, &entry)
	if err != nil {
		t.Fatal(err)
	}
	if n, m := host.Calls("Thread")-threads, host.Calls("FetchMessages")-fetches; n != 0 || m != 0 {
		t.Errorf("the cached thread made %d Thread and %d FetchMessages calls", n, m)
	}
	f.install()
	var serverEntry audit.Entry
	server, err := f.m.thread(t.Context(), req, &serverEntry)
	if err != nil {
		t.Fatal(err)
	}
	got, want := withoutSources(cached.GetMessages()), withoutSources(server.GetMessages())
	if len(got) != 3 || !slices.EqualFunc(got, want, func(a, b *chainmailv1.Message) bool { return proto.Equal(a, b) }) {
		t.Errorf("cached thread differs from the server's:\ncache  %v\nserver %v", got, want)
	}
	if cached.GetThreadId() != server.GetThreadId() || cached.GetSource() != cache.SourceCache || server.GetSource() != cache.SourceServer {
		t.Errorf("thread id %q vs %q, sources %q and %q", cached.GetThreadId(), server.GetThreadId(), cached.GetSource(), server.GetSource())
	}
	if want := []string{refs["re: plan"].String(), "messages=3", "source=cache"}; !slices.Equal(entry.Targets, want) {
		t.Errorf("audit targets = %v, want %v", entry.Targets, want)
	}
}

// TestGetThreadMixesSourcesWhenTheScopeIsStale checks that a stale folder
// in the default scope sends membership to the server while the members,
// in fresh folders, still come from the cache, so the thread reports mixed.
func TestGetThreadMixesSourcesWhenTheScopeIsStale(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	f.install("host")
	f.ageSync(t, "host", "Archive")
	host := f.fakes["host"]
	threads := host.Calls("Thread")
	var entry audit.Entry
	resp, err := f.m.thread(t.Context(), &chainmailv1.GetThreadRequest{Ref: refs["plan"].String()}, &entry)
	if err != nil {
		t.Fatal(err)
	}
	if host.Calls("Thread")-threads != 1 {
		t.Errorf("membership did not come from the server")
	}
	if len(resp.GetMessages()) != 3 {
		t.Errorf("thread has %d messages, want 3", len(resp.GetMessages()))
	}
	for _, msg := range resp.GetMessages() {
		if msg.GetSource() != cache.SourceCache {
			t.Errorf("%s source = %q, want cache", msg.GetSubject().GetUntrusted(), msg.GetSource())
		}
	}
	if resp.GetSource() != cache.SourceMixed {
		t.Errorf("source = %q, want mixed", resp.GetSource())
	}
	if want := []string{refs["plan"].String(), "messages=3", "source=mixed"}; !slices.Equal(entry.Targets, want) {
		t.Errorf("audit targets = %v, want %v", entry.Targets, want)
	}
}

// TestGetThreadTakesItsIDFromTheRecoveredSeed checks that a thread whose
// stale seed is recovered by Message-ID (integration note 18) takes its
// thread_id from the recovered seed, not from its oldest member: aside is
// older than plan and in plan's thread, but its own References start with
// other@x, so its thread ID differs. The audit still names the ref asked
// for.
func TestGetThreadTakesItsIDFromTheRecoveredSeed(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	host := f.fakes["host"]
	host.Add(fake.Message{Folder: "Archive", Date: getDaysAgo(40), Subject: "aside", MessageID: "as@x", References: []string{"other@x", "p@x"}, Body: "aside"})
	f.warm(t, "host")
	f.install("host")
	old := refs["plan"]
	moved := f.renumber(t, "host", "INBOX", "p@x")
	seed := host.FetchMessages(t.Context(), []ref.Ref{moved})[0]
	if seed.Err != nil {
		t.Fatal(seed.Err)
	}
	var entry audit.Entry
	resp, err := f.m.thread(t.Context(), &chainmailv1.GetThreadRequest{Ref: old.String()}, &entry)
	if err != nil {
		t.Fatal(err)
	}
	subjects := make([]string, len(resp.GetMessages()))
	for i, msg := range resp.GetMessages() {
		subjects[i] = msg.GetSubject().GetUntrusted()
	}
	if want := []string{"aside", "plan", "re: plan", "re: re: plan"}; !slices.Equal(subjects, want) {
		t.Fatalf("members = %v, want %v", subjects, want)
	}
	if aside := resp.GetMessages()[0].GetThreadId(); aside == seed.Message.Summary.ThreadID {
		t.Fatalf("aside's thread ID %q is the seed's, so the test proves nothing", aside)
	}
	if resp.GetThreadId() != seed.Message.Summary.ThreadID {
		t.Errorf("thread_id = %q, want the recovered seed's %q", resp.GetThreadId(), seed.Message.Summary.ThreadID)
	}
	if len(entry.Targets) == 0 || entry.Targets[0] != old.String() {
		t.Errorf("audit targets = %v, want the requested %s first", entry.Targets, old)
	}
}

// TestGetThreadFailsWhenTheRecoveredSeedVanishes checks that a recovered
// seed that is gone by the time its thread's messages are read fails the
// call with STALE_REF, as a seed that was never recovered does, instead of
// being left out as an ordinary member. The cache still lists the seed:
// it has not heard of the expunge.
func TestGetThreadFailsWhenTheRecoveredSeedVanishes(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	f.install("host")
	moved := f.renumber(t, "host", "INBOX", "p@x")
	f.fakes["host"].ExternalExpunge("INBOX", moved.UID)
	var entry audit.Entry
	resp, err := f.m.thread(t.Context(), &chainmailv1.GetThreadRequest{Ref: refs["plan"].String()}, &entry)
	if got := apiReason(err); got != api.ReasonStaleRef {
		t.Errorf("thread = %d messages, error %v (reason %q), want STALE_REF", len(resp.GetMessages()), err, got)
	}
}

// TestGetReadsTheServerWhileTheCacheIsDown checks that an account whose
// cache is down reads from the server even for a cached message.
func TestGetReadsTheServerWhileTheCacheIsDown(t *testing.T) {
	t.Parallel()
	f := newGetFixture(t)
	refs := seedGetMail(f)
	f.warm(t, "host")
	f.install("host")
	f.s.cacheDown.Store(true)
	host := f.fakes["host"]
	fetches := host.Calls("FetchMessages")
	resp, entry, err := f.m.get(t.Context(), &chainmailv1.GetRequest{Refs: getRefs(refs, "plan")})
	if err != nil {
		t.Fatal(err)
	}
	if n := host.Calls("FetchMessages") - fetches; n != 1 {
		t.Errorf("FetchMessages calls = %d, want 1", n)
	}
	if s := resp.GetItems()[0].GetMessage().GetSource(); s != cache.SourceServer || resp.GetSource() != cache.SourceServer {
		t.Errorf("sources = %q and %q, want server", s, resp.GetSource())
	}
	if last := entry.Targets[len(entry.Targets)-1]; last != "source=server" {
		t.Errorf("audit targets = %v", entry.Targets)
	}
}
