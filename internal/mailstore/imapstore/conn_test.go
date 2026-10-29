package imapstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

func newTestConn(t *testing.T, u imaptest.User) *conn {
	t.Helper()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, u)
	creds := auth.Credentials{Method: provider.AuthPassword, Username: u.Username, Secret: u.Password}
	sess, err := Dial(t.Context(), ep, creds, DialOptions{RootCAs: ca.Pool})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c := newConn(sess, 5*time.Second, time.Now)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestExamineIsReadOnlyAndCached(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var selects []string
	u := imaptest.User{
		Username: "ro@example.test", Password: "pw", Folders: []string{"Archive"},
		OnSelect: func(name string, readOnly bool) {
			mu.Lock()
			defer mu.Unlock()
			selects = append(selects, fmt.Sprintf("%s read-only=%t", name, readOnly))
		},
	}
	c := newTestConn(t, u)
	for _, f := range []string{"INBOX", "INBOX", "Archive", "INBOX"} {
		if err := c.examine(t.Context(), f); err != nil {
			t.Fatalf("examine %s: %v", f, err)
		}
	}
	mu.Lock()
	got := slices.Clone(selects)
	mu.Unlock()
	want := []string{"INBOX read-only=true", "Archive read-only=true", "INBOX read-only=true"}
	if !slices.Equal(got, want) {
		t.Errorf("selects = %q, want %q", got, want)
	}
	if c.Selected() != "INBOX" || c.uidValidity == 0 {
		t.Errorf("selected %q, UIDVALIDITY %d", c.Selected(), c.uidValidity)
	}
}

func TestExamineMissingFolder(t *testing.T) {
	t.Parallel()
	c := newTestConn(t, imaptest.User{Username: "missing@example.test", Password: "pw"})
	if err := c.examine(t.Context(), "Nope"); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("examine Nope = %v, want ErrNotFound", err)
	}
	if c.Selected() != "" || c.Dead() {
		t.Errorf("after a missing folder: selected %q, dead %t", c.Selected(), c.Dead())
	}
}

func TestDoClassifiesErrors(t *testing.T) {
	t.Parallel()
	c := newTestConn(t, imaptest.User{Username: "do@example.test", Password: "pw"})
	refusal := &imap.Error{Type: imap.StatusResponseTypeNo, Text: "no"}
	if err := c.do(t.Context(), func() error { return refusal }); !errors.Is(err, refusal) || errors.Is(err, errConnLost) || c.Dead() {
		t.Errorf("a NO reply: err %v, dead %t; want the reply, connection alive", err, c.Dead())
	}
	if err := c.Noop(t.Context()); err != nil {
		t.Fatalf("Noop: %v", err)
	}
	if err := c.do(t.Context(), func() error { return io.ErrUnexpectedEOF }); !errors.Is(err, errConnLost) || !c.Dead() {
		t.Errorf("a drop: err %v, dead %t; want errConnLost and dead", err, c.Dead())
	}
	if err := c.do(t.Context(), func() error { return nil }); !errors.Is(err, errConnLost) {
		t.Errorf("a dead connection ran a command: %v", err)
	}
}

func TestDoCancelKillsConnection(t *testing.T) {
	t.Parallel()
	c := newTestConn(t, imaptest.User{Username: "cancel@example.test", Password: "pw"})
	ctx, cancel := context.WithCancel(t.Context())
	err := c.do(ctx, func() error {
		cancel()
		return c.client().Noop().Wait()
	})
	if !errors.Is(err, context.Canceled) || !c.Dead() {
		t.Errorf("cancel: err %v, dead %t; want context.Canceled and dead", err, c.Dead())
	}
}

func TestDoTimeoutKillsConnection(t *testing.T) {
	t.Parallel()
	c := newTestConn(t, imaptest.User{Username: "slow@example.test", Password: "pw"})
	c.timeout = 20 * time.Millisecond
	err := c.do(t.Context(), func() error {
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	if !errors.Is(err, errConnLost) || !c.Dead() {
		t.Errorf("timeout: err %v, dead %t; want errConnLost and dead", err, c.Dead())
	}
}

func TestPipelineTimeoutBoundsEachCommand(t *testing.T) {
	t.Parallel()
	const timeout = 150 * time.Millisecond
	steps := func(progress func()) error {
		for range 4 {
			time.Sleep(timeout / 2)
			progress()
		}
		return nil
	}
	c := newTestConn(t, imaptest.User{Username: "steps@example.test", Password: "pw"})
	c.timeout = timeout
	if err := c.pipeline(t.Context(), steps); err != nil || c.Dead() {
		t.Errorf("four commands each within the timeout: err %v, dead %t; want no error", err, c.Dead())
	}
	quiet := newTestConn(t, imaptest.User{Username: "quiet@example.test", Password: "pw"})
	quiet.timeout = timeout
	err := quiet.pipeline(t.Context(), func(func()) error { return steps(func() {}) })
	if !errors.Is(err, errCommandTimeout) || !errors.Is(err, errConnLost) || !quiet.Dead() {
		t.Errorf("no progress for longer than the timeout: err %v, dead %t; want a timeout", err, quiet.Dead())
	}
}
