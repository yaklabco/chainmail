package imapstore

import (
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
	"github.com/jamesainslie/chainmail/internal/testutil/fakegmail"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// TestWatchConformanceFakeGmail runs the shared watch conformance cases
// against imapstore on fakegmail, which has IDLE and no CONDSTORE, so every
// pass takes the path without CONDSTORE.
func TestWatchConformanceFakeGmail(t *testing.T) {
	t.Parallel()
	storetest.RunWatch(t, storetest.WatchHarness{Folder: storetest.GmailAllMail, Gmail: true, Idle: true, New: func(t *testing.T) (mailstore.Watcher, storetest.WatchRemote) {
		s, srv, ca := watchFixture(t)
		return s, fakeGmailRemote(t, srv, ca)
	}})
}

// fakeGmailClient logs in to srv on a connection of the test's own.
func fakeGmailClient(t *testing.T, srv *fakegmail.Server, ca *tlstest.CA) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialTLS(srv.Endpoint.Addr(), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: ca.Pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login(watchUser, "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeGmailRemote changes srv as another mail client would, on one
// connection of its own.
func fakeGmailRemote(t *testing.T, srv *fakegmail.Server, ca *tlstest.CA) storetest.WatchRemote {
	t.Helper()
	c := fakeGmailClient(t, srv, ca)
	open := func(t *testing.T, folder string, readOnly bool) {
		t.Helper()
		if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: readOnly}).Wait(); err != nil {
			t.Fatalf("selecting %s: %v", folder, err)
		}
	}
	return storetest.WatchRemote{
		Append: func(t *testing.T, folder string, m storetest.Message) uint32 {
			t.Helper()
			return uint32(seedAppend(t, c, folder, m))
		},
		SetFlag: func(t *testing.T, folder string, uid uint32, flag string, on bool) {
			t.Helper()
			open(t, folder, false)
			op := imap.StoreFlagsAdd
			if !on {
				op = imap.StoreFlagsDel
			}
			store := &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.Flag(flag)}}
			if err := c.Store(imap.UIDSetNum(imap.UID(uid)), store, nil).Close(); err != nil {
				t.Fatalf("STORE %s on %d: %v", flag, uid, err)
			}
		},
		Expunge: func(t *testing.T, folder string, uid uint32) {
			t.Helper()
			open(t, folder, false)
			deleted := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
			if err := c.Store(imap.UIDSetNum(imap.UID(uid)), deleted, nil).Close(); err != nil {
				t.Fatalf("STORE \\Deleted on %d: %v", uid, err)
			}
			if err := c.UIDExpunge(imap.UIDSetNum(imap.UID(uid))).Close(); err != nil {
				t.Fatalf("UID EXPUNGE %d: %v", uid, err)
			}
		},
		NewUIDValidity: func(_ *testing.T, folder string) { srv.NewUIDValidity(folder) },
		Flags: func(t *testing.T, folder string, uid uint32) []string {
			t.Helper()
			open(t, folder, true)
			bufs, err := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, Flags: true}).Collect()
			if err != nil || len(bufs) != 1 {
				t.Fatalf("FETCH FLAGS of %d = %v, %v", uid, bufs, err)
			}
			return flagStrings(bufs[0].Flags)
		},
	}
}

func TestWatchOpenConnsCountsWatchConn(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.OpenConns(); n != 1 {
		t.Errorf("OpenConns with one watch connection = %d, want 1", n)
	}
	if _, err := s.FolderState(t.Context(), storetest.GmailAllMail); err != nil {
		t.Fatal(err)
	}
	if n := s.OpenConns(); n != 2 {
		t.Errorf("OpenConns with a watch connection and a pooled one = %d, want 2", n)
	}
	if n := s.PoolSize(); n != defaultPoolSize {
		t.Errorf("PoolSize = %d, want %d: a watch connection takes no pool slot", n, defaultPoolSize)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	if n := s.OpenConns(); n != 1 {
		t.Errorf("OpenConns after closing the watch connection = %d, want 1", n)
	}
	if err := wc.Close(); err != nil {
		t.Errorf("a second Close = %v, want nil", err)
	}
	if n := s.OpenConns(); n != 1 {
		t.Errorf("OpenConns after a second Close = %d, want 1", n)
	}
}

func TestWatchStoreCloseClosesWatchConns(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := s.OpenConns(); n != 0 {
		t.Errorf("OpenConns after Close = %d, want 0", n)
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("Sync on a watch connection after Store.Close = %v, want ErrUnavailable", err)
	}
	if _, err := s.OpenWatch(t.Context(), storetest.GmailAllMail); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("OpenWatch after Close = %v, want ErrUnavailable", err)
	}
}

