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

// TestComposeTransportLimits checks that ComposeService accepts requests the
// size of a whole message, which MessageService's 1 MiB cap would refuse,
// and still refuses one past its own cap. The handler is not implemented
// yet, so a request that reaches it answers Unimplemented.
func TestComposeTransportLimits(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, "", daemontest.Options{})
	tests := []struct {
		name string
		size int
		want connect.Code
	}{
		{name: "2 MiB reaches the handler", size: 2 << 20, want: connect.CodeUnimplemented},
		{name: "41 MiB is refused", size: 41 << 20, want: connect.CodeResourceExhausted},
	}
	for _, tt := range tests {
		req := &chainmailv1.SendRawRequest{Account: "a", Raw: make([]byte, tt.size), IdempotencyKey: "k"}
		_, err := h.Client.Compose.SendRaw(t.Context(), connect.NewRequest(req))
		if got := connect.CodeOf(err); got != tt.want {
			t.Errorf("%s: code = %v (%v), want %v", tt.name, got, err, tt.want)
		}
	}
}
