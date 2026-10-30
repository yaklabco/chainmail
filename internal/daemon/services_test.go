package daemon_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// TestWriteServicesAreRegistered checks that the daemon serves the compose
// and organize services and ProbeLimits. An unregistered route answers HTTP
// 404, which Connect reports in the error text, so any answer but that one
// means the route exists, whether or not the handler is implemented yet.
func TestWriteServicesAreRegistered(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, "", daemontest.Options{})
	calls := map[string]func() error{
		"ComposeService.Send": func() error {
			_, err := h.Client.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{}))
			return err
		},
		"OrganizeService.Move": func() error {
			_, err := h.Client.Organize.Move(t.Context(), connect.NewRequest(&chainmailv1.MoveRequest{}))
			return err
		},
		"AccountService.ProbeLimits": func() error {
			_, err := h.Client.Account.ProbeLimits(t.Context(), connect.NewRequest(&chainmailv1.ProbeLimitsRequest{}))
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err != nil && strings.Contains(err.Error(), "404") {
			t.Errorf("%s: err = %v, want the route served", name, err)
		}
	}
}
