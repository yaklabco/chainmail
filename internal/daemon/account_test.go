package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/emersion/go-smtp"
	"golang.org/x/oauth2"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/oauthtest"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

type mailServers struct {
	ca   *tlstest.CA
	imap provider.Endpoint
	smtp provider.Endpoint
}

func startMail(t *testing.T, username, password, token string) mailServers {
	t.Helper()
	ca := tlstest.New(t)
	return mailServers{
		ca:   ca,
		imap: imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: username, Password: password, Token: token}),
		smtp: smtptest.Start(t, ca, provider.SecurityStartTLS, smtptest.User{Username: username, Password: password, Token: token}),
	}
}

func localConfig(m mailServers) string {
	return fmt.Sprintf(`
[accounts.local]
provider = "custom"
address = "me@example.test"
imap = { host = "127.0.0.1", port = %d, security = "tls" }
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }
`, m.imap.Port, m.smtp.Port)
}

func testAccount(t *testing.T, h *daemontest.Harness, account string) (*chainmailv1.TestAccountResponse, error) {
	t.Helper()
	resp, err := h.Client.Account.TestAccount(t.Context(), connect.NewRequest(&chainmailv1.TestAccountRequest{Account: account}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func listAccounts(t *testing.T, h *daemontest.Harness) map[string]*chainmailv1.Account {
	t.Helper()
	resp, err := h.Client.Account.ListAccounts(t.Context(), connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	out := map[string]*chainmailv1.Account{}
	for _, a := range resp.Msg.GetAccounts() {
		out[a.GetName()] = a
	}
	return out
}

func TestListAccountsReportsAuthState(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	a := listAccounts(t, h)["host"]
	if a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_REQUIRED || !strings.Contains(a.GetAuthDetail(), "secret set accounts/host") {
		t.Errorf("before secret: %+v", a)
	}
	if a.GetProvider() != "hostinger" || a.GetAddress() != "me@example.com" || a.GetAuthMethod() != "password" {
		t.Errorf("account = %+v", a)
	}
	setSecret(t, h, "accounts/host", "pw")
	if a := listAccounts(t, h)["host"]; a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_OK || a.GetAuthDetail() != "" {
		t.Errorf("after secret: %+v", a)
	}
	if last := lastAudit(t, h); last.Action != "accounts.list" || last.Result != "ok" {
		t.Errorf("audit = %+v", last)
	}
}

func TestTestAccountPassword(t *testing.T) {
	t.Parallel()
	m := startMail(t, "me@example.test", "pw", "")
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "pw")
	resp, err := testAccount(t, h, "local")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !resp.GetImap().GetOk() || !resp.GetSmtp().GetOk() {
		t.Fatalf("checks = imap %+v smtp %+v", resp.GetImap(), resp.GetSmtp())
	}
	if !slices.Contains(resp.GetImap().GetCapabilities(), "IMAP4rev1") {
		t.Errorf("capabilities = %v", resp.GetImap().GetCapabilities())
	}
	if resp.GetAccount() != "local" {
		t.Errorf("account = %q", resp.GetAccount())
	}
	if last := lastAudit(t, h); last.Action != "account.test" || last.Account != "local" || last.Result != "ok" {
		t.Errorf("audit = %+v", last)
	}
}

func TestTestAccountWrongPassword(t *testing.T) {
	t.Parallel()
	m := startMail(t, "me@example.test", "pw", "")
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "wrong-hunter2")
	resp, err := testAccount(t, h, "local")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	for name, c := range map[string]*chainmailv1.Check{"imap": resp.GetImap(), "smtp": resp.GetSmtp()} {
		if c.GetOk() || c.GetReason() != api.ReasonAuthRejected {
			t.Errorf("%s = %+v, want AUTH_REJECTED", name, c)
		}
		if c.GetError() == "" || strings.Contains(c.GetError(), "hunter2") {
			t.Errorf("%s error = %q, want a message without the secret", name, c.GetError())
		}
	}
	if a := listAccounts(t, h)["local"]; a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_REQUIRED || !strings.Contains(a.GetAuthDetail(), "rejected") {
		t.Errorf("account after rejection = %+v", a)
	}
	rows := h.AuditRows(t)
	var test daemontest.AuditRow
	for _, r := range rows {
		if strings.Contains(fmt.Sprint(r), "hunter2") {
			t.Errorf("audit row %+v contains the secret", r)
		}
		if r.Action == "account.test" {
			test = r
		}
	}
	if test.Result != "error" || test.Reason != api.ReasonAuthRejected {
		t.Errorf("audit = %+v", test)
	}
	if _, err := testAccount(t, h, "local"); api.Detail(err).GetReason() != api.ReasonAuthRequired {
		t.Errorf("second test err = %v, want AUTH_REQUIRED until the secret changes", err)
	}
}

// Concurrent tests of an account with a bad password must not each log in:
// providers lock accounts out after a few failures.
func TestConcurrentTestsLogInOnce(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	var logins atomic.Int32
	m := mailServers{
		ca: ca,
		imap: imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
			Username: "me@example.test", Password: "pw", Logins: &logins,
		}),
		smtp: smtptest.Start(t, ca, provider.SecurityStartTLS, smtptest.User{Username: "me@example.test", Password: "pw"}),
	}
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "wrong")
	const callers = 4
	reasons := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			resp, err := testAccount(t, h, "local")
			switch {
			case err != nil:
				reasons[i] = api.Detail(err).GetReason()
			default:
				reasons[i] = resp.GetImap().GetReason()
			}
		})
	}
	wg.Wait()
	if n := logins.Load(); n != 1 {
		t.Errorf("IMAP login attempts = %d, want 1", n)
	}
	slices.Sort(reasons)
	want := []string{api.ReasonAuthRejected, api.ReasonAuthRequired, api.ReasonAuthRequired, api.ReasonAuthRequired}
	if !slices.Equal(reasons, want) {
		t.Errorf("reasons = %v, want %v", reasons, want)
	}
}

