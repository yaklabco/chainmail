package cache_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver lastReadAt opens cache.db with

	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// readNow is the clock of the read view tests, a whole second so cached
// Unix-second dates compare equal to the fake's.
var readNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func readDaysAgo(n int) time.Time { return readNow.AddDate(0, 0, -n) }

// readClock is a watch.Clock stopped at readNow.
type readClock struct{}

func (readClock) Now() time.Time { return readNow }

func (readClock) After(time.Duration) <-chan time.Time { return nil }

const readConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
`

// readFixture is one account's cache, sync state and fake server, built
// by hand as a finished backfill and prefetch leave them, and its view.
type readFixture struct {
	name    string
	fk      *fake.Store
	stateDB *sql.DB
	path    string
	store   *cache.Store
	view    *cache.View
}

func newReadFixture(t *testing.T, gmail bool) *readFixture {
	t.Helper()
	ctx := t.Context()
	name := "host"
	if gmail {
		name = "gmail"
	}
	cfg, err := config.Parse([]byte(readConfig))
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
	path := filepath.Join(dir, "cache.db")
	cs, err := cache.Open(ctx, path, cache.OpenOptions{Now: readClock{}.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	fk := fake.New(name, gmail)
	fk.SetNow(readClock{}.Now)
	a := cache.NewAccount(cache.AccountOptions{
		Account:  acct,
		Settings: func() config.CacheSettings { return cfg.CacheSettings(name) },
		MaxSize:  cfg.CacheMaxSize,
		Store:    cs,
		Log:      watch.NewLog(stateDB, readClock{}.Now, nil),
		State:    watch.NewState(stateDB),
		Mail:     func(context.Context) (cache.MailStore, func(), error) { return fk, func() {}, nil },
		Clock:    readClock{},
	})
	// Freshness also needs the builder caught up with the event log, which
	// it never is here without running the account.
	if err := cache.ReadTestCatchUp(ctx, a); err != nil {
		t.Fatal(err)
	}
	return &readFixture{name: name, fk: fk, stateDB: stateDB, path: path, store: cs, view: a.View(fk)}
}

// warm caches every folder the account's default search covers, with
// bodies, and records each in sync_folders as synced now.
func (f *readFixture) warm(t *testing.T) {
	t.Helper()
	results, err := f.fk.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		f.cacheFolder(t, r.Source, r.UIDs, true)
	}
}

// cacheFolder caches the headers of uids in src, and their bodies (unread)
// when bodies is set, records the folder's backfill and window as done, and
// records it in sync_folders as synced now: watching for INBOX and Gmail's
// All Mail, polling for the rest.
func (f *readFixture) cacheFolder(t *testing.T, src mailstore.Source, uids []uint32, bodies bool) {
	t.Helper()
	ctx := t.Context()
	headers, err := f.fk.FetchHeaders(ctx, src, uids)
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
		for _, res := range f.fk.FetchMessages(ctx, refs) {
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
		Account: f.name, Folder: src.Folder, UIDValidity: src.UIDValidity, BaselineUIDNext: next,
		BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: readNow,
	}); err != nil {
		t.Fatal(err)
	}
	mode, state := watch.ModePoll, watch.StatePolling
	if src.Folder == "INBOX" || f.name == "gmail" {
		mode, state = watch.ModeIdle, watch.StateWatching
	}
	f.putSync(t, watch.FolderRow{
		Account: f.name, Folder: src.Folder, UIDValidity: src.UIDValidity, UIDNext: next,
		Mode: mode, State: state, LastSyncAt: readNow,
	})
}

func (f *readFixture) putSync(t *testing.T, row watch.FolderRow) {
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

// ageSync makes a polled folder's last successful pass an hour old, which
// is past twice the default five minute poll, so the folder is stale.
func (f *readFixture) ageSync(t *testing.T, folder string) {
	t.Helper()
	row, ok, err := watch.NewState(f.stateDB).Folder(t.Context(), f.name, folder)
	if err != nil || !ok {
		t.Fatalf("sync row %s: %v, %t", folder, err, ok)
	}
	row.LastSyncAt = readNow.Add(-time.Hour)
	f.putSync(t, row)
}

// resetFolder gives folder a new UIDVALIDITY on the server, which renumbers
// its messages, and rebuilds the cache's copy as the builder does for a
// uidvalidity gap: rows dropped with their Message-IDs kept for recovery,
// then the headers cached again under the new UIDVALIDITY, without bodies.
func (f *readFixture) resetFolder(t *testing.T, folder string) mailstore.Source {
	t.Helper()
	f.fk.NewUIDValidity(folder)
	if err := f.store.DropFolder(t.Context(), f.name, folder, true); err != nil {
		t.Fatal(err)
	}
	res, err := f.fk.Search(t.Context(), mailstore.SearchRequest{In: folder})
	if err != nil {
		t.Fatal(err)
	}
	src := res[0].Source
	if f.name == "gmail" {
		res, err = f.fk.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{folder: {UIDValidity: src.UIDValidity}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.cacheFolder(t, res[0].Source, res[0].UIDs, false)
	return res[0].Source
}

// lastReadAt reports whether the cached body of r has a last_read_at.
func (f *readFixture) lastReadAt(t *testing.T, r ref.Ref) bool {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var at sql.NullInt64
	err = conn.QueryRowContext(t.Context(),
		`SELECT b.last_read_at FROM bodies b JOIN messages m ON m.id = b.msg
		 WHERE m.account = ? AND m.folder = ? AND m.uidvalidity = ? AND m.uid = ?`,
		r.Account, r.Folder, int64(r.UIDValidity), int64(r.UID)).Scan(&at)
	if err != nil {
		t.Fatalf("last_read_at of %v: %v", r, err)
	}
	return at.Valid
}

func readAddr(name, email string) []mailstore.Address {
	return []mailstore.Address{{Name: name, Email: email}}
}

// seedReadMail adds the read tests' mail, oldest first, and returns refs by
// subject: a three-message thread rooted at p@x across INBOX and Sent, a
// memo with an attachment in Archive, and a note without a Message-ID. On
// Gmail they all live in All Mail.
func seedReadMail(s *fake.Store, gmail bool) map[string]ref.Ref {
	inbox, sent, archive := "INBOX", "Sent", "Archive"
	if gmail {
		inbox, sent, archive = "[Gmail]/All Mail", "[Gmail]/All Mail", "[Gmail]/All Mail"
	}
	refs := map[string]ref.Ref{}
	add := func(m fake.Message) { refs[m.Subject] = s.Add(m) }
	add(fake.Message{Folder: inbox, Date: readDaysAgo(30), Subject: "plan", MessageID: "p@x", Body: "plan?", From: readAddr("Alex", "alex@example.com")})
	add(fake.Message{
		Folder: sent, Date: readDaysAgo(20), Subject: "re: plan", MessageID: "p1@x", References: []string{"p@x"},
		Body: "Thanks.\n\nOn Mon, 26 Oct 2026 at 09:00, Alex <alex@example.com> wrote:\n> plan?", Flags: []string{`\Seen`},
	})
	add(fake.Message{Folder: inbox, Date: readDaysAgo(10), Subject: "re: re: plan", MessageID: "p2@x", InReplyTo: []string{"p1@x"}, References: []string{"p@x", "p1@x"}, Body: "ok"})
	add(fake.Message{
		Folder: archive, Date: readDaysAgo(5), Subject: "memo", MessageID: "memo@x", Body: strings.Repeat("memo text ", 200),
		Cc:          readAddr("Carol", "carol@example.com"),
		Attachments: []fake.Attachment{{Part: "2", Filename: "memo.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")}},
	})
	add(fake.Message{Folder: inbox, Date: readDaysAgo(2), Subject: "note", Body: "a note"})
	return refs
}

// diffMessage says how got differs from want, or returns "" when they match.
func diffMessage(got, want *mailstore.Message) string {
	if got == nil || want == nil {
		return fmt.Sprintf("message %v, want %v", got, want)
	}
	g, w := got.Summary, want.Summary
	switch {
	case g.Ref != w.Ref:
		return fmt.Sprintf("ref %v, want %v", g.Ref, w.Ref)
	case !g.Date.Equal(w.Date) || !g.InternalDate.Equal(w.InternalDate):
		return fmt.Sprintf("dates %v/%v, want %v/%v", g.Date, g.InternalDate, w.Date, w.InternalDate)
	case !slices.Equal(g.From, w.From) || !slices.Equal(g.To, w.To) || !slices.Equal(got.Cc, want.Cc):
		return "addresses differ"
	case g.Subject != w.Subject || g.ThreadID != w.ThreadID || g.Size != w.Size || g.HasAttachment != w.HasAttachment:
		return fmt.Sprintf("summary %+v, want %+v", g, w)
	case !slices.Equal(g.Flags, w.Flags) || !slices.Equal(g.Labels, w.Labels):
		return fmt.Sprintf("flags/labels %v/%v, want %v/%v", g.Flags, g.Labels, w.Flags, w.Labels)
	case got.MessageID != want.MessageID || !slices.Equal(got.InReplyTo, want.InReplyTo) || !slices.Equal(got.References, want.References):
		return fmt.Sprintf("ids %q %v %v, want %q %v %v", got.MessageID, got.InReplyTo, got.References, want.MessageID, want.InReplyTo, want.References)
	case string(got.Raw) != string(want.Raw) || got.TextClipped != want.TextClipped:
		return "raw differs"
	case !slices.Equal(got.Attachments, want.Attachments):
		return fmt.Sprintf("attachments %v, want %v", got.Attachments, want.Attachments)
	}
	return ""
}

func readList(refs map[string]ref.Ref, subjects ...string) []ref.Ref {
	out := make([]ref.Ref, len(subjects))
	for i, s := range subjects {
		out[i] = refs[s]
	}
	return out
}

// TestViewFetchMessagesHitsMakeNoServerCall checks that cached messages in
// fresh folders come from the cache, equal to what the server returns,
// without a server call, and are marked read.
func TestViewFetchMessagesHitsMakeNoServerCall(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		f := newReadFixture(t, gmail)
		refs := seedReadMail(f.fk, gmail)
		f.warm(t)
		list := readList(refs, "plan", "re: plan", "memo", "note", "plan")
		want := f.fk.FetchMessages(t.Context(), list)
		fetches, summaries := f.fk.Calls("FetchMessages"), f.fk.Calls("FetchSummaries")
		got, sources := f.view.FetchMessagesFrom(t.Context(), list)
		if n, m := f.fk.Calls("FetchMessages")-fetches, f.fk.Calls("FetchSummaries")-summaries; n != 0 || m != 0 {
			t.Errorf("gmail=%t: %d FetchMessages and %d FetchSummaries calls, want none", gmail, n, m)
		}
		for i := range list {
			if got[i].Err != nil || sources[i] != cache.SourceCache {
				t.Errorf("gmail=%t item %d: err %v, source %q, want a cache hit", gmail, i, got[i].Err, sources[i])
				continue
			}
			if d := diffMessage(got[i].Message, want[i].Message); d != "" {
				t.Errorf("gmail=%t item %d: %s", gmail, i, d)
			}
			if !f.lastReadAt(t, list[i]) {
				t.Errorf("gmail=%t item %d: a hit did not set last_read_at", gmail, i)
			}
		}
	}
}

// TestViewFetchMessagesMissIsKept checks that a message the cache lacks is
// read from the server, kept with its row and body marked read, counted
// against the day's budget, and a cache hit the next time.
func TestViewFetchMessagesMissIsKept(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t, false)
	seedReadMail(f.fk, false)
	f.warm(t)
	late := f.fk.Add(fake.Message{Folder: "INBOX", Date: readDaysAgo(0), Subject: "late", Body: "arrived after the prefetch"})
	fetches := f.fk.Calls("FetchMessages")
	got, sources := f.view.FetchMessagesFrom(t.Context(), []ref.Ref{late})
	if got[0].Err != nil || sources[0] != cache.SourceServer || f.fk.Calls("FetchMessages")-fetches != 1 {
		t.Fatalf("miss: err %v, source %q, %d fetches", got[0].Err, sources[0], f.fk.Calls("FetchMessages")-fetches)
	}
	if _, ok, err := f.store.Row(t.Context(), late); err != nil || !ok {
		t.Errorf("row after a miss: %v, %t", err, ok)
	}
	if !f.lastReadAt(t, late) {
		t.Error("a miss did not keep its body marked read")
	}
	spent, err := f.store.Budget(t.Context(), "host", readNow.UTC().Format("2006-01-02"))
	if err != nil || spent != int64(len(got[0].Message.Raw)) {
		t.Errorf("budget = %d, %v, want %d", spent, err, len(got[0].Message.Raw))
	}
	fetches = f.fk.Calls("FetchMessages")
	again, sources := f.view.FetchMessagesFrom(t.Context(), []ref.Ref{late})
	if again[0].Err != nil || sources[0] != cache.SourceCache || f.fk.Calls("FetchMessages") != fetches {
		t.Errorf("second read: err %v, source %q, %d fetches, want a cache hit", again[0].Err, sources[0], f.fk.Calls("FetchMessages")-fetches)
	}
}

// TestViewFetchMessagesKeepsOnlyTrackedFolders checks that a message read
// from a folder the cache does not track, here Trash, is returned but not
// stored.
func TestViewFetchMessagesKeepsOnlyTrackedFolders(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t, false)
	seedReadMail(f.fk, false)
	f.warm(t)
	binned := f.fk.Add(fake.Message{Folder: "Trash", Date: readDaysAgo(1), Subject: "binned", Body: "gone"})
	got, sources := f.view.FetchMessagesFrom(t.Context(), []ref.Ref{binned})
	if got[0].Err != nil || sources[0] != cache.SourceServer {
		t.Fatalf("trash read: err %v, source %q", got[0].Err, sources[0])
	}
	if _, ok, err := f.store.Row(t.Context(), binned); err != nil || ok {
		t.Errorf("a Trash message was stored: %v, %t", err, ok)
	}
}

// TestViewFetchMessagesFlags checks spec 7.2: flags come from the cache in
// a fresh folder, from one FetchSummaries in a stale one, which also
// updates the row, and a message the refresh no longer finds is read from
// the server, which says it is gone.
func TestViewFetchMessagesFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stale     bool
		expunge   bool
		flagged   bool
		source    string
		summaries int
		fetches   int
	}{
		{name: "fresh folder", flagged: false, source: cache.SourceCache},
		{name: "stale folder", stale: true, flagged: true, source: cache.SourceCache, summaries: 1},
		{name: "stale folder, message gone", stale: true, expunge: true, source: cache.SourceServer, summaries: 1, fetches: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newReadFixture(t, false)
			memo := seedReadMail(f.fk, false)["memo"]
			f.warm(t)
			if tt.stale {
				f.ageSync(t, "Archive")
			}
			f.fk.ExternalFlags("Archive", memo.UID, []string{`\Flagged`}, nil)
			if tt.expunge {
				f.fk.ExternalExpunge("Archive", memo.UID)
			}
			summaries, fetches := f.fk.Calls("FetchSummaries"), f.fk.Calls("FetchMessages")
			got, sources := f.view.FetchMessagesFrom(t.Context(), []ref.Ref{memo})
			if n := f.fk.Calls("FetchSummaries") - summaries; n != tt.summaries {
				t.Errorf("FetchSummaries calls = %d, want %d", n, tt.summaries)
			}
			if n := f.fk.Calls("FetchMessages") - fetches; n != tt.fetches {
				t.Errorf("FetchMessages calls = %d, want %d", n, tt.fetches)
			}
			if sources[0] != tt.source {
				t.Errorf("source = %q, want %q", sources[0], tt.source)
			}
			if tt.expunge {
				if !errors.Is(got[0].Err, mailstore.ErrStaleRef) {
					t.Errorf("err = %v, want ErrStaleRef", got[0].Err)
				}
				return
			}
			if got[0].Err != nil {
				t.Fatal(got[0].Err)
			}
			if has := slices.Contains(got[0].Message.Summary.Flags, `\Flagged`); has != tt.flagged {
				t.Errorf("flags = %v, want flagged %t", got[0].Message.Summary.Flags, tt.flagged)
			}
			row, _, err := f.store.Row(t.Context(), memo)
			if err != nil {
				t.Fatal(err)
			}
			if has := slices.Contains(row.Flags, `\Flagged`); has != tt.flagged {
				t.Errorf("cached flags = %v, want flagged %t", row.Flags, tt.flagged)
			}
		})
	}
}

// TestViewFetchMessagesRecoversByMessageID checks spec 7.3 on an account
// other than Google: a ref the server calls stale is found again by its
// Message-ID when exactly one other cached message has it, and returned
// under its new ref; otherwise the stale error stands.
func TestViewFetchMessagesRecoversByMessageID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup returns the ref to read and the ref it should recover to,
		// or the zero ref when it must stay stale.
		setup func(t *testing.T, f *readFixture, refs map[string]ref.Ref) (read, want ref.Ref)
	}{
		{
			name: "folder rebuilt under a new UIDVALIDITY",
			setup: func(t *testing.T, f *readFixture, refs map[string]ref.Ref) (ref.Ref, ref.Ref) {
				old := refs["re: re: plan"]
				f.resetFolder(t, "INBOX")
				found, err := f.fk.FindByMessageID(t.Context(), "INBOX", "p2@x")
				if err != nil || len(found) != 1 {
					t.Fatalf("server lookup: %v, %v", found, err)
				}
				return old, found[0]
			},
		},
		{
			name: "moved while its old row is still cached",
			setup: func(t *testing.T, f *readFixture, refs map[string]ref.Ref) (ref.Ref, ref.Ref) {
				old := refs["re: re: plan"]
				if err := f.store.DropFolder(t.Context(), "host", "INBOX", false); err != nil {
					t.Fatal(err)
				}
				res, err := f.fk.Search(t.Context(), mailstore.SearchRequest{In: "INBOX"})
				if err != nil {
					t.Fatal(err)
				}
				f.cacheFolder(t, res[0].Source, res[0].UIDs, false)
				moved := f.fk.Move(t.Context(), []ref.Ref{old}, "Archive")
				if moved[0].Err != nil {
					t.Fatal(moved[0].Err)
				}
				f.cacheFolder(t, mailstore.Source{Folder: "Archive", UIDValidity: moved[0].Ref.UIDValidity}, []uint32{moved[0].Ref.UID}, false)
				return old, moved[0].Ref
			},
		},
		{
			name: "two copies have the Message-ID",
			setup: func(t *testing.T, f *readFixture, refs map[string]ref.Ref) (ref.Ref, ref.Ref) {
				copied := f.fk.Copy(t.Context(), []ref.Ref{refs["re: re: plan"]}, "Archive")
				if copied[0].Err != nil {
					t.Fatal(copied[0].Err)
				}
				f.cacheFolder(t, mailstore.Source{Folder: "Archive", UIDValidity: copied[0].Ref.UIDValidity}, []uint32{copied[0].Ref.UID}, false)
				old := refs["re: re: plan"]
				f.resetFolder(t, "INBOX")
				return old, ref.Ref{}
			},
		},
		{
			name: "no Message-ID",
			setup: func(t *testing.T, f *readFixture, refs map[string]ref.Ref) (ref.Ref, ref.Ref) {
				old := refs["note"]
				f.resetFolder(t, "INBOX")
				return old, ref.Ref{}
			},
		},
		{
			name: "expunged, so its row is gone without recovery",
			setup: func(t *testing.T, f *readFixture, refs map[string]ref.Ref) (ref.Ref, ref.Ref) {
				old := refs["memo"]
				f.fk.ExternalExpunge("Archive", old.UID)
				if err := f.store.DeleteRow(t.Context(), old); err != nil {
					t.Fatal(err)
				}
				return old, ref.Ref{}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newReadFixture(t, false)
			refs := seedReadMail(f.fk, false)
			f.warm(t)
			read, want := tt.setup(t, f, refs)
			got, sources := f.view.FetchMessagesFrom(t.Context(), []ref.Ref{read})
			if want == (ref.Ref{}) {
				if !errors.Is(got[0].Err, mailstore.ErrStaleRef) || sources[0] != cache.SourceServer {
					t.Errorf("err %v, source %q, want ErrStaleRef from the server", got[0].Err, sources[0])
				}
				return
			}
			if got[0].Err != nil {
				t.Fatalf("recovery failed: %v", got[0].Err)
			}
			if got[0].Message.Summary.Ref != want {
				t.Errorf("recovered ref = %v, want %v", got[0].Message.Summary.Ref, want)
			}
			if got[0].Message.MessageID != "p2@x" {
				t.Errorf("recovered Message-ID = %q", got[0].Message.MessageID)
			}
		})
	}
}

// TestViewFetchMessagesLeavesGoogleRecoveryToTheServer checks that a stale
// ref on a Google account is never looked up by Message-ID: Gmail recovers
// by X-GM-MSGID in the store, and a ref without one stays stale.
func TestViewFetchMessagesLeavesGoogleRecoveryToTheServer(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t, true)
	refs := seedReadMail(f.fk, true)
	f.warm(t)
	old := refs["re: re: plan"]
	old.GmailMsgID = 0
	f.resetFolder(t, "[Gmail]/All Mail")
	got, _ := f.view.FetchMessagesFrom(t.Context(), []ref.Ref{old})
	if !errors.Is(got[0].Err, mailstore.ErrStaleRef) {
		t.Errorf("err = %v, want ErrStaleRef", got[0].Err)
	}
}
