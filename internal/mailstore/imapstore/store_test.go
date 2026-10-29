package imapstore

import (
	"context"
	"errors"
	"io"
	"net"
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
}

func (f *fixedCreds) Credentials(_ context.Context, account string) (auth.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parked {
		return auth.Credentials{}, &auth.RequiredError{Account: account, Hint: "run: chainmail secret set accounts/" + account}
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