// Server trouble is not a verdict on the credentials, so the account must
// stay usable: a later test still dials instead of failing AUTH_REQUIRED.
func TestTestAccountServerTroubleIsNotRejection(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	m := mailServers{
		ca: ca,
		// A plain error makes the IMAP server answer NO [SERVERBUG].
		imap: imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
			Username: "me@example.test", Password: "pw", LoginError: errors.New("backend down"),
		}),
		smtp: smtptest.Start(t, ca, provider.SecurityStartTLS, smtptest.User{
			Username: "me@example.test", Password: "pw",
			AuthError: &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "temporary authentication failure"},
		}),
	}
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "pw")
	for attempt := range 2 {
		resp, err := testAccount(t, h, "local")
		if err != nil {
			t.Fatalf("attempt %d: TestAccount: %v", attempt, err)
		}
		for name, c := range map[string]*chainmailv1.Check{"imap": resp.GetImap(), "smtp": resp.GetSmtp()} {
			if c.GetOk() || c.GetReason() != api.ReasonServerUnavailable || c.GetError() == "" {
				t.Errorf("attempt %d: %s = %+v, want SERVER_UNAVAILABLE", attempt, name, c)
			}
		}
	}
	if a := listAccounts(t, h)["local"]; a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_OK {
		t.Errorf("account after server trouble = %+v, want AUTH_STATE_OK", a)
	}
	rows := h.AuditRows(t)
	var tests []daemontest.AuditRow
	for _, r := range rows {
		if r.Action == "account.test" {
			tests = append(tests, r)
		}
	}
	if len(tests) != 2 || tests[0].Result != "error" || tests[0].Reason != api.ReasonServerUnavailable || tests[1] != tests[0] {
		t.Errorf("account.test audit rows = %+v", tests)
	}
}

