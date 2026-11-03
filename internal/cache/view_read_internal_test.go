package cache

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// TestViewReadThroughKicksTheEvictor checks that a body the read path
// stores is followed by the size check: the evictor is woken when the file
// is over max_size, and left alone when it is under, or when nothing new
// was stored.
func TestViewReadThroughKicksTheEvictor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		maxSize int64
		cached  bool
		kicks   int
	}{
		{name: "a stored body over the cap", maxSize: 1, kicks: 1},
		{name: "a stored body under the cap", maxSize: 1 << 40, kicks: 0},
		{name: "a hit stores nothing", maxSize: 1, cached: true, kicks: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			clk := newPrefetchClock(t10Start)
			st := openEvictStore(t, clk.Now)
			conn, err := db.OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			fk := fake.New("acme", false)
			fk.SetNow(clk.Now)
			r := fk.Add(fake.Message{Folder: "INBOX", Date: t10Start.Add(-oneDay), Subject: "m", Body: "text of m"})
			fs, err := fk.FolderState(ctx, "INBOX")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutProgress(ctx, Progress{
				Account: "acme", Folder: "INBOX", UIDValidity: fs.UIDValidity, BaselineUIDNext: fs.UIDNext,
				BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: t10Start,
			}); err != nil {
				t.Fatal(err)
			}
			if tt.cached {
				res := fk.FetchMessages(ctx, []ref.Ref{r})[0]
				if res.Err != nil {
					t.Fatal(res.Err)
				}
				if err := st.PutRows(ctx, []Row{messageRow(res.Message)}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.PutBody(ctx, r, Body{Raw: res.Message.Raw}, false); err != nil {
					t.Fatal(err)
				}
			}
			a := NewAccount(AccountOptions{
				Account:  acctAccount(t, "hostinger", []string{"INBOX"}, nil),
				Settings: acctSettings,
				MaxSize:  func() int64 { return tt.maxSize },
				Store:    st,
				Log:      watch.NewLog(conn, clk.Now, nil),
				State:    watch.NewState(conn),
				Mail:     func(context.Context) (MailStore, func(), error) { return fk, func() {}, nil },
				Clock:    clk,
			})
			got, _ := a.View(fk).FetchMessagesFrom(ctx, []ref.Ref{r})
			if got[0].Err != nil {
				t.Fatal(got[0].Err)
			}
			if _, ok, err := st.Body(ctx, r); err != nil || !ok {
				t.Fatalf("body after the read: %v, %t", err, ok)
			}
			if n := len(st.evict.kickChan()); n != tt.kicks {
				t.Errorf("kicks queued = %d, want %d", n, tt.kicks)
			}
		})
	}
}

// keepRig is one tracked INBOX holding one message the cache lacks, with
// its sync_folders row, so the account can rebuild the folder, and the
// account over it.
type keepRig struct {
	a    *Account
	st   *Store
	fk   *fake.Store
	r    ref.Ref
	logs *keepLogs
}

