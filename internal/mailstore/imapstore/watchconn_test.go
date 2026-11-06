package imapstore

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/storetest"
	"github.com/yaklabco/chainmail/internal/testutil/fakegmail"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// TestWatchConformanceFakeGmail runs the shared watch conformance cases
// against imapstore on fakegmail, which has IDLE and no CONDSTORE, so every
// pass takes the path without CONDSTORE.
func TestWatchConformanceFakeGmail(t *testing.T) {
	t.Parallel()
	// fakegmail renumbers with every connection open, so the watch
	// connection must keep working across it.
	storetest.RunWatch(t, storetest.WatchHarness{Folder: storetest.GmailAllMail, Gmail: true, Idle: true, RenumberDropsSessions: false, New: func(t *testing.T) (mailstore.Watcher, storetest.WatchRemote) {
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

// TestWatchConnPeakCountsWatchConns checks that ConnPeak counts watch
// connections with the pooled ones, as OpenConns does: a probe adds the
// peak to its own count, so leaving watchers out would read the server's
// cap low. ResetConnPeak starts from every connection open, watchers
// included, and one that closed since still counts.
func TestWatchConnPeakCountsWatchConns(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	if n := s.ConnPeak(); n != 1 {
		t.Errorf("ConnPeak after OpenWatch = %d, want 1", n)
	}
	s.ResetConnPeak()
	if n := s.ConnPeak(); n != 1 {
		t.Errorf("ConnPeak after ResetConnPeak with a watch connection open = %d, want 1", n)
	}
	if _, err := s.FolderState(t.Context(), storetest.GmailAllMail); err != nil {
		t.Fatal(err)
	}
	if n := s.ConnPeak(); n != 2 {
		t.Errorf("ConnPeak with a watch connection and a pooled one = %d, want 2", n)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	if n := s.ConnPeak(); n != 2 {
		t.Errorf("ConnPeak after the watch connection closed = %d, want 2", n)
	}
	s.ResetConnPeak()
	if n := s.ConnPeak(); n != 1 {
		t.Errorf("ConnPeak after a reset with only the pooled connection = %d, want 1", n)
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
// timeout: the timeout bounds starting and ending IDLE, not the wait. The
// command watchdog runs on the real clock, which Store.after does not
// drive, so the margins are wide instead: the timeout leaves IDLE and DONE
// room under -race and load, and the wait is three timeouts long, so a
// watchdog spanning it would fire well inside it.
func TestWatchIdleOutlastsCommandTimeout(t *testing.T) {
	t.Parallel()
	// The 600ms wait cannot use the injected clock: the watchdog it outlasts uses the real one.
	const (
		timeout = 200 * time.Millisecond
		limit   = 3 * timeout
	)
	s, _, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	wc.(*watchConn).c.timeout = timeout
	wake, err := wc.Idle(t.Context(), limit)
	if err != nil || wake != (mailstore.Wake{}) {
		t.Fatalf("Idle(%v) with a %v command timeout = %+v, %v; want Wake{} and nil", limit, timeout, wake, err)
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

// TestWatchIdleSupportedSurvivesADroppedConnection checks a connection that
// dropped before Idle still reports the IDLE its server advertised, and
// that Idle on it fails as ErrUnavailable, so the supervisor dials again
// rather than treat the server as one without IDLE.
func TestWatchIdleSupportedSurvivesADroppedConnection(t *testing.T) {
	t.Parallel()
	s, srv, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	srv.DropConnections()
	select {
	case <-wc.(*watchConn).c.client().Closed():
	case <-time.After(2 * time.Second):
		t.Fatal("the client did not see the connection drop within 2s")
	}
	if !wc.IdleSupported() {
		t.Error("IdleSupported = false after the connection dropped, want the IDLE the server advertised")
	}
	if _, err := wc.Idle(t.Context(), time.Minute); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("Idle after the connection dropped = %v, want ErrUnavailable", err)
	}
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

// TestWatchIdleLimitUsesStoreClock drives Idle's limit through the store's
// clock: Idle asks it for the limit, and returns Wake{} only once that
// fires, however long the limit.
func TestWatchIdleLimitUsesStoreClock(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t)
	asked := make(chan time.Duration, 1)
	fire := make(chan time.Time)
	s.after = func(d time.Duration) <-chan time.Time {
		asked <- d
		return fire
	}
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	type result struct {
		wake mailstore.Wake
		err  error
	}
	done := make(chan result, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		wake, err := wc.Idle(t.Context(), time.Hour)
		done <- result{wake, err}
	})
	select {
	case d := <-asked:
		if d != time.Hour {
			t.Errorf("Idle asked the clock for %v, want the limit, 1h", d)
		}
	case res := <-done:
		t.Fatalf("Idle(1h) returned %+v, %v before asking the clock", res.wake, res.err)
	case <-time.After(2 * time.Second):
		t.Fatal("Idle did not ask the store's clock for its limit within 2s")
	}
	select {
	case res := <-done:
		t.Fatalf("Idle(1h) returned %+v, %v before its limit fired", res.wake, res.err)
	default:
	}
	fire <- time.Now()
	select {
	case res := <-done:
		if res.err != nil || res.wake != (mailstore.Wake{}) {
			t.Errorf("Idle after its limit fired = %+v, %v; want Wake{} and nil", res.wake, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Idle did not return within 2s of its limit firing")
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Errorf("Sync after the Idle = %v; the connection must stay usable", err)
	}
}

// TestWatchCloseWhileDoneIsUnanswered cancels an Idle whose DONE the server
// never answers, which leaves Idle waiting out the command timeout. Close
// during that wait shuts the connection at once, as a supervisor stopping
// within its shutdown bound needs, and Idle returns.
func TestWatchCloseWhileDoneIsUnanswered(t *testing.T) {
	t.Parallel()
	s, srv, _ := watchFixture(t)
	running := make(chan struct{}, 1)
	s.after = func(time.Duration) <-chan time.Time {
		running <- struct{}{} // Idle asks for its limit once IDLE is running
		return nil
	}
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		_, err := wc.Idle(ctx, time.Minute)
		done <- err
	})
	select {
	case <-running:
	case err := <-done:
		t.Fatalf("Idle returned %v before IDLE was running", err)
	case <-time.After(2 * time.Second):
		t.Fatal("IDLE was not running within 2s")
	}
	held, release := srv.HoldReads()
	defer release()
	cancel() // Idle sends DONE, which the server now holds unanswered
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("Idle returned %v before the server got DONE", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the server did not get DONE within 2s")
	}
	start := time.Now()
	closed := make(chan error, 1)
	wg.Go(func() { closed <- wc.Close() })
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close while DONE was unanswered = %v, want nil: it shuts the socket rather than log out", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Close took %v while DONE was unanswered, want well under 1s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s while DONE was unanswered")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("Idle closed during DONE = nil, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Idle did not return within 2s of Close")
	}
	if n := s.OpenConns(); n != 0 {
		t.Errorf("OpenConns after Close = %d, want 0", n)
	}
}

// TestWatchAbortEndsAnUnansweredLogout closes a watch connection whose
// LOGOUT the server never answers, which leaves Close waiting out its 5
// second bound. Abort, the optional mailstore.Aborter, shuts the connection
// at once: Close returns promptly and the connection is released.
func TestWatchAbortEndsAnUnansweredLogout(t *testing.T) {
	t.Parallel()
	s, srv, _ := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := wc.(mailstore.Aborter)
	if !ok {
		_ = wc.Close()
		t.Fatal("imapstore's watch connection does not implement mailstore.Aborter")
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	held, release := srv.HoldReads()
	defer release()
	closed := make(chan error, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() { closed <- wc.Close() })
	select {
	case <-held:
	case err := <-closed:
		t.Fatalf("Close returned %v before the server got LOGOUT", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the server did not get LOGOUT within 2s")
	}
	start := time.Now()
	a.Abort()
	a.Abort() // idempotent
	select {
	case err := <-closed:
		if err == nil {
			t.Error("Close cut short by Abort = nil, want the failed LOGOUT reported")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Close returned %v after Abort, want well under 1s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s of Abort")
	}
	if n := s.OpenConns(); n != 0 {
		t.Errorf("OpenConns after Abort and Close = %d, want 0", n)
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("Sync after Abort = %v, want ErrUnavailable", err)
	}
}

// TestWatchIdleCancelledBeforeContinuation cancels an Idle while the server
// has yet to answer IDLE with its continuation: Idle still waits for it,
// ends IDLE with DONE and returns the context's error, and the connection
// stays selected and usable.
func TestWatchIdleCancelledBeforeContinuation(t *testing.T) {
	t.Parallel()
	s, srv, ca := watchFixture(t)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	d0, err := wc.Sync(t.Context(), mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	held, release := srv.HoldReads()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		_, err := wc.Idle(ctx, time.Minute)
		done <- err
	})
	select {
	case <-held:
	case err := <-done:
		release()
		t.Fatalf("Idle returned %v before the server got IDLE", err)
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("the server did not get IDLE within 2s")
	}
	cancel()
	release()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Idle cancelled before the continuation = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Idle did not return within 2s of the release")
	}
	u1 := fakeGmailRemote(t, srv, ca).Append(t, storetest.GmailAllMail, storetest.Message{
		Date: gmailDay(3), From: "sender@example.test", Subject: "after cancel", MessageID: "cancel@watch.example", Body: "x",
	})
	d, err := wc.Sync(t.Context(), mailstore.SyncRequest{From: mailstore.Checkpoint{UIDValidity: d0.State.UIDValidity, UIDNext: d0.State.UIDNext}})
	if err != nil {
		t.Fatalf("Sync after an Idle cancelled before its continuation = %v; the connection must stay usable", err)
	}
	if len(d.New) != 1 || d.New[0].Ref.UID != u1 {
		t.Errorf("Sync after the cancelled Idle found %d new, want UID %d", len(d.New), u1)
	}
}

// TestWatchSyncReportsUnannouncedExpunges: Gmail tells an IDLE of an
// expunge late, and a SELECT drops whatever it had not said yet, so a watch
// connection whose IDLE keeps ending early can miss every EXPUNGE. Sync
// compares its SELECT's message count with the last one on the connection
// plus the new mail between them, and reports a shortfall as an expunge, so
// the next Idle returns one at once. New mail alone, or no change, reports
// no expunge.
func TestWatchSyncReportsUnannouncedExpunges(t *testing.T) {
	t.Parallel()
	s, srv, ca := watchFixture(t,
		fakegmail.Message{Raw: "Subject: one\r\n\r\nx\r\n", Date: gmailDay(1)},
		fakegmail.Message{Raw: "Subject: two\r\n\r\ny\r\n", Date: gmailDay(2)},
		fakegmail.Message{Raw: "Subject: three\r\n\r\nz\r\n", Date: gmailDay(3)},
	)
	remote := fakeGmailRemote(t, srv, ca)
	wc, err := s.OpenWatch(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	var from mailstore.Checkpoint
	// pass syncs from the last checkpoint and returns what the next Idle
	// would report at once.
	pass := func(desc string) mailstore.Wake {
		t.Helper()
		d, err := wc.Sync(t.Context(), mailstore.SyncRequest{From: from})
		if err != nil {
			t.Fatalf("%s: Sync: %v", desc, err)
		}
		from = mailstore.Checkpoint{UIDValidity: d.State.UIDValidity, UIDNext: d.State.UIDNext, HighestModSeq: d.State.HighestModSeq}
		wake, err := wc.Idle(t.Context(), 0)
		if err != nil {
			t.Fatalf("%s: Idle: %v", desc, err)
		}
		return wake
	}
	pass("baseline")
	remote.Append(t, storetest.GmailAllMail, storetest.Message{Subject: "four", Date: gmailDay(4), Body: "w"})
	if wake := pass("new mail"); wake.Expunged {
		t.Errorf("after new mail alone, Idle reported %+v, want no expunge", wake)
	}
	if wake := pass("no change"); wake != (mailstore.Wake{}) {
		t.Errorf("after no change, Idle reported %+v, want nothing", wake)
	}
	remote.Expunge(t, storetest.GmailAllMail, 2)
	remote.Append(t, storetest.GmailAllMail, storetest.Message{Subject: "five", Date: gmailDay(5), Body: "v"})
	if wake := pass("an expunge and new mail"); !wake.Changed || !wake.Expunged {
		t.Errorf("after an expunge and new mail, Idle reported %+v, want an expunge", wake)
	}
	if wake := pass("no change again"); wake != (mailstore.Wake{}) {
		t.Errorf("after no change again, Idle reported %+v, want nothing", wake)
	}
}