func TestTestAccountErrors(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	_, err := testAccount(t, h, "host")
	if d := api.Detail(err); d.GetReason() != api.ReasonAuthRequired || !strings.Contains(d.GetHint(), "secret set accounts/host") {
		t.Errorf("missing secret: detail = %+v", d)
	}
	if _, err := testAccount(t, h, "nope"); api.Detail(err).GetReason() != api.ReasonNotFound {
		t.Errorf("unknown account err = %v", err)
	}
	rows := h.AuditRows(t)
	if len(rows) != 2 || rows[0].Reason != api.ReasonAuthRequired || rows[1].Reason != api.ReasonNotFound {
		t.Errorf("audit rows = %+v", rows)
	}
}

// startOAuth runs mail servers that accept token for me@gmail.com, and a
// daemon whose gm account has finished an OAuth2 login against oauthtest.
func startOAuth(t *testing.T, token string) *daemontest.Harness {
	t.Helper()
	m := startMail(t, "me@gmail.com", "unused", token)
	o := oauthtest.Start(t)
	cfg := fmt.Sprintf(`
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"
imap = { host = "127.0.0.1", port = %d, security = "tls" }
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }
`, m.imap.Port, m.smtp.Port)
	h := daemontest.Start(t, cfg, daemontest.Options{
		RootCAs:     m.ca.Pool,
		AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())},
	})
	setSecret(t, h, "oauth_clients/g", oauthtest.ClientSecret)
	begin, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: "gm"}))
	if err != nil {
		t.Fatal(err)
	}
	redirect := oauthtest.RedirectFor(t, begin.Msg.GetConsentUrl(), oauthtest.Code)
	if _, err := h.Client.Admin.FinishAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: "gm", RedirectUrl: redirect})); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestTestAccountOAuth(t *testing.T) {
	t.Parallel()
	h := startOAuth(t, oauthtest.AccessToken)
	resp, err := testAccount(t, h, "gm")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !resp.GetImap().GetOk() || !resp.GetSmtp().GetOk() {
		t.Errorf("checks = imap %+v smtp %+v", resp.GetImap(), resp.GetSmtp())
	}
}

// A token endpoint outage is a server problem, not a verdict on the secrets.
func TestTestAccountTokenEndpointOutage(t *testing.T) {
	t.Parallel()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(tokens.Close)
	m := startMail(t, "me@gmail.com", "unused", oauthtest.AccessToken)
	cfg := fmt.Sprintf(`
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"
imap = { host = "127.0.0.1", port = %d, security = "tls" }
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }
`, m.imap.Port, m.smtp.Port)
	h := daemontest.Start(t, cfg, daemontest.Options{
		RootCAs:     m.ca.Pool,
		AuthOptions: []auth.Option{auth.WithOAuthEndpoint(oauth2.Endpoint{TokenURL: tokens.URL, AuthStyle: oauth2.AuthStyleInParams})},
	})
	for name, value := range map[string]string{"oauth_clients/g": oauthtest.ClientSecret, "tokens/gm": oauthtest.RefreshToken} {
		if err := h.Vault.Set(name, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := testAccount(t, h, "gm")
	if got := api.Detail(err).GetReason(); got != api.ReasonServerUnavailable || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("err = %v (reason %s), want unavailable SERVER_UNAVAILABLE", err, got)
	}
	if a := listAccounts(t, h)["gm"]; a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_OK {
		t.Errorf("account after outage = %+v, want AUTH_STATE_OK", a)
	}
	var test daemontest.AuditRow
	for _, r := range h.AuditRows(t) {
		if r.Action == "account.test" {
			test = r
		}
	}
	if test.Result != "error" || test.Reason != api.ReasonServerUnavailable {
		t.Errorf("audit = %+v", test)
	}
}

func TestTestAccountOAuthRejected(t *testing.T) {
	t.Parallel()
	h := startOAuth(t, "some-other-token")
	resp, err := testAccount(t, h, "gm")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if c := resp.GetImap(); c.GetOk() || c.GetReason() != api.ReasonAuthRejected {
		t.Errorf("imap = %+v, want AUTH_REJECTED", c)
	}
	if strings.Contains(resp.GetImap().GetError(), oauthtest.AccessToken) {
		t.Errorf("imap error %q contains the access token", resp.GetImap().GetError())
	}
	if c := resp.GetSmtp(); c.GetOk() || c.GetReason() != api.ReasonAuthRejected || !strings.Contains(c.GetError(), "not attempted") {
		t.Errorf("smtp = %+v, want AUTH_REJECTED, not attempted", c)
	}
	a := listAccounts(t, h)["gm"]
	if a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_REQUIRED || !strings.Contains(a.GetAuthDetail(), "chainmail auth login gm") {
		t.Errorf("account after rejection = %+v, want the auth login hint", a)
	}
}

// A rejected login is not retried, because providers such as Hostinger count
// IMAP and SMTP failures together toward a lockout. The SMTP endpoint here
// has nothing listening, so a real probe would report SERVER_UNAVAILABLE.
func TestTestAccountSkipsSMTPAfterIMAPRejection(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	m := mailServers{
		ca:   ca,
		imap: imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "me@example.test", Password: "pw"}),
		smtp: provider.Endpoint{Host: "127.0.0.1", Port: closedPort(t), Security: provider.SecurityStartTLS},
	}
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "wrong")
	resp, err := testAccount(t, h, "local")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if c := resp.GetImap(); c.GetReason() != api.ReasonAuthRejected {
		t.Errorf("imap = %+v, want AUTH_REJECTED", c)
	}
	if c := resp.GetSmtp(); c.GetOk() || c.GetReason() != api.ReasonAuthRejected || !strings.Contains(c.GetError(), "not attempted") {
		t.Errorf("smtp = %+v, want AUTH_REJECTED without an attempt", c)
	}
}

