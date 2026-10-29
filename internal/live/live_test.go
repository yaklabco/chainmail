//go:build live

package live

import (
	"os"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
)

func TestEveryAccountConnects(t *testing.T) {
	socket := os.Getenv("CHAINMAIL_SOCKET")
	if socket == "" {
		t.Fatal("set CHAINMAIL_SOCKET to the running daemon's socket")
	}
	c := client.New(socket, "live-test")
	defer c.Close()
	list, err := c.Account.ListAccounts(t.Context(), connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		t.Fatalf("ListAccounts: %v (%s)", err, api.Detail(err).GetHint())
	}
	if len(list.Msg.GetAccounts()) == 0 {
		t.Fatal("no accounts configured")
	}
	for _, a := range list.Msg.GetAccounts() {
		t.Run(a.GetName(), func(t *testing.T) {
			resp, err := c.Account.TestAccount(t.Context(), connect.NewRequest(&chainmailv1.TestAccountRequest{Account: a.GetName()}))
			if err != nil {
				t.Fatalf("%v (%s)", err, api.Detail(err).GetHint())
			}
			for _, check := range []struct {
				name string
				c    *chainmailv1.Check
			}{{"imap", resp.Msg.GetImap()}, {"smtp", resp.Msg.GetSmtp()}} {
				if !check.c.GetOk() {
					t.Errorf("%s: %s: %s", check.name, check.c.GetReason(), check.c.GetError())
					continue
				}
				t.Logf("%s ok in %dms, capabilities %v", check.name, check.c.GetLatencyMs(), check.c.GetCapabilities())
			}
		})
	}
}