// TestWatchStoreCloseDuringIdle closes the store while a watcher idles:
// Close does not wait on the IDLE, which only DONE would end, and the
// watcher's Idle returns ErrUnavailable at once, so it can reconnect.
func TestWatchStoreCloseDuringIdle(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		_, err := wc.Idle(t.Context(), time.Minute)
		done <- err
	})
	// Wait until Idle is under way, so Close meets it.
	waitIdling(t, wc.(*watchConn), done)
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Store.Close took %v with a watcher idling, want well under the 5s LOGOUT bound", elapsed)
	}
	select {
	case err := <-done:
		if !errors.Is(err, mailstore.ErrUnavailable) {
			t.Errorf("Idle after Store.Close = %v, want ErrUnavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Idle did not return within 2s of Store.Close")
	}
	if n := s.OpenConns(); n != 0 {
		t.Errorf("OpenConns after Close = %d, want 0", n)
	}
}

func TestWatchRejectedCredentials(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	srv := fakegmail.Start(t, ca, watchUser, "pw")
	creds := passwordCreds(watchUser, "wrong")
	s := New(testAccount("gmail", srv.Endpoint, true), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })

	_, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	var rejected *auth.RejectedError
	if !errors.As(err, &rejected) || !errors.Is(err, auth.ErrRejected) {
		t.Fatalf("OpenWatch with a wrong password = %v, want a RejectedError", err)
	}
	if n := creds.rejections(); n != 1 {
		t.Errorf("rejections = %d, want 1", n)
	}
	if n := s.OpenConns(); n != 0 {
		t.Errorf("OpenConns after a rejected watch dial = %d, want 0", n)
	}
	_, err = s.OpenWatch(t.Context(), storetest.GmailAllMail)
	var required *auth.RequiredError
	if !errors.As(err, &required) {
		t.Errorf("second OpenWatch = %v, want RequiredError: a parked account is not dialed again", err)
	}
	if n := creds.rejections(); n != 1 {
		t.Errorf("rejections after the second OpenWatch = %d, want 1", n)
	}
}

// TestWatchIdleOutlastsCommandTimeout idles longer than the command
// timeout: the timeout bounds starting and ending IDLE, not the wait.
func TestWatchIdleOutlastsCommandTimeout(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	wc.(*watchConn).c.timeout = 20 * time.Millisecond
	wake, err := wc.Idle(t.Context(), 50*time.Millisecond)
	if err != nil || wake != (mailstore.Wake{}) {
		t.Fatalf("Idle(50ms) with a 20ms command timeout = %+v, %v; want Wake{} and nil", wake, err)
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Errorf("Sync after the Idle = %v; the connection must stay usable", err)
	}
}

// TestWatchDroppedConnection drops the server's connections: Sync and an
// Idle in progress both report ErrUnavailable rather than hang.
func TestWatchDroppedConnection(t *testing.T) {
	t.Parallel()
	t.Run("sync", func(t *testing.T) {
		t.Parallel()
		s, srv, _ := watchFixture(t)
		wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = wc.Close() })
		srv.DropConnections()
		if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnavailable) {
			t.Errorf("Sync on a dropped connection = %v, want ErrUnavailable", err)
		}
	})
	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		s, srv, _ := watchFixture(t)
		wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = wc.Close() })
		done := make(chan error, 1)
		var wg sync.WaitGroup
		t.Cleanup(wg.Wait)
		wg.Go(func() {
			_, err := wc.Idle(t.Context(), time.Minute)
			done <- err
		})
		srv.DropConnections()
		select {
		case err := <-done:
			if !errors.Is(err, mailstore.ErrUnavailable) {
				t.Errorf("Idle on a dropped connection = %v, want ErrUnavailable", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Idle did not return within 2s of the connection dropping")
		}
	})
}

// waitIdling waits up to 2 seconds for w to be in Idle, failing t if Idle
// returns first, with what it returned, or never starts.
func waitIdling(t *testing.T, w *watchConn, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !w.idling.Load() {
		select {
		case err := <-done:
			t.Fatalf("Idle returned %v before it was under way", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Idle was not under way within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}
