package imapstore

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// fixedCreds hands out one set of credentials and parks the account on the
// first rejection, as auth.Manager does.
type fixedCreds struct {
	mu       sync.Mutex
	creds    auth.Credentials
	parked   bool
	rejected int
	// then, when set, is the secret handed out after the first call, as if
	// the password were rotated once the daemon was running.
	then  string
	calls int
}

func (f *fixedCreds) Credentials(_ context.Context, account string) (auth.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parked {
		return auth.Credentials{}, &auth.RequiredError{Account: account, Hint: "run: chainmail secret set accounts/" + account}
	}
	f.calls++
	if f.then != "" && f.calls > 1 {
		c := f.creds
		c.Secret = f.then
		return c, nil
	}
	return f.creds, nil
}

func (f *fixedCreds) Rejected(string, auth.Credentials) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = true
	f.rejected++
}

func (f *fixedCreds) rejections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejected
}

func passwordCreds(user, pw string) *fixedCreds {
	return &fixedCreds{creds: auth.Credentials{Method: provider.AuthPassword, Username: user, Secret: pw}}
}

func noBackoff(int) time.Duration { return 0 }

func testAccount(name string, ep provider.Endpoint, gmail bool) config.ResolvedAccount {
	return config.ResolvedAccount{
		Name: name, Address: name + "@example.test", Username: name + "@example.test", Auth: provider.AuthPassword,
		Provider: provider.Preset{Name: "custom", Gmail: gmail}, IMAP: ep,
	}
}

func TestStoreParksRejectedAccount(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "right", Logins: &logins})
	creds := passwordCreds("host@example.test", "wrong")
	s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })

	_, err := s.ListFolders(t.Context())
	var rejected *auth.RejectedError
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Hint, "chainmail secret set accounts/host") {
		t.Fatalf("first ListFolders = %v, want a RejectedError with a hint", err)
	}
	_, err = s.ListFolders(t.Context())
	var required *auth.RequiredError
	if !errors.As(err, &required) {
		t.Fatalf("second ListFolders = %v, want RequiredError", err)
	}
	if logins.Load() != 1 || creds.rejections() != 1 {
		t.Errorf("logins = %d, rejections = %d; want 1 and 1", logins.Load(), creds.rejections())
	}
}

func TestStoreLogsInOnceWhenRejectedConcurrently(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "right", Logins: &logins})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "wrong"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := s.ListFolders(t.Context()); err == nil {
				t.Error("ListFolders succeeded with a wrong password")
			}
		})
	}
	wg.Wait()
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want 1: a rejected password must not be tried once per connection", n)
	}
}

func TestStoreShrinksPoolOnLimit(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw", LoginError: limit})
	creds := passwordCreds("host@example.test", "pw")
	s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.ListFolders(t.Context()); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Fatalf("ListFolders = %v, want ErrUnavailable", err)
	}
	s.pool.mu.Lock()
	poolCap := s.pool.max
	s.pool.mu.Unlock()
	if poolCap != defaultPoolSize-1 || creds.rejections() != 0 {
		t.Errorf("pool cap = %d, rejections = %d; want %d and 0", poolCap, creds.rejections(), defaultPoolSize-1)
	}
}

func TestStoreUnavailableAfterRetries(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := ln.Addr().(*net.TCPAddr)
	_ = ln.Close() // nothing listens on the port now
	ep := provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityTLS}
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{Retries: 2, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.ListFolders(t.Context())
	if !errors.Is(err, mailstore.ErrUnavailable) || !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("ListFolders = %v, want ErrUnavailable after 3 attempts", err)
	}
}

func TestStoreHonoursCancellation(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw"})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool})
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ListFolders(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ListFolders on a cancelled context = %v", err)
	}
}

func TestStoreSearchReportsLoginRefusalForTheAccount(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "right"})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "wrong"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	// Bounds skip the folder list, so the refusal first shows up in a source.
	res, err := s.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: 1}, "Archive": {UIDValidity: 1}}})
	if !errors.Is(err, auth.ErrRejected) && !errors.Is(err, auth.ErrAuthRequired) {
		t.Errorf("Search = %+v, %v; want the login refusal for the whole account", res, err)
	}
}

