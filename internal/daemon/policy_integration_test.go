//go:build integration

package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore/imapstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/dovecot"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
)

// policyDoveConfig is one account on Dovecot's IMAP and an smtptest sink.
// Only *@allowed.test, and the account's own address, may be sent to.
const policyDoveConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = %[3]d, security = "starttls" }

[policy]
send_allow = ["*@allowed.test"]
`

// passwordCreds hands out one fixed password login.
type passwordCreds struct{ user string }

func (c passwordCreds) Credentials(context.Context, string) (auth.Credentials, error) {
	return auth.Credentials{Method: provider.AuthPassword, Username: c.user, Secret: dovecot.Password}, nil
}

func (passwordCreds) Rejected(string, auth.Credentials) {}

// TestPolicyHeldSendLandsInDovecotDrafts sends past the policy gate against
// a real IMAP server. A send with a blocked Bcc is held: nothing reaches
// SMTP, and Dovecot's Drafts folder gets the message with its Bcc, \Draft
// and the same Message-ID. A send to allowed recipients only goes out.
func TestPolicyHeldSendLandsInDovecotDrafts(t *testing.T) {
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	const user = "policy@example.test"
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: user, Password: dovecot.Password})
	cfgTOML := fmt.Sprintf(policyDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port)
	h := daemontest.Start(t, cfgTOML, daemontest.Options{RootCAs: dove.CA.Pool})
	setSecret(t, h, "accounts/dove", dovecot.Password)
	env := sendEnv{Harness: h}

	// 1. The Bcc is off the allowlist, so the send is held.
	spec := &chainmailv1.ComposeSpec{
		Account: "dove",
		To:      []string{"a@allowed.test"},
		Bcc:     []string{"b@blocked.test"},
		Subject: "Held for review",
		Content: &chainmailv1.Content{Body: markdown("Please check this one.")},
	}
	res, err := env.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "held"})
	if err != nil {
		t.Fatalf("held Send: %v", err)
	}
	held := res.GetHeld()
	if held == nil {
		t.Fatalf("result = %+v, want held", res)
	}
	if !slices.Equal(held.GetBlocked(), []string{"b@blocked.test"}) || held.GetTooManyRecipients() || held.GetHint() != heldHintText {
		t.Errorf("held = %+v, want b@blocked.test blocked with the fixed hint", held)
	}
	if n, froms := len(sink.Messages()), sink.MailFroms(); n != 0 || len(froms) != 0 {
		t.Fatalf("SMTP sink got %d messages and MAIL FROM %v, want nothing", n, froms)
	}
	env.checkHeldAudit(t, "send", held, "blocked:1")

	// 2. The draft is in Dovecot's Drafts, read back through the IMAP store.
	draftRef := parseRef(t, held.GetDraft().GetRef())
	if draftRef.Folder != "Drafts" {
		t.Errorf("draft ref folder = %q, want Drafts", draftRef.Folder)
	}
	cfg, err := config.Parse([]byte(cfgTOML))
	if err != nil {
		t.Fatal(err)
	}
	acct, err := cfg.Account("dove")
	if err != nil {
		t.Fatal(err)
	}
	st := imapstore.New(acct, passwordCreds{user: user}, imapstore.Options{RootCAs: dove.CA.Pool})
	t.Cleanup(func() { _ = st.Close() })
	found, err := st.FindByMessageID(t.Context(), "Drafts", held.GetDraft().GetMessageId())
	if err != nil {
		t.Fatalf("FindByMessageID: %v", err)
	}
	if !slices.Equal(found, []ref.Ref{draftRef}) {
		t.Fatalf("Drafts messages with the draft's Message-ID = %v, want only %v", found, draftRef)
	}
	got := st.FetchMessages(t.Context(), []ref.Ref{draftRef})
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("FetchMessages = %+v", got)
	}
	msg := got[0].Message
	if !slices.Contains(msg.Summary.Flags, `\Draft`) {
		t.Errorf("draft flags = %v, want \\Draft", msg.Summary.Flags)
	}
	hdr := header(t, msg.Raw)
	if id, _ := hdr.MessageID(); id != held.GetDraft().GetMessageId() {
		t.Errorf("draft Message-ID = %q, result says %q", id, held.GetDraft().GetMessageId())
	}
	if bcc, err := hdr.AddressList("Bcc"); err != nil || len(bcc) != 1 || bcc[0].Address != "b@blocked.test" {
		t.Errorf("draft Bcc = %v, %v, want b@blocked.test", bcc, err)
	}
	if to, err := hdr.AddressList("To"); err != nil || len(to) != 1 || to[0].Address != "a@allowed.test" {
		t.Errorf("draft To = %v, %v, want a@allowed.test", to, err)
	}
	if subj, _ := hdr.Subject(); subj != "Held for review" {
		t.Errorf("draft Subject = %q", subj)
	}

	// 3. With only allowed recipients, the send goes out.
	spec.Bcc = nil
	res, err = env.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "allowed"})
	if err != nil {
		t.Fatalf("allowed Send: %v", err)
	}
	if res.GetSent() == nil {
		t.Fatalf("result = %+v, want sent", res)
	}
	msgs := sink.Messages()
	if len(msgs) != 1 {
		t.Fatalf("SMTP sink got %d messages, want 1", len(msgs))
	}
	if msgs[0].From != user || !slices.Equal(msgs[0].To, []string{"a@allowed.test"}) {
		t.Errorf("envelope = %s to %v, want %s to [a@allowed.test]", msgs[0].From, msgs[0].To, user)
	}
	if !bytes.Contains(msgs[0].Data, []byte("Subject: Held for review")) {
		t.Errorf("sent message lacks its subject:\n%s", msgs[0].Data)
	}
	if d := env.decisions(t, "send"); !slices.Equal(d, []string{"held", "allowed"}) {
		t.Errorf("send decisions = %v, want [held allowed]", d)
	}
}
