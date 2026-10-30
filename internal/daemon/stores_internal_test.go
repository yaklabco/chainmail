package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

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
	err := s.exclusive(context.Background(), acct, func(_ mailstore.Store, replace func() mailstore.Store) error {
		replace()
		return nil
	})
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
