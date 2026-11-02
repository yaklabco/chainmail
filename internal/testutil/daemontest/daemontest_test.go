package daemontest_test

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

const hostConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

// A harness started with Reuse is a restart of the daemon it reuses: it
// stops the old one, then serves the same secrets, state database and
// socket path, so the audit rows written before the restart are still
// there.
func TestReuseRestartsOnTheSameState(t *testing.T) {
	t.Parallel()
	first := daemontest.Start(t, hostConfig, daemontest.Options{})
	if _, err := first.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{
		Name: "accounts/host", Value: []byte("hunter2"),
	})); err != nil {
		t.Fatal(err)
	}
	second := daemontest.Start(t, hostConfig, daemontest.Options{Reuse: first})
	// Start stopped the first daemon; Stop returns that first result.
	if err := first.Stop(); err != nil {
		t.Errorf("first daemon: %v", err)
	}
	if second.DB != first.DB || second.Socket != first.Socket {
		t.Error("the restarted daemon has a state database or socket of its own")
	}
	resp, err := second.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus after the restart: %v", err)
	}
	if got := resp.Msg.GetSecretNames(); !slices.Equal(got, []string{"accounts/host"}) {
		t.Errorf("secret names after the restart = %v, want [accounts/host]", got)
	}
	sawSet := false
	for _, r := range second.AuditRows(t) {
		if r.Action == "secret.set" {
			sawSet = true
		}
	}
	if !sawSet {
		t.Error("the secret.set audit row written before the restart is gone")
	}
}