// keepLogs collects a keepRig account's log output.
type keepLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *keepLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *keepLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newKeepRig(t *testing.T) *keepRig {
	t.Helper()
	ctx := t.Context()
	clk := newPrefetchClock(t10Start)
	st := openEvictStore(t, clk.Now)
	conn, err := db.OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fk := fake.New("acme", false)
	fk.SetNow(clk.Now)
	logs := &keepLogs{}
	r := fk.Add(fake.Message{Folder: "INBOX", Date: t10Start.Add(-oneDay), Subject: "m", MessageID: "m@x", Body: "text of m"})
	fs, err := fk.FolderState(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutProgress(ctx, Progress{
		Account: "acme", Folder: "INBOX", UIDValidity: fs.UIDValidity, BaselineUIDNext: fs.UIDNext,
		BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: t10Start,
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.PutFolder(ctx, tx, watch.FolderRow{
		Account: "acme", Folder: "INBOX", UIDValidity: fs.UIDValidity, UIDNext: fs.UIDNext,
		Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: t10Start,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a := NewAccount(AccountOptions{
		Account:  acctAccount(t, "hostinger", []string{"INBOX"}, nil),
		Settings: acctSettings,
		MaxSize:  func() int64 { return 1 << 40 },
		Store:    st,
		Log:      watch.NewLog(conn, clk.Now, nil),
		State:    watch.NewState(conn),
		Mail:     func(context.Context) (MailStore, func(), error) { return fk, func() {}, nil },
		Clock:    clk,
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
	})
	return &keepRig{a: a, st: st, fk: fk, r: r, logs: logs}
}

// TestViewKeepReadDiscardsWorkOverADrop checks that a read the server
// answered is not written when the folder is dropped or rebuilt between
// keepRead's admission and its write: the row would belong to the cache's
// old generation.
func TestViewKeepReadDiscardsWorkOverADrop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		drop func(ctx context.Context, k *keepRig) error
	}{
		{name: "the folder rebuilt", drop: func(ctx context.Context, k *keepRig) error {
			_, err := k.a.Rebuild(ctx, "INBOX")
			return err
		}},
		{name: "the account rebuilt", drop: func(ctx context.Context, k *keepRig) error {
			_, err := k.a.Rebuild(ctx, "")
			return err
		}},
		{name: "the folder dropped", drop: func(ctx context.Context, k *keepRig) error {
			return k.st.DropFolder(ctx, "acme", "INBOX", false)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			k := newKeepRig(t)
			dropped := 0
			k.a.beforeKeepWrite = func() {
				dropped++
				if err := tt.drop(ctx, k); err != nil {
					t.Error(err)
				}
			}
			got, _ := k.a.View(k.fk).FetchMessagesFrom(ctx, []ref.Ref{k.r})
			if got[0].Err != nil {
				t.Fatal(got[0].Err)
			}
			if dropped != 1 {
				t.Fatalf("drops run = %d, want 1", dropped)
			}
			if _, ok, err := k.st.Row(ctx, k.r); err != nil || ok {
				t.Errorf("row written over the drop: %v, %t", err, ok)
			}
			if _, ok, err := k.st.Body(ctx, k.r); err != nil || ok {
				t.Errorf("body written over the drop: %v, %t", err, ok)
			}
		})
	}
}

// otherAccountServer is the fake server, except that it labels each
// message it returns as another account's.
type otherAccountServer struct{ *fake.Store }

func (s otherAccountServer) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out := s.Store.FetchMessages(ctx, refs)
	for _, res := range out {
		if res.Message != nil {
			res.Message.Summary.Ref.Account = "other"
		}
	}
	return out
}

// TestViewKeepReadIgnoresAnotherAccount checks that keepRead never stores
// or charges a message that is not the view's account's.
func TestViewKeepReadIgnoresAnotherAccount(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	k := newKeepRig(t)
	got, _ := k.a.View(otherAccountServer{k.fk}).FetchMessagesFrom(ctx, []ref.Ref{k.r})
	if got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	foreign := k.r
	foreign.Account = "other"
	if _, ok, err := k.st.Row(ctx, foreign); err != nil || ok {
		t.Errorf("another account's row was stored: %v, %t", err, ok)
	}
	day := t10Start.UTC().Format("2006-01-02")
	for _, account := range []string{"acme", "other"} {
		if spent, err := k.st.Budget(ctx, account, day); err != nil || spent != 0 {
			t.Errorf("budget of %s = %d, %v, want 0", account, spent, err)
		}
	}
}

// lockGuard bounds a wait on a read that must not block on the write lock.
// It elapses only when a test fails.
const lockGuard = 10 * time.Second

// signalServer is the fake server, except that FetchMessages reports on
// fetched once it has answered.
type signalServer struct {
	*fake.Store
	fetched chan<- struct{}
}

func (s signalServer) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out := s.Store.FetchMessages(ctx, refs)
	s.fetched <- struct{}{}
	return out
}

// TestViewReadDoesNotWaitForTheWriteLockToDownload checks that a canceled
// read reaches the server while another writer holds the account's write
// lock: reading the epoch before the download takes no lock.
func TestViewReadDoesNotWaitForTheWriteLockToDownload(t *testing.T) {
	t.Parallel()
	k := newKeepRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	k.a.lock()
	fetched := make(chan struct{}, 1)
	done := make(chan mailstore.MessageResult, 1)
	go func() {
		got, _ := k.a.View(signalServer{Store: k.fk, fetched: fetched}).FetchMessagesFrom(ctx, []ref.Ref{k.r})
		done <- got[0]
	}()
	select {
	case <-fetched:
		k.a.unlock()
	case <-time.After(lockGuard):
		k.a.unlock()
		<-done
		t.Fatal("a canceled read waited for the write lock before downloading")
	}
	if got := <-done; got.Err != nil {
		t.Fatal(got.Err)
	}
}

// TestViewKeepReadGivesUpWaitingForTheWriteLock checks that the write after
// a download stops waiting for a held write lock when its bounded context
// ends: the read returns the server's message, stores nothing, logs why,
// and leaves the lock with its holder.
func TestViewKeepReadGivesUpWaitingForTheWriteLock(t *testing.T) {
	t.Parallel()
	k := newKeepRig(t)
	k.a.keepTimeout = 20 * time.Millisecond
	k.a.lock()
	done := make(chan mailstore.MessageResult, 1)
	go func() {
		got, _ := k.a.View(k.fk).FetchMessagesFrom(t.Context(), []ref.Ref{k.r})
		done <- got[0]
	}()
	var got mailstore.MessageResult
	select {
	case got = <-done:
	case <-time.After(lockGuard):
		k.a.unlock()
		<-done
		t.Fatal("the write after a download kept waiting for the held write lock")
	}
	if len(k.a.wmu) != 1 {
		t.Error("the held write lock was released")
	}
	k.a.unlock()
	if got.Err != nil {
		t.Fatal(got.Err)
	}
	if _, ok, err := k.st.Row(t.Context(), k.r); err != nil || ok {
		t.Errorf("row stored without the write lock: %v, %t", err, ok)
	}
	if logs := k.logs.String(); !strings.Contains(logs, "waiting to store a read failed") {
		t.Errorf("logs lack the give-up:\n%s", logs)
	}
}

// cacheKeepRow caches the keepRig message's row, with its body when
// withBody is set, as the server has it now.
func cacheKeepRow(t *testing.T, k *keepRig, withBody bool) {
	t.Helper()
	ctx := t.Context()
	res := k.fk.FetchMessages(ctx, []ref.Ref{k.r})[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if err := k.st.PutRows(ctx, []Row{messageRow(res.Message)}); err != nil {
		t.Fatal(err)
	}
	if !withBody {
		return
	}
	if _, err := k.st.PutBody(ctx, k.r, Body{Raw: res.Message.Raw}, false); err != nil {
		t.Fatal(err)
	}
}

// builderEvent applies one flags or expunged event of the keepRig message
// through the builder, as its group would deliver it.
func builderEvent(ctx context.Context, k *keepRig, kind watch.Kind, flags []string) error {
	return k.a.apply(ctx, []watch.Event{{Kind: kind, Account: "acme", Folder: "INBOX", Ref: k.r.String(), Flags: flags}})
}

// TestViewReadDoesNotUndoABuilderChange checks that a read the server
// answered is not written over a change the builder applied to the message
// while it was downloaded: an expunged message's row is not made again, and
// a flags change stands, though the body of a message still cached is kept.
// The download is charged either way.
func TestViewReadDoesNotUndoABuilderChange(t *testing.T) {
	t.Parallel()
	answered := []string{`\Answered`}
	tests := []struct {
		name    string
		cached  bool
		kind    watch.Kind
		wantRow bool
	}{
		{name: "an expunge of a cached row", cached: true, kind: watch.KindExpunged},
		{name: "an expunge of an uncached message", kind: watch.KindExpunged},
		{name: "a flags change of a cached row", cached: true, kind: watch.KindFlags, wantRow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			k := newKeepRig(t)
			if tt.cached {
				cacheKeepRow(t, k, false)
			}
			applied := 0
			k.a.beforeKeepWrite = func() {
				applied++
				if err := builderEvent(ctx, k, tt.kind, answered); err != nil {
					t.Error(err)
				}
			}
			got, _ := k.a.View(k.fk).FetchMessagesFrom(ctx, []ref.Ref{k.r})
			if got[0].Err != nil {
				t.Fatal(got[0].Err)
			}
			if applied != 1 {
				t.Fatalf("builder changes applied = %d, want 1", applied)
			}
			row, ok, err := k.st.Row(ctx, k.r)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.wantRow {
				t.Fatalf("row cached = %t, want %t", ok, tt.wantRow)
			}
			if tt.wantRow && !slices.Equal(row.Flags, answered) {
				t.Errorf("cached flags = %v, want the builder's %v", row.Flags, answered)
			}
			if _, ok, err := k.st.Body(ctx, k.r); err != nil || ok != tt.wantRow {
				t.Errorf("body cached = %t, %v, want %t", ok, err, tt.wantRow)
			}
			day := t10Start.UTC().Format("2006-01-02")
			if spent, err := k.st.Budget(ctx, "acme", day); err != nil || spent != int64(len(got[0].Message.Raw)) {
				t.Errorf("budget = %d, %v, want the download's %d", spent, err, len(got[0].Message.Raw))
			}
		})
	}
}

// hookServer is the fake server, except that FetchSummaries runs after
// once it has answered, before the caller sees the answer.
type hookServer struct {
	*fake.Store
	after func()
}

func (s hookServer) FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	out, err := s.Store.FetchSummaries(ctx, src, uids, opts)
	if s.after != nil {
		s.after()
	}
	return out, err
}