func TestStoreCloseWaitsForBorrowedConnections(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	raw := "From: a@example.com\r\nSubject: s\r\n\r\nbody\r\n"
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Mail: map[string][]string{"INBOX": {raw}},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool})
	res, err := s.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	r := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: res[0].Source.UIDValidity, UID: res[0].UIDs[0]}
	_, rc, err := s.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a stream still held a connection", err)
	case <-time.After(200 * time.Millisecond):
	}
	data, err := io.ReadAll(rc)
	if err != nil || string(data) != raw {
		t.Errorf("stream = %q, %v", data, err)
	}
	if err := rc.Close(); err != nil {
		t.Error(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the stream was closed")
	}
	s.pool.mu.Lock()
	open := s.pool.open
	s.pool.mu.Unlock()
	if open != 0 {
		t.Errorf("%d connections open after Close", open)
	}
	if _, err := s.ListFolders(t.Context()); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("ListFolders after Close = %v, want ErrUnavailable", err)
	}
}

func TestStoreDoesNotRetryLoginRefusals(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]*imap.Error{
		"alert":       {Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: "Application-specific password required"},
		"unavailable": {Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "try again later"},
		"bad":         {Type: imap.StatusResponseTypeBad, Text: "LOGIN is disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logins atomic.Int32
			ca := tlstest.New(t)
			ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw", LoginError: refusal, Logins: &logins})
			creds := passwordCreds("host@example.test", "pw")
			s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
			t.Cleanup(func() { _ = s.Close() })
			_, err := s.ListFolders(t.Context())
			if !errors.Is(err, mailstore.ErrUnavailable) || errors.Is(err, errConnLost) {
				t.Errorf("ListFolders = %v, want ErrUnavailable and not a lost connection", err)
			}
			if n := logins.Load(); n != 1 || creds.rejections() != 0 {
				t.Errorf("logins = %d, rejections = %d; want 1 and 0: the server answered, so the login is not retried", n, creds.rejections())
			}
		})
	}
}

func TestStoreLogsInOnceWhenThePasswordGoesBadLater(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "right", Logins: &logins})
	creds := passwordCreds("host@example.test", "right")
	s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.ListFolders(t.Context()); err != nil {
		t.Fatalf("ListFolders with the right password: %v", err)
	}
	// Drop the connection that worked, so every borrow below has to dial.
	c, err := s.borrow(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	c.kill()
	s.release(c)
	// The password is rotated while the daemon runs: the one it holds is now wrong.
	creds.mu.Lock()
	creds.creds.Secret = "stale"
	creds.mu.Unlock()
	logins.Store(0)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			c, err := s.borrow(t.Context(), "", false)
			if err == nil {
				s.release(c)
				t.Error("borrow dialed with a stale password and succeeded")
			}
		})
	}
	wg.Wait()
	if n := logins.Load(); n != 1 || creds.rejections() != 1 {
		t.Errorf("logins = %d, rejections = %d; want 1 and 1: a rejected password is tried once, not once per connection", n, creds.rejections())
	}
}

func TestStoreDialWaitingForTheLoginGateGivesUpOnCancel(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw"})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	s.loginGate <- struct{}{} // another login is in progress
	t.Cleanup(func() { <-s.loginGate })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.dial(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("dial behind a held gate = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("dial took %v to give up after a 50ms deadline", elapsed)
	}
}

func TestStoreThreadReportsLoginRefusalForTheAccount(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	raw := "From: a@example.com\r\nSubject: s\r\nMessage-ID: <t1@x>\r\n\r\nbody\r\n"
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "right", Folders: []string{"Archive", "Sent"}, Mail: map[string][]string{"INBOX": {raw}},
	})
	creds := passwordCreds("host@example.test", "right")
	creds.then = "stale" // the seed's login works; the folder searches that dial afresh are refused
	s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	res, err := s.Search(t.Context(), mailstore.SearchRequest{In: "INBOX"})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	r := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: res[0].Source.UIDValidity, UID: res[0].UIDs[0]}
	refs, err := s.Thread(t.Context(), r)
	if !errors.Is(err, auth.ErrRejected) && !errors.Is(err, auth.ErrAuthRequired) {
		t.Errorf("Thread = %v, %v; want the login refusal for the whole account, not a partial thread", refs, err)
	}
}

func TestStoreThreadReportsCancellation(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	raw := "From: a@example.com\r\nSubject: s\r\nMessage-ID: <t1@x>\r\n\r\nbody\r\n"
	// The context ends while the folder searches run: when one opens Archive.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Folders: []string{"Archive", "Sent"}, Mail: map[string][]string{"INBOX": {raw}},
		OnSelect: func(name string, _ bool) {
			if name == "Archive" {
				cancel()
			}
		},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff, PoolSize: 1})
	t.Cleanup(func() { _ = s.Close() })
	res, err := s.Search(t.Context(), mailstore.SearchRequest{In: "INBOX"})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	r := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: res[0].Source.UIDValidity, UID: res[0].UIDs[0]}
	refs, err := s.Thread(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Thread = %v, %v; want context.Canceled, not a partial thread", refs, err)
	}
}

