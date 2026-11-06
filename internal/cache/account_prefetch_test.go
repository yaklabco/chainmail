package cache

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/watch"
)

// prefetchAccount is an Account over a fake store whose INBOX the watch
// engine tracks and watches, with a real state.db and cache.db, all on a
// test clock that fires only when the test moves it. msg is a message that
// was in INBOX before the cache tracked the folder.
type prefetchAccount struct {
	clk *prefetchClock
	st  *Store
	fk  *fake.Store
	a   *Account
	msg ref.Ref
}

func newPrefetchAccount(t *testing.T) prefetchAccount {
	t.Helper()
	ctx := t.Context()
	clk := newPrefetchClock(t10Start)
	st := openEvictStore(t, clk.Now)
	stateDB, err := db.OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() }) // a close error at test end changes nothing
	fk := fake.New("acme", false)
	msg := fk.Add(fake.Message{Folder: "INBOX", Subject: "before tracking", Date: t10Start.Add(-oneDay)})
	fs, err := fk.FolderState(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := stateDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.PutFolder(ctx, tx, watch.FolderRow{
		Account: "acme", Folder: "INBOX", UIDValidity: fs.UIDValidity, UIDNext: fs.UIDNext,
		Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: t10Start,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a := NewAccount(AccountOptions{
		Account: config.ResolvedAccount{
			Name: "acme", Watch: []string{"INBOX"}, SyncFolders: []string{"INBOX"},
			SyncPoll: 5 * time.Minute, SyncEnabled: true,
		},
		Settings: func() config.CacheSettings {
			return config.CacheSettings{
				Enabled: true, PrefetchBodies: 90 * oneDay, ReadRetention: 30 * oneDay,
				DailyDownload: 1 << 30, BackfillBatch: 250, FreshLag: 30 * time.Second,
			}
		},
		MaxSize: func() int64 { return 1 << 40 },
		Store:   st,
		Log:     watch.NewLog(stateDB, clk.Now, slog.New(slog.DiscardHandler)),
		State:   watch.NewState(stateDB),
		Mail:    func(context.Context) (MailStore, func(), error) { return fk, func() {}, nil },
		Clock:   clk,
	})
	return prefetchAccount{clk: clk, st: st, fk: fk, a: a, msg: msg}
}

// waitBody moves the clock a minute at a time, which fires every wait of
// the loops whatever its length, until r's body is cached.
func (p prefetchAccount) waitBody(t *testing.T, r ref.Ref) {
	t.Helper()
	pollUntil(t, "the body through Account.Run", func() bool {
		p.clk.Advance(time.Minute)
		return hasBody(t, p.st, r)
	})
}

// Account.Run starts the prefetcher with the builder and the backfill: a
// message already in INBOX when the cache first tracks the folder gets its
// header from the backfill, then its body from the prefetcher.
func TestAccountRunPrefetchesBodies(t *testing.T) {
	t.Parallel()
	p := newPrefetchAccount(t)
	runCtx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { p.a.Run(runCtx) })
	defer func() {
		cancel()
		wg.Wait()
	}()
	p.waitBody(t, p.msg)
	if got := p.fk.BackgroundCalls("FetchMessages"); got == 0 {
		t.Errorf("BackgroundCalls(FetchMessages) = 0, want the prefetch on a background connection")
	}
}

// Ending Run's context stops the prefetcher with the other workers: Run
// returns, and a bodyless window header stored afterwards is never
// fetched however far the clock moves.
func TestAccountRunStopsPrefetchWithContext(t *testing.T) {
	t.Parallel()
	p := newPrefetchAccount(t)
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.a.Run(runCtx)
	}()
	p.waitBody(t, p.msg)
	cancel()
	select {
	case <-done:
	case <-time.After(acctWait):
		t.Fatal("Run did not return after cancel")
	}

	late := p.fk.Add(fake.Message{Folder: "INBOX", Subject: "after stop", Date: t10Start})
	if err := p.st.PutRows(t.Context(), []Row{{
		Account: late.Account, Folder: late.Folder, UIDValidity: late.UIDValidity, UID: late.UID,
		InternalDate: t10Start, Date: t10Start, Subject: "after stop",
	}}); err != nil {
		t.Fatal(err)
	}
	before := p.fk.Calls("FetchMessages")
	for range 10 {
		p.clk.Advance(time.Hour)
	}
	if got := p.fk.Calls("FetchMessages"); got != before {
		t.Errorf("FetchMessages calls after Run returned = %d, want %d", got, before)
	}
	if hasBody(t, p.st, late) {
		t.Error("a body was fetched after Run returned")
	}
}