// staleKeepRig is a keepRig whose message is cached with its body in a
// folder that is not fresh, so a read refreshes its flags from the server,
// which has flagged it.
func staleKeepRig(t *testing.T) *keepRig {
	t.Helper()
	k := newKeepRig(t)
	cacheKeepRow(t, k, true)
	if err := k.st.SetBackfill(t.Context(), "acme", "INBOX", 1, false); err != nil {
		t.Fatal(err)
	}
	k.fk.ExternalFlags("INBOX", k.r.UID, []string{`\Flagged`}, nil)
	return k
}

// TestViewFlagRefreshDoesNotUndoABuilderChange checks that flags fetched
// from the server for a hit in a folder that is not fresh are not written
// over a flags change the builder applied while they were fetched.
func TestViewFlagRefreshDoesNotUndoABuilderChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	k := staleKeepRig(t)
	answered := []string{`\Answered`}
	server := hookServer{Store: k.fk, after: func() {
		if err := builderEvent(ctx, k, watch.KindFlags, answered); err != nil {
			t.Error(err)
		}
	}}
	got, sources := k.a.View(server).FetchMessagesFrom(ctx, []ref.Ref{k.r})
	if got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	if sources[0] != SourceCache {
		t.Errorf("source = %q, want %q", sources[0], SourceCache)
	}
	row, ok, err := k.st.Row(ctx, k.r)
	if err != nil || !ok {
		t.Fatalf("row: %v, %t", err, ok)
	}
	if !slices.Equal(row.Flags, answered) {
		t.Errorf("cached flags = %v, want the builder's %v", row.Flags, answered)
	}
}