func TestStoreRetriesADialTimeoutOnce(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close() // ends Accept; nothing to do about a close error at test end
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			// Never answer the TLS handshake; hang up when the test ends.
			wg.Go(func() {
				<-t.Context().Done()
				_ = conn.Close() // the client gave up long ago
			})
		}
	})
	addr, _ := ln.Addr().(*net.TCPAddr)
	ep := provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityTLS}
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{Backoff: noBackoff})
	s.dialTimeout = 100 * time.Millisecond
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.ListFolders(t.Context()); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("ListFolders = %v, want ErrUnavailable", err)
	}
	if n := accepted.Load(); n != 2 {
		t.Errorf("%d connection attempts, want 2: a dial that timed out is retried once, like a command", n)
	}
}

// TestStoreDoesNotResendALoginAfterAHangUp checks that a server hanging up
// after the credentials went out ends the dial: logging in again would only
// send them again.
func TestStoreDoesNotResendALoginAfterAHangUp(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw", ByeOnLogin: true, Logins: &logins})
	creds := passwordCreds("host@example.test", "pw")
	s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	_, err := s.ListFolders(t.Context())
	if !errors.Is(err, mailstore.ErrUnavailable) || errors.Is(err, errConnLost) {
		t.Errorf("ListFolders = %v, want ErrUnavailable and not a lost connection", err)
	}
	if n := logins.Load(); n != 1 || creds.rejections() != 0 {
		t.Errorf("logins = %d, rejections = %d; want 1 and 0", n, creds.rejections())
	}
}

// TestStoreCodelessTooManyIsARejection checks that a NO without a code is a
// rejection of the credentials even when its text says "too many", as a
// server that counts failed logins says.
func TestStoreCodelessTooManyIsARejection(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	ca := tlstest.New(t)
	refusal := &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Too many invalid login attempts"}
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw", LoginError: refusal, Logins: &logins})
	creds := passwordCreds("host@example.test", "pw")
	s := New(testAccount("host", ep, false), creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.ListFolders(t.Context()); !errors.Is(err, auth.ErrRejected) || errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("ListFolders = %v, want auth.ErrRejected and not unavailable", err)
	}
	s.pool.mu.Lock()
	poolCap := s.pool.max
	s.pool.mu.Unlock()
	if n := logins.Load(); n != 1 || creds.rejections() != 1 || poolCap != defaultPoolSize {
		t.Errorf("logins = %d, rejections = %d, pool cap = %d; want 1, 1 and %d", n, creds.rejections(), poolCap, defaultPoolSize)
	}
}

func TestIsConnLimit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "slow down"}, true},
		{&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: "Too many simultaneous connections. (Failure)"}, true},
		{&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: "Maximum number of connections from user+IP exceeded"}, true},
		{&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: "Application-specific password required"}, false},
		{&imap.Error{Type: imap.StatusResponseTypeNo, Text: "Too many invalid login attempts"}, false},
		{&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "too many users"}, false},
		{errors.New("too many"), false},
	} {
		if got := isConnLimit(tt.err); got != tt.want {
			t.Errorf("isConnLimit(%v) = %t, want %t", tt.err, got, tt.want)
		}
	}
}

func TestJitteredBackoffNeverPanics(t *testing.T) {
	t.Parallel()
	for _, attempt := range []int{0, 1, 8, 9, 36, 63, 64, 100} {
		if d := jitteredBackoff(attempt); d <= 0 {
			t.Errorf("jitteredBackoff(%d) = %v, want a positive wait", attempt, d)
		}
	}
	if o := (Options{Retries: 1000}).withDefaults(); o.Retries != maxRetries {
		t.Errorf("Retries 1000 became %d, want the cap %d", o.Retries, maxRetries)
	}
}

// TestStoreDoesNotResendALoginThatTimedOut checks that a LOGIN the server
// never answers is not sent again on a fresh connection, unlike a dial that
// timed out before logging in.
func TestStoreDoesNotResendALoginThatTimedOut(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := startStallingServer(t, ca, "LOGIN")
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	s.dialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { _ = s.Close() })
	_, err := s.ListFolders(t.Context())
	if !errors.Is(err, mailstore.ErrUnavailable) || !errors.Is(err, errLogin) || strings.Contains(err.Error(), "attempts") {
		t.Errorf("ListFolders = %v, want ErrUnavailable from the login, without a retry", err)
	}
}

