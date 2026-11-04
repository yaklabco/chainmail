package imapstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

const backgroundRaw = "From: a@example.com\r\nSubject: s\r\n\r\nbody\r\n"

// backgroundStore returns a store with a pool of size (0 means the
// default) on a test server holding one message in INBOX, and counters of
// its logins and of its backoff waits, which a retry takes. loginErr, when
// set, answers every login. onLimit becomes Options.OnConnLimit.
func backgroundStore(t *testing.T, size int, loginErr error, onLimit func(int)) (*Store, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	logins, backoffs := new(atomic.Int32), new(atomic.Int32)
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Logins: logins, LoginError: loginErr,
		Mail: map[string][]string{"INBOX": {backgroundRaw}},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{
		RootCAs: ca.Pool, PoolSize: size, OnConnLimit: onLimit,
		Backoff: func(int) time.Duration {
			backoffs.Add(1)
			return 0
		},
	})
	t.Cleanup(func() { _ = s.Close() })
	return s, logins, backoffs
}

// holdConn opens a stream reader on INBOX's one message, which keeps a
// pooled connection borrowed, and returns the message's ref and a func
// that closes the reader. The func may run twice; t's cleanup runs it
// before the store closes.
func holdConn(t *testing.T, s *Store) (ref.Ref, func()) {
	t.Helper()
	res, err := s.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	r := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: res[0].Source.UIDValidity, UID: res[0].UIDs[0]}
	_, rc, err := s.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { _ = rc.Close() })
	t.Cleanup(release)
	return r, release
}

// backgroundCtx is a background context bounded by a second, so a call
// that waits instead of failing fails the test rather than hanging it.
func backgroundCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(mailstore.WithBackground(t.Context()), time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A background call on a pool with no connection to spare fails at once
// with ErrBusy, which is not ErrUnavailable, is not retried and logs in no
// more. Once the connection is back, background work reuses it.
func TestStoreBackgroundBusyWhenThePoolIsFull(t *testing.T) {
	t.Parallel()
	s, logins, backoffs := backgroundStore(t, 1, nil, nil)
	_, release := holdConn(t, s)
	ctx := backgroundCtx(t)
	_, err := s.ListFolders(ctx)
	if !errors.Is(err, mailstore.ErrBusy) || errors.Is(err, mailstore.ErrUnavailable) {
		t.Fatalf("background ListFolders on a full pool = %v, want ErrBusy and not ErrUnavailable", err)
	}
	if n := backoffs.Load(); n != 0 {
		t.Errorf("backoff waits = %d, want 0: ErrBusy is never retried", n)
	}
	release()
	if _, err := s.ListFolders(ctx); err != nil {
		t.Fatalf("background ListFolders with an idle connection = %v", err)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want 1: background work reuses the idle connection", n)
	}
}

// Every read reports ErrBusy where it reports a failure today: the call's
// error, a source's for Search, an item's for FetchMessages.
func TestStoreBackgroundReadsReportBusy(t *testing.T) {
	t.Parallel()
	s, _, backoffs := backgroundStore(t, 1, nil, nil)
	r, _ := holdConn(t, s)
	ctx := backgroundCtx(t)
	src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
	reads := []struct {
		name string
		read func() error
	}{
		{"ListFolders", func() error {
			_, err := s.ListFolders(ctx)
			return err
		}},
		{"Search", func() error {
			res, err := s.Search(ctx, mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{r.Folder: {UIDValidity: r.UIDValidity}}})
			if err != nil {
				return err
			}
			return res[0].Err
		}},
		{"FetchSummaries", func() error {
			_, err := s.FetchSummaries(ctx, src, []uint32{r.UID}, mailstore.SummaryOptions{})
			return err
		}},
		{"FetchMessages", func() error { return s.FetchMessages(ctx, []ref.Ref{r})[0].Err }},
		{"Thread", func() error {
			_, err := s.Thread(ctx, r)
			return err
		}},
		{"FolderState", func() error {
			_, err := s.FolderState(ctx, r.Folder)
			return err
		}},
	}
	for _, rd := range reads {
		if err := rd.read(); !errors.Is(err, mailstore.ErrBusy) {
			t.Errorf("background %s on a full pool = %v, want ErrBusy", rd.name, err)
		}
	}
	if n := backoffs.Load(); n != 0 {
		t.Errorf("backoff waits = %d, want 0", n)
	}
}

// With room in the pool and no one waiting, background work dials.
func TestStoreBackgroundDialsWhenThePoolHasRoom(t *testing.T) {
	t.Parallel()
	s, logins, _ := backgroundStore(t, 2, nil, nil)
	holdConn(t, s)
	if _, err := s.ListFolders(backgroundCtx(t)); err != nil {
		t.Fatalf("background ListFolders with room in the pool = %v", err)
	}
	if n := logins.Load(); n != 2 {
		t.Errorf("logins = %d, want 2", n)
	}
}

// A background dial refused for being over the server's connection limit
// shrinks the pool and is heard by OnConnLimit, as any refusal is, and the
// caller is told to come back later rather than that the account is down.
func TestStoreBackgroundRefusedForTheLimitIsBusy(t *testing.T) {
	t.Parallel()
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
	var heard atomic.Int32
	s, logins, backoffs := backgroundStore(t, 0, limit, func(n int) { heard.Store(int32(n)) })
	_, err := s.ListFolders(backgroundCtx(t))
	if !errors.Is(err, mailstore.ErrBusy) || errors.Is(err, mailstore.ErrUnavailable) {
		t.Fatalf("background ListFolders refused for the limit = %v, want ErrBusy and not ErrUnavailable", err)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want 1", n)
	}
	if n := backoffs.Load(); n != 0 {
		t.Errorf("backoff waits = %d, want 0", n)
	}
	if got := s.PoolSize(); got != defaultPoolSize-1 {
		t.Errorf("pool size = %d, want %d", got, defaultPoolSize-1)
	}
	if got := heard.Load(); got != defaultPoolSize-1 {
		t.Errorf("OnConnLimit heard %d, want %d", got, defaultPoolSize-1)
	}
}

// Foreground calls keep working as before on a store that also serves
// background work.
func TestStoreForegroundStillWaitsForAConnection(t *testing.T) {
	t.Parallel()
	s, _, _ := backgroundStore(t, 1, nil, nil)
	_, release := holdConn(t, s)
	// The waiting call runs under a context this test cancels, and the
	// cleanup, registered after the store's and holdConn's so that it runs
	// before them, joins it before they close anything it uses, on every
	// path out of the test.
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	done := make(chan error, 1)
	wg.Go(func() {
		_, err := s.ListFolders(ctx)
		done <- err
	})
	eventually(t, func() bool { return waiters(s.pool) == 1 })
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("foreground ListFolders after the wait = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting foreground call never got the connection")
	}
}

// StreamPart ignores the background mark: the cache never streams, and a
// reader holds its connection until it is closed, so it waits for a
// connection as a foreground call does.
func TestStreamPartIgnoresTheBackgroundMark(t *testing.T) {
	t.Parallel()
	s, _, _ := backgroundStore(t, 1, nil, nil)
	r, release := holdConn(t, s)
	ctx, cancel := context.WithTimeout(mailstore.WithBackground(t.Context()), 30*time.Millisecond)
	defer cancel()
	if _, _, err := s.StreamPart(ctx, r, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("background StreamPart on a full pool = %v, want it to wait as a foreground call does", err)
	}
	release()
	_, rc, err := s.StreamPart(mailstore.WithBackground(t.Context()), r, "")
	if err != nil {
		t.Fatalf("background StreamPart with an idle connection = %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Error(err)
	}
}