// TestViewFlagRefreshWritesUnderTheWriteLock checks that a flag refresh
// writes only under the account's write lock: while another writer holds
// it, the read still answers with the server's flags once its bounded
// wait ends, leaves the cached flags alone, and leaves the lock with its
// holder.
func TestViewFlagRefreshWritesUnderTheWriteLock(t *testing.T) {
	t.Parallel()
	k := staleKeepRig(t)
	k.a.keepTimeout = 20 * time.Millisecond
	k.a.lock()
	done := make(chan mailstore.MessageResult, 1)
	go func() {
		got, _ := k.a.View(k.fk).FetchMessagesFrom(t.Context(), []ref.Ref{k.r})
		done <- got[0]
	}()
	var got mailstore.MessageResult
	select {
	case got = <-done:
	case <-time.After(lockGuard):
		k.a.unlock()
		<-done
		t.Fatal("the flag refresh kept waiting for the held write lock")
	}
	if len(k.a.wmu) != 1 {
		t.Error("the held write lock was released")
	}
	k.a.unlock()
	if got.Err != nil {
		t.Fatal(got.Err)
	}
	if !slices.Contains(got.Message.Summary.Flags, `\Flagged`) {
		t.Errorf("flags = %v, want the server's", got.Message.Summary.Flags)
	}
	row, ok, err := k.st.Row(t.Context(), k.r)
	if err != nil || !ok {
		t.Fatalf("row: %v, %t", err, ok)
	}
	if slices.Contains(row.Flags, `\Flagged`) {
		t.Errorf("cached flags = %v, written without the write lock", row.Flags)
	}
}
