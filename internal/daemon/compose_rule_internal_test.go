package daemon

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// ruleForwardConfig is one standard account that submits to an smtptest
// server on the given port and may send to example.net.
const ruleForwardConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }

[policy]
send_allow = ["*@example.net"]
`

// ruleForwardServer is a daemon that is never served, over a fake store
// for host, whose SMTP is the returned capture server.
func ruleForwardServer(t *testing.T) (*Server, *fake.Store, *smtptest.Server) {
	t.Helper()
	ca := tlstest.New(t)
	srv := smtptest.StartWith(t, ca, provider.SecurityStartTLS, smtptest.Options{}, smtptest.User{Username: "me@example.com", Password: "pw"})
	host := fake.New("host", false)
	s, _ := newTestServerOptions(t, fmt.Sprintf(ruleForwardConfig, srv.Endpoint().Port), Options{
		RootCAs:  ca.Pool,
		NewStore: func(config.ResolvedAccount) mailstore.Store { return host },
	})
	if err := setSecretDirect(peerContext(t.Context()), s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
		t.Fatal(err)
	}
	return s, host, srv
}

// TestForwardAsAuditsTheRuleTargets forwards one message as the rule
// caller, with no socket peer in the context, to an allowed recipient,
// which is sent, and to a blocked one, which is held as a draft. A repeat
// with the same idempotency key returns the stored result without sending
// or saving again. Both rows carry client rule, the daemon's uid, and the
// rule's targets after the ref, and no recipient address.
func TestForwardAsAuditsTheRuleTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		to       string
		held     bool
		decision string
		reason   string
	}{
		{name: "allowed", to: "ops@example.net", decision: decisionAllowed},
		{name: "held", to: "you@blocked.invalid", held: true, decision: decisionHeld, reason: api.ReasonHeld},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, host, srv := ruleForwardServer(t)
			orig := host.Add(fake.Message{
				Folder:    "INBOX",
				Date:      time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
				From:      []mailstore.Address{{Name: "Billing", Email: "billing@example.org"}},
				To:        []mailstore.Address{{Email: "me@example.com"}},
				Subject:   "Invoice 42",
				Body:      "Total due\r\n",
				MessageID: "<inv42@example.org>",
			})
			req := &chainmailv1.ForwardRequest{
				Ref:            orig.String(),
				To:             []string{tt.to},
				Content:        &chainmailv1.Content{Body: &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Text{Text: "Filed by a rule"}}},
				IdempotencyKey: "rule.ru_x.0123456789abcdef",
			}
			res, err := s.compose.forwardAs(t.Context(), ruleCaller(), req, testRuleTargets)
			if err != nil {
				t.Fatalf("forwardAs: %v", err)
			}
			if got := res.GetHeld() != nil; got != tt.held || (!tt.held && res.GetSent() == nil) {
				t.Fatalf("result = %+v, want held %v", res, tt.held)
			}
			wantSent := 1
			if tt.held {
				wantSent = 0
			}
			if n := len(srv.Messages()); n != wantSent {
				t.Fatalf("SMTP got %d messages, want %d", n, wantSent)
			}
			appended := len(host.Appended())

			again, err := s.compose.forwardAs(t.Context(), ruleCaller(), req, testRuleTargets)
			if err != nil {
				t.Fatalf("repeat: %v", err)
			}
			if tt.held && again.GetHeld() == nil || !tt.held && !again.GetSent().GetDuplicate() {
				t.Errorf("repeat = %+v, want the stored result", again)
			}
			if n := len(srv.Messages()); n != wantSent {
				t.Errorf("SMTP got %d messages after the repeat, want %d", n, wantSent)
			}
			if n := len(host.Appended()); n != appended {
				t.Errorf("appends = %d after the repeat, want %d", n, appended)
			}

			rows := ruleAuditRows(t, s, "forward")
			if len(rows) != 2 {
				t.Fatalf("audit rows = %+v, want two", rows)
			}
			wantHead := slices.Concat([]string{orig.String()}, testRuleTargets)
			for i, decision := range []string{tt.decision, decisionDuplicate} {
				row := rows[i]
				if row.Client != ruleClient || row.CallerUID != uint32(os.Getuid()) || row.Account != "host" ||
					row.Decision != decision || row.Result != audit.ResultOK {
					t.Errorf("row %d = %+v, want client rule, uid %d, host, decision %s, ok", i, row, os.Getuid(), decision)
				}
				if len(row.Targets) < len(wantHead) || !slices.Equal(row.Targets[:len(wantHead)], wantHead) {
					t.Errorf("row %d targets = %q, want them to start %q", i, row.Targets, wantHead)
				}
				if strings.Contains(strings.Join(row.Targets, " "), tt.to) {
					t.Errorf("row %d targets = %q, hold a recipient", i, row.Targets)
				}
			}
			if rows[0].Reason != tt.reason {
				t.Errorf("first row reason = %q, want %q", rows[0].Reason, tt.reason)
			}
		})
	}
}
