package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
)

// closeFails is a store whose logout fails with text from the server.
type closeFails struct{ mailstore.Store }

func (closeFails) Close() error {
	return fmt.Errorf("%w: * BYE secret-server-text", mailstore.ErrUnavailable)
}

// A store replaced after a probe may fail to log out. The log says why in
// a reason code, never in the server's words.
func TestReplacedStoreLogoutLogsNoServerText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := &storeSet{
		build:  func(acct config.ResolvedAccount) mailstore.Store { return closeFails{fake.New(acct.Name, false)} },
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	acct := config.ResolvedAccount{Name: "host"}
	// A call builds the store the replace then logs out of.
	_, release, err := s.entry(acct).acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	err = s.replace(context.Background(), acct, 1)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret-server-text") {
		t.Errorf("log = %q, holds the server's text", buf.String())
	}
	if !strings.Contains(buf.String(), "logging out of the replaced store") {
		t.Errorf("log = %q, want the logout failure recorded", buf.String())
	}
}

// slowClose is a store whose logout waits for release, as one does while a
// stream reader holds a connection.
type slowClose struct {
	mailstore.Store
	closing chan struct{} // closed once Close starts
	release chan struct{}
}

func (s *slowClose) Close() error {
	close(s.closing)
	<-s.release
	return s.Store.Close()
}

// The write lock is held only to swap stores: calls on the new store go on
// while the old one logs out.
func TestReplaceDoesNotHoldCallsWhileTheOldStoreLogsOut(t *testing.T) {
	t.Parallel()
	first := &slowClose{Store: fake.New("host", false), closing: make(chan struct{}), release: make(chan struct{})}
	built := 0
	s := &storeSet{build: func(acct config.ResolvedAccount) mailstore.Store {
		built++
		if built == 1 {
			return first
		}
		return fake.New(acct.Name, false)
	}}
	acct := config.ResolvedAccount{Name: "host"}
	h := s.get(acct)
	if _, err := h.ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.replace(t.Context(), acct, 1) }()
	<-first.closing
	defer close(first.release)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := h.ListFolders(ctx); err != nil {
		t.Fatalf("ListFolders while the old store logs out: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("replace returned %v before the old store logged out", err)
	default:
	}
}
