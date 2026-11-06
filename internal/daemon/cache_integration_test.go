//go:build integration

package daemon_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/imapstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/dovecot"
)

// cacheDoveConfig is one account on Dovecot with the default sync set:
// without sync.folders a non-Google account syncs every folder a default
// search covers, so the cache can answer default searches. Polled folders
// sync every 30 seconds, the shortest sync.poll, and the cache backfills in
// small batches without pausing, so the test mailbox is ready in seconds.
// The body window is 30 days. The SMTP endpoint is never dialled.
const cacheDoveConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = 1, security = "starttls" }
sync = { poll = "30s" }

[cache]
prefetch_bodies = "30d"
backfill_batch = 10
backfill_pause = "0s"
`

const (
	cacheUser = "cache@example.test"
	// cacheReadyWithin bounds a backfill and prefetch of the test mailbox,
	// including the first pass of each polled folder.
	cacheReadyWithin = 90 * time.Second
	// cacheFollowWithin covers a new or flags event and the builder's
	// header fetch.
	cacheFollowWithin = 10 * time.Second
	// cacheExpungeWithin covers the sweep a server EXPUNGE triggers, at
	// most doveSweepFloor after the one before, and the builder's apply.
	cacheExpungeWithin = 20 * time.Second
	// cacheRecoverWithin covers a UIDVALIDITY change: reconnect, gap,
	// rebuild of the folder and its backfill.
	cacheRecoverWithin = 60 * time.Second
	// cachePageRows is small so every query spans several pages.
	cachePageRows = 4
)

// cacheFolders are the folders the cache tracks on Dovecot: every folder
// a default search covers, which is all but Trash and Junk.
var cacheFolders = []string{"Archive", "Drafts", "INBOX", "Sent"}

// seedMail is one message of the cache test's mailbox.
type seedMail struct {
	id         string // the test's name for it
	folder     string
	days       int // arrived this many days ago
	from, to   string
	cc         string
	subject    string
	body       string
	replyTo    string // id of the message this one answers
	attachment string // file name of an attachment, when set
	flags      []string
}

// cacheSeed is the mailbox. Each query in cacheDoveQueries matches some of
// it. The window is 30 days, so bodies of messages older than that are
// not prefetched. Message-IDs are cm00@example.org and up, holding no word
// a text query looks for, since Dovecot's TEXT search reads every header.
var cacheSeed = []seedMail{
	{
		id: "invoice", folder: "INBOX", days: 2, from: "Ann <ann@example.org>", to: "Bob <bob@example.test>",
		subject: "Invoice 42", body: "Please pay invoice 42 by Friday.",
	},
	{
		id: "report", folder: "INBOX", days: 3, from: "Dave <dave@example.net>", to: "Bob <bob@example.test>", cc: "Carol <carol@example.org>",
		subject: "Weekly report", body: "The weekly report is attached.", attachment: "report.pdf", flags: []string{`\Seen`},
	},
	{
		id: "lunch", folder: "INBOX", days: 5, from: "Ann <ann@example.org>", to: "Bob <bob@example.test>",
		subject: "Lunch", body: "Lunch on Thursday?", flags: []string{`\Seen`, `\Flagged`},
	},
	{
		id: "kickoff", folder: "INBOX", days: 10, from: "Erin <erin@example.com>", to: "Bob <bob@example.test>",
		subject: "Project X kickoff", body: "Kickoff notes for project X.", flags: []string{"projectx"},
	},
	{
		id: "oldinvoice", folder: "INBOX", days: 40, from: "Ann <ann@example.org>", to: "Bob <bob@example.test>",
		subject: "Invoice 7", body: "Please pay invoice 7.", flags: []string{`\Seen`},
	},
	{
		id: "annual", folder: "INBOX", days: 120, from: "Dave <dave@example.net>", to: "Bob <bob@example.test>",
		subject: "Annual report", body: strings.Repeat("Quarterly numbers follow.\r\n", 320), flags: []string{`\Seen`},
	},
	{
		id: "paid", folder: "Sent", days: 1, from: "Bob <bob@example.test>", to: "Ann <ann@example.org>",
		subject: "Re: Invoice 42", body: "Paid, thanks.", replyTo: "invoice", flags: []string{`\Seen`},
	},
	{
		id: "lunchreply", folder: "Sent", days: 2, from: "Bob <bob@example.test>", to: "Ann <ann@example.org>",
		subject: "Re: Lunch", body: "Thursday works.", flags: []string{`\Seen`},
	},
	{
		id: "question", folder: "Sent", days: 100, from: "Bob <bob@example.test>", to: "Dave <dave@example.net>",
		subject: "Report question", body: "Which quarter is this?", flags: []string{`\Seen`},
	},
	{
		id: "budget", folder: "Archive", days: 20, from: "Carol <carol@example.org>", to: "Bob <bob@example.test>",
		subject: "Budget", body: "The budget sheet is attached.", attachment: "budget.xlsx", flags: []string{`\Seen`},
	},
	{
		id: "ancient", folder: "Archive", days: 200, from: "Ann <ann@example.org>", to: "Bob <bob@example.test>",
		subject: "Ancient history", body: "From long ago.", flags: []string{`\Seen`, `\Flagged`},
	},
	{
		id: "scam", folder: "Trash", days: 4, from: "Dave <dave@example.net>", to: "Bob <bob@example.test>",
		subject: "Urgent invoice", body: "An invoice scam.", flags: []string{`\Seen`},
	},
}

// cacheDoveQueries have the same meaning on the cache and on Dovecot
// (spec section 6.5), so both must return the same refs in the same order.
var cacheDoveQueries = []string{
	"",
	"from:ann",
	"from:example.net",
	"to:bob",
	"cc:carol",
	"subject:report",
	"is:unread",
	"is:starred",
	"in:inbox",
	"in:sent",
	"in:archive",
	"larger:5K",
	"smaller:5K",
	"newer_than:7d",
	"older_than:90d",
	"from:ann is:unread",
	"from:ann OR from:dave",
	"in:inbox -is:read",
	"invoice newer_than:7d",
	`"pay invoice" newer_than:7d`,
}

// raw is the message as Dovecot stores it: Message-ID cm<i>@example.org,
// arrival and Date header at date, and a multipart body when it has an
// attachment.
func (m seedMail) raw(i int, index map[string]int, date time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", m.from, m.to)
	if m.cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", m.cc)
	}
	fmt.Fprintf(&b, "Subject: %s\r\nMessage-ID: <cm%02d@example.org>\r\nDate: %s\r\n", m.subject, i, date.Format(time.RFC1123Z))
	if m.replyTo != "" {
		parent := index[m.replyTo]
		fmt.Fprintf(&b, "In-Reply-To: <cm%02d@example.org>\r\nReferences: <cm%02d@example.org>\r\n", parent, parent)
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	if m.attachment == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n" + m.body + "\r\n")
		return b.String()
	}
	b.WriteString("Content-Type: multipart/mixed; boundary=\"cm-seed\"\r\n\r\n")
	b.WriteString("--cm-seed\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + m.body + "\r\n")
	fmt.Fprintf(&b, "--cm-seed\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=%q\r\n"+
		"Content-Transfer-Encoding: base64\r\n\r\n%s\r\n", m.attachment, base64.StdEncoding.EncodeToString([]byte("Not really a "+m.attachment+".")))
	b.WriteString("--cm-seed--\r\n")
	return b.String()
}

// seedCache appends cacheSeed to user's mailbox, dated back from now, and
// returns each message's ref by its id.
func seedCache(t *testing.T, dove *dovecot.Server, user string, now time.Time) map[string]string {
	t.Helper()
	index := make(map[string]int, len(cacheSeed))
	for i, m := range cacheSeed {
		index[m.id] = i
	}
	refs := make(map[string]string, len(cacheSeed))
	for i, m := range cacheSeed {
		date := now.Add(-time.Duration(m.days) * 24 * time.Hour)
		uid, err := dove.AppendFlags(user, m.folder, m.raw(i, index, date), date, m.flags)
		if err != nil {
			t.Fatalf("seeding %s: %v", m.id, err)
		}
		refs[m.id] = doveRef(t, dove, user, m.folder, uint32(uid))
	}
	return refs
}

// doveRef is the ref of uid in folder of the dove account.
func doveRef(t *testing.T, dove *dovecot.Server, user, folder string, uid uint32) string {
	t.Helper()
	v, err := dove.UIDValidity(user, folder)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Ref{Account: "dove", Folder: folder, UIDValidity: v, UID: uid}.String()
}

// doveCountingStore is the IMAP store with a count of the reads that reach the
// mail server for a caller, as opposed to the cache's background work:
// Search, FetchSummaries, FetchMessages, Thread and FetchHeaders on a
// context without mailstore.WithBackground. A get answered from the cache
// leaves the count as it was. Every other method, and every optional
// interface the daemon and the cache look for (Watcher, HeaderFetcher,
// Scoper and the connection probes), comes from the embedded store.
type doveCountingStore struct {
	*imapstore.Store
	reads *atomic.Int64
}

func (s doveCountingStore) count(ctx context.Context) {
	if !mailstore.IsBackground(ctx) {
		s.reads.Add(1)
	}
}

func (s doveCountingStore) Search(ctx context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	s.count(ctx)
	return s.Store.Search(ctx, req)
}

func (s doveCountingStore) FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	s.count(ctx)
	return s.Store.FetchSummaries(ctx, src, uids, opts)
}

func (s doveCountingStore) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	s.count(ctx)
	return s.Store.FetchMessages(ctx, refs)
}

func (s doveCountingStore) Thread(ctx context.Context, r ref.Ref) ([]ref.Ref, error) {
	s.count(ctx)
	return s.Store.Thread(ctx, r)
}

func (s doveCountingStore) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	s.count(ctx)
	return s.Store.FetchHeaders(ctx, src, uids)
}

// TestCacheAgainstDovecot runs the cache end to end on Dovecot (spec
// section 11.3). It seeds a mailbox across INBOX, Sent, Archive and Trash,
// waits for backfill and prefetch, and then checks in order: cache and
// server agree on a fixed query set, page for page, and a cursor carries
// across sources; queries the cache cannot answer go to the server, and
// has:, filename: and label: are answered by the cache only; a cached get
// reads nothing from the server and a missed one is kept; the cache follows
// a delivery, a flag and an expunge within seconds; a folder rebuild and an
// evict sweep work and are audited; a UIDVALIDITY change rebuilds the
// folder and an old ref is recovered by Message-ID; and deleting cache.db
// with the daemon stopped rebuilds it while a watch group loses and repeats
// no event.
func TestCacheAgainstDovecot(t *testing.T) {
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	refs := seedCache(t, dove, cacheUser, time.Now())
	var reads atomic.Int64
	cfg := fmt.Sprintf(cacheDoveConfig, cacheUser, dove.Endpoint.Port)
	opts := daemontest.Options{
		RootCAs:         dove.CA.Pool,
		Watch:           true,
		Cache:           true,
		WatchHeartbeat:  doveHeartbeat,
		WatchSweepFloor: doveSweepFloor,
		Secrets:         map[string]string{"accounts/dove": dovecot.Password},
		NewStore: func(acct config.ResolvedAccount) mailstore.Store {
			st := imapstore.New(acct, passwordCreds{user: cacheUser}, imapstore.Options{RootCAs: dove.CA.Pool})
			return doveCountingStore{Store: st, reads: &reads}
		},
	}
	h := daemontest.Start(t, cfg, opts)
	st := waitCacheReady(t, h)
	headers := map[string]uint64{}
	for _, f := range st.GetFolders() {
		headers[f.GetFolder()] = f.GetHeaders()
	}
	if want := map[string]uint64{"INBOX": 6, "Sent": 3, "Archive": 2, "Drafts": 0}; !maps.Equal(headers, want) {
		t.Fatalf("cached headers per folder = %v, want %v", headers, want)
	}

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"same answers", func(t *testing.T) { checkSameAnswers(t, h) }},
		{"cursor across sources", func(t *testing.T) { checkCursorAcrossSources(t, h) }},
		{"server only", func(t *testing.T) { checkServerOnly(t, h) }},
		{"cache only operators", func(t *testing.T) { checkCacheOnlyOperators(t, h, refs) }},
		{"reads", func(t *testing.T) { checkReads(t, h, &reads, refs) }},
		{"follows", func(t *testing.T) { checkFollows(t, h, dove) }},
		{"rebuild and evict", func(t *testing.T) { checkRebuildAndEvict(t, h) }},
		{"recovery", func(t *testing.T) { checkRecovery(t, h, dove, refs) }},
		{"rebuilt on restart", func(t *testing.T) { h = checkRebuiltOnRestart(t, h, cfg, opts, dove) }},
		{"audit", func(t *testing.T) { checkCacheAudit(t, h) }},
	}
	for _, s := range steps {
		// Each step leaves the state the next one starts from.
		if !t.Run(s.name, s.run) {
			return
		}
	}
}

// waitCacheReady polls GetCacheStatus until dove's cache is ready with
// every folder of cacheFolders backfilled and its window fetched.
func waitCacheReady(t *testing.T, h *daemontest.Harness) *chainmailv1.AccountCacheStatus {
	t.Helper()
	deadline := time.Now().Add(cacheReadyWithin)
	// Every call ends by the deadline, so a stalled one fails the wait in
	// time, with the last status seen.
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	var last *chainmailv1.GetCacheStatusResponse
	for {
		resp, err := h.Client.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: []string{"dove"}}))
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("the cache of dove is not ready within %v: GetCacheStatus: %v; last status %v", cacheReadyWithin, err, last)
			}
			t.Fatalf("GetCacheStatus: %v", err)
		}
		last = resp.Msg
		if st := readyStatus(resp.Msg, "dove"); st != nil {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache of dove is not ready within %v: %v", cacheReadyWithin, resp.Msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readyStatus returns account's status when it is ready and tracks exactly
// cacheFolders, each backfilled with its window done, and nil otherwise.
func readyStatus(resp *chainmailv1.GetCacheStatusResponse, account string) *chainmailv1.AccountCacheStatus {
	for _, a := range resp.GetAccounts() {
		if a.GetAccount() != account || a.GetState() != "ready" {
			continue
		}
		var names []string
		for _, f := range a.GetFolders() {
			if !f.GetBackfillDone() || !f.GetWindowDone() {
				return nil
			}
			names = append(names, f.GetFolder())
		}
		slices.Sort(names)
		if slices.Equal(names, cacheFolders) {
			return a
		}
	}
	return nil
}

// cachePage is one search run to its last page.
type cachePage struct {
	// rows has one line per row: the ref, then the thread count and the
	// snippet when the search asked for them.
	rows []string
	// source is SearchResponse.source of the first page.
	source string
	// count is dove's account count on the first page.
	count *chainmailv1.AccountCount
}

// searchPage runs one search page and fails the test on an error or a
// partial result.
func searchPage(t *testing.T, h *daemontest.Harness, req *chainmailv1.SearchRequest) *chainmailv1.SearchResponse {
	t.Helper()
	resp, err := searchPageCtx(t.Context(), h, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// searchPageCtx runs one search page under ctx and returns an error on a
// failed call or a partial result.
func searchPageCtx(ctx context.Context, h *daemontest.Harness, req *chainmailv1.SearchRequest) (*chainmailv1.SearchResponse, error) {
	resp, err := h.Client.Message.Search(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("search %q (source %q): %w", req.GetQueryString(), req.GetSource(), err)
	}
	if p := resp.Msg.GetPartial(); len(p) > 0 {
		return nil, fmt.Errorf("search %q (source %q): partial %v", req.GetQueryString(), req.GetSource(), p)
	}
	return resp.Msg, nil
}

// searchDove pages through q on dove with source, limit rows a page.
func searchDove(t *testing.T, h *daemontest.Harness, q, source string, limit uint32, snippets, threads bool) cachePage {
	t.Helper()
	out, err := searchDoveCtx(t.Context(), h, q, source, limit, snippets, threads)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// searchDoveCtx is searchDove under ctx, returning an error instead of
// failing the test.
func searchDoveCtx(ctx context.Context, h *daemontest.Harness, q, source string, limit uint32, snippets, threads bool) (cachePage, error) {
	var out cachePage
	cursor := ""
	for page := 0; ; page++ {
		if page == 50 {
			return cachePage{}, fmt.Errorf("search %q (source %q): still a next_cursor after 50 pages", q, source)
		}
		resp, err := searchPageCtx(ctx, h, &chainmailv1.SearchRequest{
			QueryString: q, Accounts: []string{"dove"}, Limit: limit, Cursor: cursor,
			Source: source, Snippets: snippets, Threads: threads,
		})
		if err != nil {
			return cachePage{}, err
		}
		if page == 0 {
			out.source = resp.GetSource()
			for _, ac := range resp.GetAccountCounts() {
				if ac.GetAccount() == "dove" {
					out.count = ac
				}
			}
		}
		for _, r := range resp.GetRows() {
			line := r.GetRef()
			if threads {
				line += fmt.Sprintf(" threads=%d", r.GetThreadCount())
			}
			if snippets {
				line += " snippet=" + r.GetSnippet().GetUntrusted()
			}
			out.rows = append(out.rows, line)
		}
		if cursor = resp.GetNextCursor(); cursor == "" {
			return out, nil
		}
	}
}

// countDove counts q's matches on dove with source.
func countDove(t *testing.T, h *daemontest.Harness, q, source string) uint64 {
	t.Helper()
	return searchPage(t, h, &chainmailv1.SearchRequest{
		QueryString: q, Accounts: []string{"dove"}, CountOnly: true, Source: source,
	}).GetCount()
}

// rowRefs lists a page's refs in order.
func rowRefs(resp *chainmailv1.SearchResponse) []string {
	refs := make([]string, 0, len(resp.GetRows()))
	for _, r := range resp.GetRows() {
		refs = append(refs, r.GetRef())
	}
	return refs
}

// checkAnswered fails the test unless p was answered completely by want,
// in SearchResponse.source and in dove's account count, with a synced_at
// in the past when want is the cache.
func checkAnswered(t *testing.T, q, source string, p cachePage, want string) {
	t.Helper()
	ac := p.count
	switch {
	case ac == nil:
		t.Errorf("%q with source %q: no account count for dove", q, source)
	case p.source != want || ac.GetSource() != want:
		t.Errorf("%q with source %q: answered by %q (account count %q), want %q", q, source, p.source, ac.GetSource(), want)
	case !ac.GetComplete() || ac.GetIncompleteReason() != "":
		t.Errorf("%q with source %q: complete %v, reason %q, want complete", q, source, ac.GetComplete(), ac.GetIncompleteReason())
	case want == "cache" && (ac.GetSyncedAt() == nil || ac.GetSyncedAt().AsTime().After(time.Now())):
		t.Errorf("%q with source %q: synced_at %v, want a time in the past", q, source, ac.GetSyncedAt())
	}
}

// checkSameAnswers runs every query of cacheDoveQueries from the server,
// from the cache by force and with no source, page for page, and requires
// the same rows and the same count. Snippets and thread collapsing agree
// too.
func checkSameAnswers(t *testing.T, h *daemontest.Harness) {
	for _, q := range cacheDoveQueries {
		server := searchDove(t, h, q, "server", cachePageRows, false, false)
		if len(server.rows) == 0 {
			t.Errorf("%q: the server finds nothing; every query must match part of cacheSeed", q)
			continue
		}
		checkAnswered(t, q, "server", server, "server")
		for _, source := range []string{"cache", ""} {
			got := searchDove(t, h, q, source, cachePageRows, false, false)
			if !slices.Equal(got.rows, server.rows) {
				t.Errorf("%q with source %q: rows %v, the server's %v", q, source, got.rows, server.rows)
			}
			checkAnswered(t, q, source, got, "cache")
		}
		if c, s := countDove(t, h, q, "cache"), countDove(t, h, q, "server"); c != s {
			t.Errorf("%q: count_only from the cache %d, from the server %d", q, c, s)
		}
	}
	for _, tc := range []struct {
		q                 string
		snippets, threads bool
	}{
		{q: "newer_than:7d", snippets: true},
		{q: "", threads: true},
	} {
		server := searchDove(t, h, tc.q, "server", 50, tc.snippets, tc.threads)
		cache := searchDove(t, h, tc.q, "cache", 50, tc.snippets, tc.threads)
		if !slices.Equal(cache.rows, server.rows) {
			t.Errorf("%q (snippets %v, threads %v): the cache's rows\n%v\nthe server's\n%v", tc.q, tc.snippets, tc.threads, cache.rows, server.rows)
		}
	}
}

// checkCursorAcrossSources pages a search with the first page from one
// source and the rest from the other, both ways, and requires the rows of
// an all-server run, none missed and none repeated (spec section 6.2).
func checkCursorAcrossSources(t *testing.T, h *daemontest.Harness) {
	want := searchDove(t, h, "", "server", 3, false, false).rows
	for _, tc := range []struct{ first, rest string }{{"cache", "server"}, {"server", "cache"}} {
		resp := searchPage(t, h, &chainmailv1.SearchRequest{Accounts: []string{"dove"}, Limit: 3, Source: tc.first})
		got := rowRefs(resp)
		for page := 1; resp.GetNextCursor() != ""; page++ {
			if page == 50 {
				t.Fatalf("first page from %s: still a next_cursor after 50 pages", tc.first)
			}
			resp = searchPage(t, h, &chainmailv1.SearchRequest{Accounts: []string{"dove"}, Limit: 3, Cursor: resp.GetNextCursor(), Source: tc.rest})
			got = append(got, rowRefs(resp)...)
		}
		if !slices.Equal(got, want) {
			t.Errorf("first page from %s, the rest from %s: %v, want %v", tc.first, tc.rest, got, want)
		}
	}
}

// checkServerOnly checks queries the cache cannot answer completely: with
// no source they go to the server, and forced to the cache they come back
// incomplete with the reason. A folder the cache never synced gives no
// rows.
func checkServerOnly(t *testing.T, h *daemontest.Harness) {
	for _, tc := range []struct{ q, reason string }{
		{q: "invoice", reason: "text_outside_window"},
		{q: "invoice newer_than:60d", reason: "text_outside_window"},
		{q: "in:trash", reason: "folder_not_synced"},
	} {
		server := searchDove(t, h, tc.q, "server", cachePageRows, false, false)
		auto := searchDove(t, h, tc.q, "", cachePageRows, false, false)
		if len(server.rows) == 0 || !slices.Equal(auto.rows, server.rows) {
			t.Errorf("%q with no source: rows %v, the server's %v, want the same and not empty", tc.q, auto.rows, server.rows)
		}
		checkAnswered(t, tc.q, "", auto, "server")
		forced := searchDove(t, h, tc.q, "cache", cachePageRows, false, false)
		if ac := forced.count; ac.GetSource() != "cache" || ac.GetComplete() || ac.GetIncompleteReason() != tc.reason {
			t.Errorf("%q forced to the cache: source %q, complete %v, reason %q, want cache, incomplete, %s",
				tc.q, ac.GetSource(), ac.GetComplete(), ac.GetIncompleteReason(), tc.reason)
		}
		if tc.reason == "folder_not_synced" && len(forced.rows) != 0 {
			t.Errorf("%q forced to the cache: rows %v from a folder it never synced", tc.q, forced.rows)
		}
	}
}

// checkCacheOnlyOperators checks has:, filename: and label: on a
// non-Google account (spec section 6.6): the cache answers them once the
// account is fresh, and the server path still refuses them, with a hint
// that points at chainmail cache status.
func checkCacheOnlyOperators(t *testing.T, h *daemontest.Harness, refs map[string]string) {
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{q: "has:attachment", want: []string{"report", "budget"}},
		{q: "filename:report.pdf", want: []string{"report"}},
		{q: "label:projectx", want: []string{"kickoff"}},
		{q: "has:attachment in:archive", want: []string{"budget"}},
	} {
		auto := searchDove(t, h, tc.q, "", cachePageRows, false, false)
		checkAnswered(t, tc.q, "", auto, "cache")
		want := make([]string, len(tc.want))
		for i, id := range tc.want {
			want[i] = refs[id]
		}
		slices.Sort(want)
		if got := slices.Sorted(slices.Values(auto.rows)); !slices.Equal(got, want) {
			t.Errorf("%q: rows %v, want %v", tc.q, got, want)
		}
		_, err := h.Client.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{
			QueryString: tc.q, Accounts: []string{"dove"}, Source: "server",
		}))
		d := api.Detail(err)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || d.GetReason() != api.ReasonInvalidArgument || !strings.Contains(d.GetHint(), "chainmail cache status") {
			t.Errorf("%q from the server: %v (hint %q), want INVALID_ARGUMENT with a hint naming chainmail cache status", tc.q, err, d.GetHint())
		}
	}
}

// getResult is one message read with get, and the response's source.
type getResult struct {
	source string
	msg    *chainmailv1.Message
}

// getDove reads one ref and fails the test unless it comes back as a message.
func getDove(t *testing.T, h *daemontest.Harness, r string) getResult {
	t.Helper()
	resp, err := h.Client.Message.Get(t.Context(), connect.NewRequest(&chainmailv1.GetRequest{Refs: []string{r}}))
	if err != nil {
		t.Fatalf("Get %s: %v", r, err)
	}
	items := resp.Msg.GetItems()
	if len(items) != 1 || items[0].GetMessage() == nil {
		t.Fatalf("Get %s: items %v, want one message", r, items)
	}
	return getResult{source: resp.Msg.GetSource(), msg: items[0].GetMessage()}
}

// withoutSource is m with source cleared, to compare a cache read with a
// server read.
func withoutSource(m *chainmailv1.Message) *chainmailv1.Message {
	c := proto.CloneOf(m)
	c.Source = ""
	return c
}

// checkReads checks get and thread through the cache (spec section 7.2,
// done when item 2): a prefetched body in a watched folder is read with no
// call to the mail server; a body outside the window is read from the
// server once, kept, and then answered from the cache with the same
// message; and a thread across INBOX and Sent comes from the cache.
func checkReads(t *testing.T, h *daemontest.Harness, reads *atomic.Int64, refs map[string]string) {
	before := reads.Load()
	hit := getDove(t, h, refs["invoice"])
	if n := reads.Load() - before; n != 0 {
		t.Errorf("a cache hit made %d reads of the mail server, want 0", n)
	}
	if hit.source != "cache" || hit.msg.GetSource() != "cache" {
		t.Errorf("get of a prefetched message: response source %q, message source %q, want cache", hit.source, hit.msg.GetSource())
	}
	if !strings.Contains(hit.msg.GetBody().GetUntrusted(), "Please pay invoice 42 by Friday.") {
		t.Errorf("cached body = %q", hit.msg.GetBody().GetUntrusted())
	}

	before = reads.Load()
	miss := getDove(t, h, refs["annual"])
	if reads.Load() == before {
		t.Error("get of a message outside the window made no read of the mail server")
	}
	if miss.msg.GetSource() != "server" {
		t.Errorf("first get outside the window: message source %q, want server", miss.msg.GetSource())
	}
	before = reads.Load()
	again := getDove(t, h, refs["annual"])
	if n := reads.Load() - before; n != 0 {
		t.Errorf("the second get of a message read once made %d reads of the mail server, want 0", n)
	}
	if again.msg.GetSource() != "cache" {
		t.Errorf("second get: message source %q, want cache", again.msg.GetSource())
	}
	if !proto.Equal(withoutSource(again.msg), withoutSource(miss.msg)) {
		t.Errorf("the cache's message differs from the server's:\ncache  %v\nserver %v", again.msg, miss.msg)
	}

	thread, err := h.Client.Message.GetThread(t.Context(), connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: refs["paid"]}))
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	var got []string
	for _, m := range thread.Msg.GetMessages() {
		got = append(got, m.GetRef())
	}
	if want := []string{refs["invoice"], refs["paid"]}; thread.Msg.GetSource() != "cache" || !slices.Equal(got, want) {
		t.Errorf("thread = %v from %q, want %v from cache", got, thread.Msg.GetSource(), want)
	}
}

// waitCacheRows polls a forced cache search of q until its refs are want.
func waitCacheRows(t *testing.T, h *daemontest.Harness, q string, want []string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	// Every search ends by the deadline, so a stalled one fails the wait
	// in time, with the last answer seen.
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	var got []string
	for {
		p, err := searchDoveCtx(ctx, h, q, "cache", 50, false, false)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("the cache answers %q with %v, then %v, after %v, want %v", q, got, err, within, want)
			}
			t.Fatal(err)
		}
		got = p.rows
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache answers %q with %v after %v, want %v", q, got, within, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkFollows has another IMAP client deliver, flag and expunge a message
// and requires the cache to follow each within seconds.
func checkFollows(t *testing.T, h *daemontest.Harness, dove *dovecot.Server) {
	uid, err := dove.Append(cacheUser, "INBOX", doveWatchMail("Follow me", "follow1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := doveRef(t, dove, cacheUser, "INBOX", uint32(uid))
	waitCacheRows(t, h, "subject:follow", []string{r}, cacheFollowWithin)
	if err := dove.SetFlag(cacheUser, "INBOX", uid, `\Flagged`, true); err != nil {
		t.Fatal(err)
	}
	waitCacheRows(t, h, "subject:follow is:starred", []string{r}, cacheFollowWithin)
	if err := dove.Expunge(cacheUser, "INBOX", uid); err != nil {
		t.Fatal(err)
	}
	waitCacheRows(t, h, "subject:follow", nil, cacheExpungeWithin)
}

// checkRebuildAndEvict rebuilds one folder and runs an evict sweep through
// CacheService. Every body is in the window or was read just now, so the
// sweep removes none.
func checkRebuildAndEvict(t *testing.T, h *daemontest.Harness) {
	rb, err := h.Client.Cache.RebuildCache(t.Context(), connect.NewRequest(&chainmailv1.RebuildCacheRequest{Account: "dove", Folder: "Archive"}))
	if err != nil {
		t.Fatalf("RebuildCache: %v", err)
	}
	if got := rb.Msg.GetFolders(); !slices.Equal(got, []string{"Archive"}) {
		t.Errorf("RebuildCache folders = %v, want [Archive]", got)
	}
	waitCacheReady(t, h)
	server := searchDove(t, h, "in:archive", "server", cachePageRows, false, false)
	cache := searchDove(t, h, "in:archive", "cache", cachePageRows, false, false)
	if !slices.Equal(cache.rows, server.rows) {
		t.Errorf("in:archive after the rebuild: the cache's %v, the server's %v", cache.rows, server.rows)
	}
	ev, err := h.Client.Cache.EvictCache(t.Context(), connect.NewRequest(&chainmailv1.EvictCacheRequest{}))
	if err != nil {
		t.Fatalf("EvictCache: %v", err)
	}
	if n := ev.Msg.GetBodiesEvicted(); n != 0 {
		t.Errorf("the sweep evicted %d bodies, want 0", n)
	}
}

// checkRecovery renumbers INBOX on the server, as a server-side reset
// would, and reads a ref from before (spec section 7.3). Until the cache
// has the folder again under the new UIDVALIDITY the read is STALE_REF;
// then it continues on the message's new ref, found by its Message-ID.
// Dovecot keeps the UIDs, so only the UIDVALIDITY of the ref changes.
func checkRecovery(t *testing.T, h *daemontest.Harness, dove *dovecot.Server, refs map[string]string) {
	old := parseRef(t, refs["lunch"])
	if err := dove.SetUIDValidity(cacheUser, "INBOX", old.UIDValidity+1000); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(cacheRecoverWithin)
	// Every Get ends by the deadline, so a stalled one fails the wait in
	// time, with what the last answer was still waiting for.
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	waiting := "no Get of the old ref answered"
	for {
		resp, err := h.Client.Message.Get(ctx, connect.NewRequest(&chainmailv1.GetRequest{Refs: []string{refs["lunch"]}}))
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("after %v: %s; then Get of the old ref: %v", cacheRecoverWithin, waiting, err)
			}
			t.Fatalf("Get of the old ref: %v", err)
		}
		items := resp.Msg.GetItems()
		if len(items) != 1 {
			t.Fatalf("Get of the old ref: %d items, want 1", len(items))
		}
		if m := items[0].GetMessage(); m != nil {
			got := parseRef(t, m.GetRef())
			if got != old {
				if got.Folder != "INBOX" || got.UID != old.UID || got.UIDValidity != old.UIDValidity+1000 || m.GetSubject().GetUntrusted() != "Lunch" {
					t.Fatalf("recovered %s (%q), want UID %d of INBOX under UIDVALIDITY %d", m.GetRef(), m.GetSubject().GetUntrusted(), old.UID, old.UIDValidity+1000)
				}
				break
			}
			// The cache still holds the folder under the old
			// UIDVALIDITY: the gap has not reached it yet.
			waiting = "the cache still answers the old ref " + refs["lunch"] + "; the UIDVALIDITY change never reached it"
		} else {
			reason := items[0].GetError().GetReason()
			if reason != api.ReasonStaleRef && reason != api.ReasonServerUnavailable {
				t.Fatalf("Get of the old ref: %s, want STALE_REF or SERVER_UNAVAILABLE until the message is found by its Message-ID", reason)
			}
			waiting = "the old ref was not recovered by Message-ID (last answer " + reason + ")"
		}
		// Every iteration, the old answer as much as STALE_REF, stops at
		// the deadline, so a cache that never sees the gap fails here.
		if time.Now().After(deadline) {
			t.Fatalf("after %v: %s", cacheRecoverWithin, waiting)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitCacheReady(t, h)
	server := searchDove(t, h, "in:inbox", "server", cachePageRows, false, false)
	cache := searchDove(t, h, "in:inbox", "cache", cachePageRows, false, false)
	if !slices.Equal(cache.rows, server.rows) {
		t.Errorf("in:inbox after the renumbering: the cache's %v, the server's %v", cache.rows, server.rows)
	}
}

// checkRebuiltOnRestart is done when item 3: with a watch group caught up,
// the daemon stops, cache.db and its WAL and SHM files are deleted, two
// messages arrive, and the daemon restarts on the same state. The group
// resumes with exactly those two new events, the cache is rebuilt and
// agrees with the server again, and the cache's own group stays hidden.
// It returns the restarted harness.
func checkRebuiltOnRestart(t *testing.T, h *daemontest.Harness, cfg string, opts daemontest.Options, dove *dovecot.Server) *daemontest.Harness {
	const group = "rebuild"
	w := subscribeDove(t, h, &chainmailv1.SubscribeRequest{Group: group, Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}}})
	ackDove(t, h, group, w.heartbeat(doveReadyWithin).GetPosition())
	w.close()

	db := filepath.Join(h.StateDir(), "cache.db")
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("cache.db is not in the state directory: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("stopping the daemon: %v", err)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	var want []string
	for i := range 2 {
		subject := fmt.Sprintf("Down %d", i+1)
		if _, err := dove.Append(cacheUser, "INBOX", doveWatchMail(subject, fmt.Sprintf("cachedown%d", i+1)), time.Now()); err != nil {
			t.Fatal(err)
		}
		want = append(want, subject)
	}
	restarted := opts
	restarted.Reuse = h
	h2 := daemontest.Start(t, cfg, restarted)

	w2 := subscribeDove(t, h2, &chainmailv1.SubscribeRequest{
		Group:  group,
		Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}, Kinds: []string{"new"}},
	})
	var got []string
	for len(got) < len(want) {
		got = append(got, w2.next("new", doveReadyWithin, nil).GetMessage().GetSubject().GetUntrusted())
	}
	if !slices.Equal(got, want) {
		t.Errorf("new events after the restart = %q, want %q", got, want)
	}
	w2.quiet("new", 4)
	w2.close()

	waitCacheReady(t, h2)
	for _, q := range []string{"", "subject:down", "in:inbox is:unread"} {
		server := searchDove(t, h2, q, "server", cachePageRows, false, false)
		cache := searchDove(t, h2, q, "cache", cachePageRows, false, false)
		if !slices.Equal(cache.rows, server.rows) {
			t.Errorf("%q after the rebuild: the cache's %v, the server's %v", q, cache.rows, server.rows)
		}
		if q == "subject:down" && len(cache.rows) != 2 {
			t.Errorf("the rebuilt cache has %d of the messages that arrived while the daemon was down, want 2", len(cache.rows))
		}
	}

	groups, err := h2.Client.Watch.ListGroups(t.Context(), connect.NewRequest(&chainmailv1.ListGroupsRequest{}))
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	var names []string
	for _, g := range groups.Msg.GetGroups() {
		names = append(names, g.GetName())
		if strings.HasPrefix(g.GetName(), "cache.") {
			t.Errorf("ListGroups shows the internal group %q", g.GetName())
		}
	}
	if !slices.Contains(names, group) {
		t.Errorf("ListGroups = %v, want %s among them", names, group)
	}
	return h2
}

// checkCacheAudit reads the audit log both daemons wrote: search and get
// rows name the source that answered, rebuild and evict are audited
// writes, status is not audited, and no row holds message content.
func checkCacheAudit(t *testing.T, h *daemontest.Harness) {
	saw := map[string]bool{}
	for _, row := range h.AuditRows(t) {
		text := fmt.Sprintf("%+v", row)
		for _, content := range []string{"Invoice", "Lunch", "Follow me", "Down 1"} {
			if strings.Contains(text, content) {
				t.Errorf("audit row %s holds message content", text)
			}
		}
		switch row.Action {
		case "search", "get":
			for _, source := range []string{"cache", "server"} {
				if strings.Contains(row.Targets, "source="+source) {
					saw[row.Action+" "+source] = true
				}
			}
		case "cache.rebuild", "cache.evict":
			saw[row.Action] = true
			if row.Action == "cache.rebuild" && row.Account != "dove" {
				t.Errorf("cache.rebuild row %s, want account dove", text)
			}
		default:
			if strings.HasPrefix(row.Action, "cache.") {
				t.Errorf("unexpected audit row %s: GetCacheStatus is not audited", text)
			}
		}
	}
	for _, want := range []string{"search cache", "search server", "get cache", "get server", "cache.rebuild", "cache.evict"} {
		if !saw[want] {
			t.Errorf("no audit row for %s (saw %v)", want, saw)
		}
	}
}
