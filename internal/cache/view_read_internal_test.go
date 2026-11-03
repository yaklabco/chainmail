package cache

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
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
