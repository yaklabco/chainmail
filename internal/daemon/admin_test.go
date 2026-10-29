package daemon_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/daemon"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/oauthtest"
)

const hostingerConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

const gmailConfig = `
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"

[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

func setSecret(t *testing.T, h *daemontest.Harness, name, value string) {
	t.Helper()
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: name, Value: []byte(value)}))
	if err != nil {
		t.Fatalf("SetSecret(%s): %v", name, err)
	}
}

func TestSetSecretShowsInStatus(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	setSecret(t, h, "accounts/host", "hunter2")
	resp, err := h.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Msg.GetSecretNames(), []string{"accounts/host"}) {
		t.Errorf("secret names = %v", resp.Msg.GetSecretNames())
	}
	if !resp.Msg.GetDevMode() || resp.Msg.GetVersion() != "dev" || resp.Msg.GetStartedAt() == nil {
		t.Errorf("status = %+v", resp.Msg)
	}
}

func TestSetSecretIsAuditedWithoutValue(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	setSecret(t, h, "accounts/host", "hunter2")
	rows := h.AuditRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v", rows)
	}
	r := rows[0]
	if r.Action != "secret.set" || r.Targets != `["accounts/host"]` || r.Result != "ok" || r.Client != "test" {
		t.Errorf("row = %+v", r)
	}
	for _, field := range []string{r.Action, r.Targets, r.Reason, r.Account, r.Client} {
		if strings.Contains(field, "hunter2") {
			t.Error("audit row contains the secret value")
		}
	}
}

func TestSetSecretRejectsInvalidName(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: "../x", Value: []byte("v")}))
	if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
		t.Errorf("reason = %s, want INVALID_ARGUMENT (err %v)", got, err)
	}
	if rows := h.AuditRows(t); len(rows) != 1 || rows[0].Result != "error" || rows[0].Reason != api.ReasonInvalidArgument {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestOAuthLogin(t *testing.T) {
	t.Parallel()
	o := oauthtest.Start(t)
	h := daemontest.Start(t, gmailConfig, daemontest.Options{AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())}})
	setSecret(t, h, "oauth_clients/g", oauthtest.ClientSecret)

	begin, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: "gm"}))
	if err != nil {
		t.Fatalf("BeginAuthLogin: %v", err)
	}
	if begin.Msg.GetExpiresAt() == nil || !strings.HasPrefix(begin.Msg.GetConsentUrl(), o.URL) {
		t.Errorf("begin = %+v", begin.Msg)
	}
	redirect := oauthtest.RedirectFor(t, begin.Msg.GetConsentUrl(), oauthtest.Code)
	if _, err := h.Client.Admin.FinishAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: "gm", RedirectUrl: redirect})); err != nil {
		t.Fatalf("FinishAuthLogin: %v", err)
	}
	rt, err := h.Vault.Get(auth.RefreshTokenSecret("gm"))
	if err != nil || string(rt) != oauthtest.RefreshToken {
		t.Errorf("refresh token = %q, %v", rt, err)
	}
	var actions []string
	for _, r := range h.AuditRows(t) {
		actions = append(actions, r.Action)
	}
	if want := []string{"secret.set", "auth.login.begin", "auth.login.finish"}; !slices.Equal(actions, want) {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}

func TestBeginAuthLoginErrors(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, gmailConfig, daemontest.Options{})
	tests := []struct {
		account string
		reason  string
	}{
		{account: "host", reason: api.ReasonInvalidArgument},
		{account: "gm", reason: api.ReasonAuthRequired},
		{account: "nope", reason: api.ReasonNotFound},
	}
	for _, tt := range tests {
		_, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: tt.account}))
		if got := api.Detail(err).GetReason(); got != tt.reason {
			t.Errorf("%s: reason = %s, want %s (err %v)", tt.account, got, tt.reason, err)
		}
	}
}

func TestForbiddenPeer(t *testing.T) {
	t.Parallel()
	other := daemon.NewAuthorizer(uint32(os.Getuid())+1, -1)
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{Authorizer: other})
	_, err := h.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied (err %v)", connect.CodeOf(err), err)
	}
	if got := api.Detail(err).GetReason(); got != api.ReasonForbidden {
		t.Errorf("reason = %s", got)
	}
}

func TestSetSecretRejectsOversizedRequest(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	big := strings.Repeat("x", 1<<20+1)
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: "accounts/host", Value: []byte(big)}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted (err %v)", connect.CodeOf(err), err)
	}
	if names := h.Vault.Names(); len(names) != 0 {
		t.Errorf("vault names = %v, want none", names)
	}
}

func TestAccountServiceIsUnimplemented(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	if _, err := h.Client.Account.ListAccounts(t.Context(), connect.NewRequest(&chainmailv1.ListAccountsRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("ListAccounts: %v", err)
	}
	if _, err := h.Client.Account.TestAccount(t.Context(), connect.NewRequest(&chainmailv1.TestAccountRequest{Account: "host"})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("TestAccount: %v", err)
	}
}