// The client giving up while IMAP is stuck is reported as TIMEOUT, and the
// audit row is still written.
func TestTestAccountTimeout(t *testing.T) {
	t.Parallel()
	silent := silentEndpoint(t)
	m := mailServers{ca: tlstest.New(t), imap: silent, smtp: silent}
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "pw")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := h.Client.Account.TestAccount(ctx, connect.NewRequest(&chainmailv1.TestAccountRequest{Account: "local"})); err == nil {
		t.Fatal("TestAccount succeeded against a silent server")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if last := lastAudit(t, h); last.Action == "account.test" {
			if last.Result != "error" || last.Reason != api.ReasonTimeout {
				t.Errorf("audit = %+v, want TIMEOUT", last)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no account.test audit row after the client gave up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closedPort returns a local port that nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr.Port
}

// silentEndpoint accepts connections and never sends a byte, until the test ends.
func silentEndpoint(t *testing.T) provider.Endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
			wg.Go(func() {
				<-t.Context().Done()
				_ = conn.Close() // the test is over; the client may already have hung up
			})
		}
	})
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityTLS}
}

// When the checks fail differently, the audit row keeps the rejection, since
// that is the failure that parked the account.
func TestTestAccountAuditPrefersRejection(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	m := mailServers{
		ca: ca,
		imap: imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
			Username: "me@example.test", Password: "pw", LoginError: errors.New("backend down"),
		}),
		smtp: smtptest.Start(t, ca, provider.SecurityStartTLS, smtptest.User{Username: "me@example.test", Password: "pw"}),
	}
	h := daemontest.Start(t, localConfig(m), daemontest.Options{RootCAs: m.ca.Pool})
	setSecret(t, h, "accounts/local", "wrong")
	resp, err := testAccount(t, h, "local")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if resp.GetImap().GetReason() != api.ReasonServerUnavailable || resp.GetSmtp().GetReason() != api.ReasonAuthRejected {
		t.Fatalf("checks = imap %+v smtp %+v", resp.GetImap(), resp.GetSmtp())
	}
	if last := lastAudit(t, h); last.Action != "account.test" || last.Result != "error" || last.Reason != api.ReasonAuthRejected {
		t.Errorf("audit = %+v, want AUTH_REJECTED", last)
	}
}

func lastAudit(t *testing.T, h *daemontest.Harness) daemontest.AuditRow {
	t.Helper()
	rows := h.AuditRows(t)
	if len(rows) == 0 {
		t.Fatal("no audit rows")
	}
	return rows[len(rows)-1]
}