func TestStoreOpenConnsCountsIdleAndBorrowedConnections(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	raw := "From: a@example.com\r\nSubject: s\r\n\r\nbody\r\n"
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Mail: map[string][]string{"INBOX": {raw}},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool})
	if n := s.OpenConns(); n != 0 {
		t.Fatalf("OpenConns before use = %d, want 0", n)
	}
	res, err := s.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	if n := s.OpenConns(); n != 1 {
		t.Fatalf("OpenConns with one idle connection = %d, want 1", n)
	}
	r := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: res[0].Source.UIDValidity, UID: res[0].UIDs[0]}
	_, rc, err := s.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	// Close logs out the idle connections at once; the stream's stays open.
	eventually(t, func() bool { return s.OpenConns() == 1 })
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a stream still held a connection", err)
	case <-time.After(100 * time.Millisecond):
	}
	if n := s.OpenConns(); n != 1 {
		t.Errorf("OpenConns while a stream is open after Close = %d, want 1", n)
	}
	if err := rc.Close(); err != nil {
		t.Error(err)
	}
	if err := <-closed; err != nil {
		t.Errorf("Close = %v", err)
	}
	if n := s.OpenConns(); n != 0 {
		t.Errorf("OpenConns after the stream closed = %d, want 0", n)
	}
}

// A watch connection the server refuses for being over its connection
// limit, while the pool's idle connections hold every slot, shrinks the
// pool to make room, logs out an idle connection over the new cap, and
// tells OnConnLimit, as a refused pooled dial does. The watcher's next dial
// then gets in.
func TestOpenWatchRefusedForConnLimitMakesRoom(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	conns := new(atomic.Int32)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw", MaxConns: 3, Conns: conns})
	var (
		mu    sync.Mutex
		heard []int
	)
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{
		RootCAs: ca.Pool, Backoff: noBackoff,
		OnConnLimit: func(n int) {
			mu.Lock()
			defer mu.Unlock()
			heard = append(heard, n)
		},
	})
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	// Three borrowed connections, back in the pool idle, fill the server.
	var cs []*conn
	for range defaultPoolSize {
		c, err := s.borrow(ctx, "", false)
		if err != nil {
			t.Fatalf("borrow: %v", err)
		}
		cs = append(cs, c)
	}
	for _, c := range cs {
		s.release(c)
	}
	if n := conns.Load(); n != 3 {
		t.Fatalf("server sessions = %d, want 3", n)
	}
	if _, err := s.OpenWatch(ctx, "INBOX"); !errors.Is(err, mailstore.ErrUnavailable) || !errors.Is(err, mailstore.ErrConnLimit) {
		t.Fatalf("OpenWatch with the server full = %v, want ErrUnavailable and ErrConnLimit", err)
	}
	if got, open := s.PoolSize(), s.OpenConns(); got != 2 || open != 2 {
		t.Errorf("after the refusal: pool size %d, open %d; want 2 and 2", got, open)
	}
	mu.Lock()
	got := slices.Clone(heard)
	mu.Unlock()
	if !slices.Equal(got, []int{2}) {
		t.Errorf("OnConnLimit heard %v, want [2]", got)
	}
	// The server ends the logged-out session once its socket closes.
	for deadline := time.Now().Add(10 * time.Second); conns.Load() != 2; {
		if time.Now().After(deadline) {
			t.Fatalf("server sessions = %d, want 2 once the idle connection logged out", conns.Load())
		}
		time.Sleep(time.Millisecond)
	}
	w, err := s.OpenWatch(ctx, "INBOX")
	if err != nil {
		t.Fatalf("OpenWatch after the pool made room: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// A watch connection refused while the pool holds nothing, as when the
// server still counts sessions from before a restart, leaves the pool as
// it is and records nothing: shrinking a pool that holds no slot frees
// none, and a cap stored from it would pin the pool for a month.
func TestOpenWatchRefusedWithNothingPooledChangesNothing(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "host@example.test", Password: "pw", LoginError: limit})
	var heard atomic.Int32
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{
		RootCAs: ca.Pool, Backoff: noBackoff,
		OnConnLimit: func(int) { heard.Add(1) },
	})
	t.Cleanup(func() { _ = s.Close() })
	for range 3 {
		if _, err := s.OpenWatch(t.Context(), "INBOX"); !errors.Is(err, mailstore.ErrConnLimit) {
			t.Fatalf("OpenWatch = %v, want ErrConnLimit", err)
		}
	}
	if got := s.PoolSize(); got != defaultPoolSize || heard.Load() != 0 {
		t.Errorf("after refusals with nothing pooled: pool size %d, OnConnLimit calls %d; want %d and 0", got, heard.Load(), defaultPoolSize)
	}
}
