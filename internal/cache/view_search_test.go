package cache_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// searchNow is the clock of the search view tests, a whole second so cached
// Unix-second dates compare equal to the fake's.
var searchNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func searchDaysAgo(n int) time.Time { return searchNow.AddDate(0, 0, -n) }

// searchClock is a watch.Clock stopped at searchNow.
type searchClock struct{}

func (searchClock) Now() time.Time { return searchNow }

func (searchClock) After(time.Duration) <-chan time.Time { return nil }

const searchConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
`

// searchServer wraps a fake store and records the UIDs of every
// FetchSummaries call, to show which rows the view asked the server for.
// While fail is set, FetchSummaries returns it after recording the call.
type searchServer struct {
	*fake.Store
	mu    sync.Mutex
	asked [][]uint32
	fail  error
}

func (s *searchServer) FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	s.mu.Lock()
	s.asked = append(s.asked, slices.Clone(uids))
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	return s.Store.FetchSummaries(ctx, src, uids, opts)
}

func (s *searchServer) summaryCalls() [][]uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

func (s *searchServer) failSummaries(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// searchFixture is one account's cache, built by hand as a finished
// backfill and prefetch leave it, and its view over a fake server.
type searchFixture struct {
	name   string
	server *searchServer
	store  *cache.Store
	view   *cache.View
}

func newSearchFixture(t *testing.T, gmail bool) *searchFixture {
	t.Helper()
	ctx := t.Context()
	name := "host"
	if gmail {
		name = "gmail"
	}
	cfg, err := config.Parse([]byte(searchConfig))
	if err != nil {
		t.Fatal(err)
	}
	acct, err := cfg.Account(name)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stateDB, err := db.OpenState(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() })
	cs, err := cache.Open(ctx, filepath.Join(dir, "cache.db"), cache.OpenOptions{Now: searchClock{}.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	fk := fake.New(name, gmail)
	fk.SetNow(searchClock{}.Now)
	server := &searchServer{Store: fk}
	a := cache.NewAccount(cache.AccountOptions{
		Account:  acct,
		Settings: func() config.CacheSettings { return cfg.CacheSettings(name) },
		MaxSize:  cfg.CacheMaxSize,
		Store:    cs,
		Log:      watch.NewLog(stateDB, searchClock{}.Now, nil),
		State:    watch.NewState(stateDB),
		Mail:     func(context.Context) (cache.MailStore, func(), error) { return fk, func() {}, nil },
		Clock:    searchClock{},
	})
	return &searchFixture{name: name, server: server, store: cs, view: a.View(server)}
}

// warm caches every folder the account's default search covers, with
// bodies, as a finished backfill and prefetch leave them.
func (f *searchFixture) warm(t *testing.T) {
	t.Helper()
	results, err := f.server.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		f.cacheFolder(t, r.Source, r.UIDs, true)
	}
}

// cacheFolder caches the headers of uids in src, and their bodies when
// bodies is set, then records the folder's backfill and window as done.
func (f *searchFixture) cacheFolder(t *testing.T, src mailstore.Source, uids []uint32, bodies bool) {
	t.Helper()
	ctx := t.Context()
	headers, err := f.server.FetchHeaders(ctx, src, uids)
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
		for _, res := range f.server.FetchMessages(ctx, refs) {
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			body := cache.Body{Raw: res.Message.Raw, TextClipped: res.Message.TextClipped}
			if _, err := f.store.PutBody(ctx, res.Message.Summary.Ref, body, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	next := uint32(1)
	if len(uids) > 0 {
		next = slices.Max(uids) + 1
	}
	if err := f.store.PutProgress(ctx, cache.Progress{
		Account: f.name, Folder: src.Folder, UIDValidity: src.UIDValidity, BaselineUIDNext: next,
		BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: searchNow,
	}); err != nil {
		t.Fatal(err)
	}
}

func searchAddr(name, email string) []mailstore.Address {
	return []mailstore.Address{{Name: name, Email: email}}
}

// seedSearchMail adds the messages the search view tests query, oldest
// first so UIDs rise with arrival, and returns their refs by subject. On
// Gmail they all live in All Mail, with labels; elsewhere in INBOX, Archive
// and Sent.
func seedSearchMail(s *fake.Store, gmail bool) map[string]ref.Ref {
	inbox, archive, sent := "INBOX", "Archive", "Sent"
	var inboxLabel, sentLabel, work []string
	if gmail {
		inbox, archive, sent = "[Gmail]/All Mail", "[Gmail]/All Mail", "[Gmail]/All Mail"
		inboxLabel, sentLabel, work = []string{`\Inbox`}, []string{`\Sent`}, []string{"work"}
	}
	acme := searchAddr("Acme Billing", "billing@acme.example")
	alex := searchAddr("Alex Doe", "alex@example.com")
	refs := map[string]ref.Ref{}
	add := func(m fake.Message) { refs[m.Subject] = s.Add(m) }
	add(fake.Message{Folder: archive, Date: searchDaysAgo(30), Subject: "old plan", Body: "notes about the plan", From: alex, Flags: []string{`\Seen`}})
	add(fake.Message{
		Folder: inbox, Date: searchDaysAgo(20), Subject: "invoice 41", Body: "the invoice is attached",
		From: acme, To: searchAddr("Bob", "bob@example.com"), Labels: slices.Concat(inboxLabel, work), Flags: []string{"todo"},
		Attachments: []fake.Attachment{{Part: "2", Filename: "invoice-41.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")}},
	})
	add(fake.Message{
		Folder: inbox, Date: searchDaysAgo(10), Subject: "lunch", Body: strings.Repeat("long text ", 300),
		Cc: searchAddr("Carol", "carol@example.com"), Labels: inboxLabel, Flags: []string{`\Seen`, `\Flagged`},
	})
	add(fake.Message{Folder: sent, Date: searchDaysAgo(5), Subject: "re: old plan", Body: "agreed on the plan", From: searchAddr("Me", "me@example.com"), Labels: sentLabel, Flags: []string{`\Seen`}})
	add(fake.Message{Folder: inbox, Date: searchDaysAgo(3), Subject: "invoice 42", Body: "please pay the invoice", From: acme, Labels: inboxLabel})
	return refs
}

// searchRequest parses q and splits off in:, as the daemon's plan does.
func searchRequest(t *testing.T, q string, countOnly bool) mailstore.SearchRequest {
	t.Helper()
	n, err := query.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	scope, rest := query.Split(n)
	return mailstore.SearchRequest{Query: rest, In: scope.In, CountOnly: countOnly}
}

// diffResults says how got differs from want, source by source, or returns
// "" when they match.
func diffResults(got, want []mailstore.SourceResult) string {
	if len(got) != len(want) {
		return fmt.Sprintf("%d sources %v, want %d %v", len(got), sourceFolders(got), len(want), sourceFolders(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		switch {
		case g.Source != w.Source:
			return fmt.Sprintf("source %d = %+v, want %+v", i, g.Source, w.Source)
		case (g.Err == nil) != (w.Err == nil):
			return fmt.Sprintf("%s: err %v, want %v", w.Source.Folder, g.Err, w.Err)
		case !slices.Equal(g.UIDs, w.UIDs):
			return fmt.Sprintf("%s: UIDs %v, want %v", w.Source.Folder, g.UIDs, w.UIDs)
		case g.Count != w.Count:
			return fmt.Sprintf("%s: count %d, want %d", w.Source.Folder, g.Count, w.Count)
		}
	}
	return ""
}

func sourceFolders(results []mailstore.SourceResult) []string {
	var out []string
	for _, r := range results {
		out = append(out, r.Source.Folder)
	}
	return out
}

// matchedRefs returns the refs of every UID in results, on an account
// without Gmail message IDs.
func matchedRefs(account string, results []mailstore.SourceResult) []ref.Ref {
	var out []ref.Ref
	for _, r := range results {
		for _, uid := range r.UIDs {
			out = append(out, ref.Ref{Account: account, Folder: r.Source.Folder, UIDValidity: r.Source.UIDValidity, UID: uid})
		}
	}
	return out
}

// TestViewSearchMatchesServer checks that the view finds what the fake
// server finds for header operators, dates, sizes, in: and whole-word text,
// folder by folder, with and without count_only.
func TestViewSearchMatchesServer(t *testing.T) {
	t.Parallel()
	common := []string{
		"", "from:acme", "-from:acme", "subject:invoice", "to:bob", "cc:carol",
		"is:unread", "is:read", "is:starred", "newer_than:7d", "older_than:7d",
		"after:2026-10-20", "before:2026-10-20", "larger:2K", "smaller:2K",
		"from:acme OR subject:plan", "invoice", `"the plan"`, "in:inbox",
	}
	for _, gmail := range []bool{false, true} {
		queries := slices.Clone(common)
		if gmail {
			queries = append(queries, "has:attachment", "filename:pdf", "label:work", "in:archive", "in:sent")
		} else {
			queries = append(queries, "in:Archive", "in:sent")
		}
		t.Run(fmt.Sprintf("gmail=%t", gmail), func(t *testing.T) {
			t.Parallel()
			f := newSearchFixture(t, gmail)
			seedSearchMail(f.server.Store, gmail)
			f.warm(t)
			for _, q := range queries {
				for _, countOnly := range []bool{false, true} {
					req := searchRequest(t, q, countOnly)
					want, err := f.server.Search(t.Context(), req)
					if err != nil {
						t.Fatalf("server %q: %v", q, err)
					}
					got, err := f.view.Search(t.Context(), req)
					if err != nil {
						t.Fatalf("view %q: %v", q, err)
					}
					if d := diffResults(got, want); d != "" {
						t.Errorf("%q count_only=%t: %s", q, countOnly, d)
					}
				}
			}
		})
	}
}

// TestViewSearchAnswersCacheOnlyOperators checks has:, filename: and label:
// on an account other than Google, which the server refuses: label: is an
// IMAP keyword, and a system flag is never a label, so it matches nothing.
func TestViewSearchAnswersCacheOnlyOperators(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false)
	refs := seedSearchMail(f.server.Store, false)
	f.warm(t)
	tests := []struct {
		query string
		want  []ref.Ref
	}{
		{query: "has:attachment", want: []ref.Ref{refs["invoice 41"]}},
		{query: "filename:pdf", want: []ref.Ref{refs["invoice 41"]}},
		{query: "filename:41", want: []ref.Ref{refs["invoice 41"]}},
		{query: "label:todo", want: []ref.Ref{refs["invoice 41"]}},
		{query: `label:\Seen`, want: nil},
	}
	for _, tt := range tests {
		got, err := f.view.Search(t.Context(), searchRequest(t, tt.query, false))
		if err != nil {
			t.Fatalf("%q: %v", tt.query, err)
		}
		if refs := matchedRefs("host", got); !slices.Equal(refs, tt.want) {
			t.Errorf("%q matched %v, want %v", tt.query, refs, tt.want)
		}
	}
}

// TestViewSearchBounds checks a search with Bounds, as the server's Search
// takes them: exactly those folders, each below its bound, a changed
// UIDVALIDITY failing only that folder, and a folder the cache does not hold
// failing as not found.
func TestViewSearchBounds(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false)
	refs := seedSearchMail(f.server.Store, false)
	f.warm(t)
	below := refs["invoice 42"].UID
	req := mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{
		"INBOX": {UIDValidity: 1, Below: below},
		"Sent":  {UIDValidity: 9},
	}}
	want, err := f.server.Search(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.view.Search(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffResults(got, want); d != "" {
		t.Errorf("bounded search: %s", d)
	}
	if len(got) != 2 || got[0].Source.Folder != "INBOX" || !errors.Is(got[1].Err, mailstore.ErrStaleRef) {
		t.Errorf("bounded results = %+v, want INBOX then a stale Sent", got)
	}
	if err := f.store.DropFolder(t.Context(), "host", "Archive", false); err != nil {
		t.Fatal(err)
	}
	got, err = f.view.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"Archive": {UIDValidity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !errors.Is(got[0].Err, mailstore.ErrNotFound) {
		t.Errorf("bounded search of a folder the cache lacks = %+v, want ErrNotFound", got)
	}
}

// TestViewSearchLeavesOutFoldersTheCacheDoesNotHold checks that a folder
// in the search's scope that the cache has no progress for contributes no
// result, which only a forced cache search ever sees.
func TestViewSearchLeavesOutFoldersTheCacheDoesNotHold(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false)
	seedSearchMail(f.server.Store, false)
	f.warm(t)
	if err := f.store.DropFolder(t.Context(), "host", "Archive", false); err != nil {
		t.Fatal(err)
	}
	got, err := f.view.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if folders := sourceFolders(got); slices.Contains(folders, "Archive") || !slices.Contains(folders, "INBOX") {
		t.Errorf("folders = %v, want every default folder but Archive", folders)
	}
}

// TestViewSearchServerOnlyMatchesNothing checks that a query the cache can
// never answer completely returns its held folders with no matches, never
// a partial list that could pass for the answer.
func TestViewSearchServerOnlyMatchesNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		gmail bool
		query string
	}{
		{gmail: true, query: "in:anywhere"},
		{gmail: false, query: "invoice OR lunch"},
	}
	for _, tt := range tests {
		f := newSearchFixture(t, tt.gmail)
		seedSearchMail(f.server.Store, tt.gmail)
		f.warm(t)
		got, err := f.view.Search(t.Context(), searchRequest(t, tt.query, false))
		if err != nil {
			t.Fatalf("%q: %v", tt.query, err)
		}
		if len(got) == 0 {
			t.Fatalf("%q: no sources, want the held folders", tt.query)
		}
		for _, r := range got {
			if len(r.UIDs) != 0 || r.Count != 0 || r.Err != nil {
				t.Errorf("%q: %s = %v (count %d, err %v), want no matches", tt.query, r.Source.Folder, r.UIDs, r.Count, r.Err)
			}
		}
	}
}

// diffSummary says how got differs from want, leaving out Preview and
// Sample, or returns "" when they match.
func diffSummary(got, want mailstore.Summary) string {
	switch {
	case got.Ref != want.Ref:
		return fmt.Sprintf("ref %v, want %v", got.Ref, want.Ref)
	case !got.Date.Equal(want.Date) || !got.InternalDate.Equal(want.InternalDate):
		return fmt.Sprintf("dates %v/%v, want %v/%v", got.Date, got.InternalDate, want.Date, want.InternalDate)
	case !slices.Equal(got.From, want.From) || !slices.Equal(got.To, want.To):
		return fmt.Sprintf("from/to %v/%v, want %v/%v", got.From, got.To, want.From, want.To)
	case got.Subject != want.Subject || got.Size != want.Size || got.HasAttachment != want.HasAttachment || got.ThreadID != want.ThreadID:
		return fmt.Sprintf("summary %+v, want %+v", got, want)
	case !slices.Equal(got.Flags, want.Flags) || !slices.Equal(got.Labels, want.Labels):
		return fmt.Sprintf("flags/labels %v/%v, want %v/%v", got.Flags, got.Labels, want.Flags, want.Labels)
	}
	return ""
}

// snippetText renders a summary's snippet as the daemon's snippetOf does.
func snippetText(s mailstore.Summary) string {
	switch {
	case s.Preview != nil:
		return render.Clip(*s.Preview)
	case s.Sample != nil:
		return render.Snippet(s.Sample.MediaType, s.Sample.Charset, s.Sample.Encoding, s.Sample.Data)
	default:
		return ""
	}
}

// TestViewFetchSummariesMatchesServer checks rows from the cache against
// the server's: the same fields, in the order of the UIDs asked for, a UID
// the folder lacks left out, and a changed UIDVALIDITY stale.
func TestViewFetchSummariesMatchesServer(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		f := newSearchFixture(t, gmail)
		seedSearchMail(f.server.Store, gmail)
		f.warm(t)
		results, err := f.server.Search(t.Context(), mailstore.SearchRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			uids := append(slices.Clone(r.UIDs), 999)
			want, err := f.server.FetchSummaries(t.Context(), r.Source, uids, mailstore.SummaryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.view.FetchSummaries(t.Context(), r.Source, uids, mailstore.SummaryOptions{})
			if err != nil {
				t.Fatalf("gmail=%t %s: %v", gmail, r.Source.Folder, err)
			}
			if len(got) != len(want) {
				t.Fatalf("gmail=%t %s: %d rows, want %d", gmail, r.Source.Folder, len(got), len(want))
			}
			for i := range want {
				if d := diffSummary(got[i], want[i]); d != "" {
					t.Errorf("gmail=%t %s row %d: %s", gmail, r.Source.Folder, i, d)
				}
			}
			stale := mailstore.Source{Folder: r.Source.Folder, UIDValidity: r.Source.UIDValidity + 1}
			if _, err := f.view.FetchSummaries(t.Context(), stale, r.UIDs, mailstore.SummaryOptions{}); len(r.UIDs) > 0 && !errors.Is(err, mailstore.ErrStaleRef) {
				t.Errorf("gmail=%t %s with a changed UIDVALIDITY: %v, want ErrStaleRef", gmail, r.Source.Folder, err)
			}
		}
	}
}

// TestViewFetchSummariesSnippets checks that snippets come from cached
// bodies and equal the server's, and that rows without a cached body get
// theirs from one server call for those rows only.
func TestViewFetchSummariesSnippets(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false)
	seedSearchMail(f.server.Store, false)
	f.warm(t)
	late := f.server.Add(fake.Message{Folder: "INBOX", Date: searchDaysAgo(0), Subject: "late", Body: "arrived after the prefetch"})
	f.cacheFolder(t, mailstore.Source{Folder: "INBOX", UIDValidity: late.UIDValidity}, []uint32{late.UID}, false)
	results, err := f.server.Search(t.Context(), mailstore.SearchRequest{In: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := results[0]
	want, err := f.server.FetchSummaries(t.Context(), inbox.Source, inbox.UIDs, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.server.summaryCalls())
	got, err := f.view.FetchSummaries(t.Context(), inbox.Source, inbox.UIDs, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if g, w := snippetText(got[i]), snippetText(want[i]); g != w {
			t.Errorf("row %d snippet = %q, want %q", i, g, w)
		}
	}
	calls := f.server.summaryCalls()[before:]
	if len(calls) != 1 || !slices.Equal(calls[0], []uint32{late.UID}) {
		t.Errorf("server FetchSummaries calls = %v, want one for UID %d only", calls, late.UID)
	}
}

// TestViewFetchSummariesRepeatedMissingUID checks that a UID asked for twice
// whose body the cache lacks gets the server's snippet in both rows, as the
// server's own FetchSummaries gives it.
func TestViewFetchSummariesRepeatedMissingUID(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false)
	seedSearchMail(f.server.Store, false)
	f.warm(t)
	late := f.server.Add(fake.Message{Folder: "INBOX", Date: searchDaysAgo(0), Subject: "late", Body: "arrived after the prefetch"})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: late.UIDValidity}
	f.cacheFolder(t, src, []uint32{late.UID}, false)
	uids := []uint32{late.UID, late.UID}
	want, err := f.server.FetchSummaries(t.Context(), src, uids, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.view.FetchSummaries(t.Context(), src, uids, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if g, w := snippetText(got[i]), snippetText(want[i]); g != w || g == "" {
			t.Errorf("row %d snippet = %q, want %q", i, g, w)
		}
	}
}

// TestViewFetchSummariesKeepsCachedRowsWhenTheServerFails checks that a
// failed server snippet fill does not discard the page: every row comes
// back, rows with a cached body keep their snippets, and the rows the
// server could not fill have none.
func TestViewFetchSummariesKeepsCachedRowsWhenTheServerFails(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false)
	seedSearchMail(f.server.Store, false)
	f.warm(t)
	late := f.server.Add(fake.Message{Folder: "INBOX", Date: searchDaysAgo(0), Subject: "late", Body: "arrived after the prefetch"})
	f.cacheFolder(t, mailstore.Source{Folder: "INBOX", UIDValidity: late.UIDValidity}, []uint32{late.UID}, false)
	results, err := f.server.Search(t.Context(), mailstore.SearchRequest{In: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := results[0]
	want, err := f.server.FetchSummaries(t.Context(), inbox.Source, inbox.UIDs, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		t.Fatal(err)
	}
	f.server.failSummaries(mailstore.ErrUnavailable)
	got, err := f.view.FetchSummaries(t.Context(), inbox.Source, inbox.UIDs, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		t.Fatalf("FetchSummaries: %v, want the cached rows", err)
	}
	if len(got) != len(want) || len(got) < 2 {
		t.Fatalf("%d rows, want %d (at least 2)", len(got), len(want))
	}
	for i := range want {
		if d := diffSummary(got[i], want[i]); d != "" {
			t.Errorf("row %d: %s", i, d)
		}
		if got[i].Ref.UID == late.UID {
			if got[i].Preview != nil || got[i].Sample != nil {
				t.Errorf("row %d (no cached body): preview %v, sample %v, want neither", i, got[i].Preview, got[i].Sample)
			}
			continue
		}
		if g, w := snippetText(got[i]), snippetText(want[i]); g != w || g == "" {
			t.Errorf("row %d snippet = %q, want %q", i, g, w)
		}
	}
}
