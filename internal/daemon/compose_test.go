package daemon_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// sendConfig points every account's SMTP at one proxy port, so a test can
// move the accounts between smtptest servers with different faults.
const sendConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
display_name = "Me Myself"
smtp = { host = "127.0.0.1", port = %[1]d, security = "starttls" }

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
smtp = { host = "127.0.0.1", port = %[1]d, security = "starttls" }

[accounts.small]
provider = "hostinger"
address = "small@example.com"
max_message_size = "16KiB"
smtp = { host = "127.0.0.1", port = %[1]d, security = "starttls" }
`

var smtpUsers = []smtptest.User{
	{Username: "me@example.com", Password: "pw"},
	{Username: "me@gmail.example", Password: "pw"},
	{Username: "small@example.com", Password: "pw"},
}

// sendEnv is a daemon with fake mail stores whose accounts submit through
// a proxy to an smtptest server.
type sendEnv struct {
	*daemontest.Harness
	ca                 *tlstest.CA
	proxy              *smtpProxy
	host, gmail, small *fake.Store
}

// startSend starts the daemon. Each opt adjusts its harness options.
func startSend(t *testing.T, opts ...func(*daemontest.Options)) sendEnv {
	t.Helper()
	e := sendEnv{
		ca:    tlstest.New(t),
		proxy: startProxy(t),
		host:  fake.New("host", false),
		gmail: fake.New("gmail", true),
		small: fake.New("small", false),
	}
	hopts := daemontest.Options{
		RootCAs: e.ca.Pool,
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			switch a.Name {
			case "gmail":
				return e.gmail
			case "small":
				return e.small
			default:
				return e.host
			}
		},
		OutcomeSearchInterval: 10 * time.Millisecond,
	}
	for _, o := range opts {
		o(&hopts)
	}
	e.Harness = daemontest.Start(t, fmt.Sprintf(sendConfig, e.proxy.port()), hopts)
	for _, name := range []string{"host", "gmail", "small"} {
		setSecret(t, e.Harness, "accounts/"+name, "pw")
	}
	return e
}

// smtp starts an smtptest server with opts and sends the proxy to it.
func (e sendEnv) smtp(t *testing.T, opts smtptest.Options) *smtptest.Server {
	t.Helper()
	srv := smtptest.StartWith(t, e.ca, provider.SecurityStartTLS, opts, smtpUsers...)
	e.proxy.target.Store(srv.Endpoint().Addr())
	return srv
}

func (e sendEnv) send(t *testing.T, req *chainmailv1.SendRequest) (*chainmailv1.ComposeResult, error) {
	t.Helper()
	resp, err := e.Client.Compose.Send(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetResult(), nil
}

func (e sendEnv) sendRaw(t *testing.T, req *chainmailv1.SendRawRequest) (*chainmailv1.ComposeResult, error) {
	t.Helper()
	resp, err := e.Client.Compose.SendRaw(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetResult(), nil
}

func (e sendEnv) upload(t *testing.T, header *chainmailv1.UploadHeader, chunks ...[]byte) (*chainmailv1.UploadAttachmentResponse, error) {
	t.Helper()
	stream := e.Client.Compose.UploadAttachment(t.Context())
	if header != nil {
		if err := stream.Send(&chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Header{Header: header}}); err != nil {
			t.Fatalf("sending header: %v", err)
		}
	}
	for _, c := range chunks {
		if err := stream.Send(&chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Data{Data: c}}); err != nil {
			break // the server answered early; CloseAndReceive has its answer
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// idemState is the stored state of an idempotency key.
func (e sendEnv) idemState(t *testing.T, account, key string) string {
	t.Helper()
	var state string
	if err := e.DB.QueryRowContext(t.Context(), "SELECT state FROM idempotency WHERE account = ? AND key = ?", account, key).Scan(&state); err != nil {
		t.Fatalf("reading idempotency row: %v", err)
	}
	return state
}

// decisions lists the decision column of every audit row of action.
func (e sendEnv) decisions(t *testing.T, action string) []string {
	t.Helper()
	rows, err := e.DB.QueryContext(t.Context(), "SELECT decision FROM audit WHERE action = ? ORDER BY id", action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any iteration error
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// checkAuditPrivate fails when any audit row holds a recipient or subject.
func (e sendEnv) checkAuditPrivate(t *testing.T, secrets ...string) {
	t.Helper()
	for _, r := range e.AuditRows(t) {
		row := fmt.Sprintf("%+v", r)
		for _, s := range append(secrets, "you@", "cc@", "hidden@", "Quarterly") {
			if strings.Contains(row, s) {
				t.Errorf("audit row %s holds %q", row, s)
			}
		}
	}
}

func markdown(s string) *chainmailv1.ComposeBody {
	return &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Markdown{Markdown: s}}
}

func hostSpec() *chainmailv1.ComposeSpec {
	return &chainmailv1.ComposeSpec{
		Account: "host",
		To:      []string{"You <you@example.net>"},
		Cc:      []string{"cc@example.net"},
		Bcc:     []string{"hidden@example.net"},
		Subject: "Quarterly numbers",
		Content: &chainmailv1.Content{Body: markdown("Hello **there**")},
	}
}

// leaves maps each leaf part's media type to its decoded body.
func leaves(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing message: %v", err)
	}
	out := map[string]string{}
	err = ent.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil {
			return err
		}
		mt, _, _ := part.Header.ContentType()
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		b, err := io.ReadAll(part.Body)
		if err != nil {
			return err
		}
		out[mt] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking message: %v", err)
	}
	return out
}

func header(t *testing.T, raw []byte) *gomail.Header {
	t.Helper()
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing message: %v", err)
	}
	return &gomail.Header{Header: ent.Header}
}

func parseRef(t *testing.T, s string) ref.Ref {
	t.Helper()
	r, err := ref.Parse(s)
	if err != nil {
		t.Fatalf("ref %q: %v", s, err)
	}
	return r
}

func countOps(s *fake.Store, op string) int {
	n := 0
	for _, o := range s.Ops() {
		if o == op {
			n++
		}
	}
	return n
}

func TestSendMarkdown(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	req := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "send-1"}
	res, err := e.send(t, req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := res.GetSent()
	if sent == nil || sent.GetMessageId() == "" || sent.GetDuplicate() || sent.GetSentCopyError() != "" {
		t.Fatalf("result = %+v", res)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.From != "me@example.com" {
		t.Errorf("MAIL FROM = %q", m.From)
	}
	if want := []string{"you@example.net", "cc@example.net", "hidden@example.net"}; !slices.Equal(m.To, want) {
		t.Errorf("RCPT TO = %v, want %v", m.To, want)
	}
	h := header(t, m.Data)
	if subj, _ := h.Subject(); subj != "Quarterly numbers" {
		t.Errorf("Subject = %q", subj)
	}
	if h.Has("Bcc") || bytes.Contains(m.Data, []byte("hidden@")) {
		t.Error("the Bcc recipient is in the message bytes")
	}
	if from := h.Get("From"); !strings.Contains(from, "Me Myself") || !strings.Contains(from, "me@example.com") {
		t.Errorf("From = %q", from)
	}
	if id, _ := h.MessageID(); id != sent.GetMessageId() {
		t.Errorf("Message-ID = %q, result says %q", id, sent.GetMessageId())
	}
	parts := leaves(t, m.Data)
	if !strings.Contains(parts["text/plain"], "Hello **there**") || !strings.Contains(parts["text/html"], "<strong>there</strong>") {
		t.Errorf("parts = %v", parts)
	}

	// The Sent copy went to the Sent folder, marked read.
	r := parseRef(t, sent.GetSentRef())
	if r.Folder != "Sent" {
		t.Errorf("sent_ref folder = %q", r.Folder)
	}
	if !slices.Contains(e.host.Flags(r), `\Seen`) {
		t.Errorf("Sent copy flags = %v", e.host.Flags(r))
	}
	if got := e.host.Appended(); len(got) != 1 || !bytes.Equal(got[0], m.Data) {
		t.Errorf("appended %d messages, want the sent bytes once", len(got))
	}
	if e.idemState(t, "host", "send-1") != "done" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "send-1"))
	}
	last := lastAudit(t, e.Harness)
	if last.Action != "send" || last.Account != "host" || last.Result != "ok" || !strings.Contains(last.Targets, sent.GetMessageId()) {
		t.Errorf("audit = %+v", last)
	}
	e.checkAuditPrivate(t)
}

func TestSendRequiresAValidKey(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	for _, key := range []string{"", "has space", strings.Repeat("k", 129), "semi;colon"} {
		_, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: key})
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("key %q: err = %v, want INVALID_ARGUMENT", key, err)
		}
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
	if last := lastAudit(t, e.Harness); last.Action != "send" || last.Result != "error" || last.Reason != api.ReasonInvalidArgument {
		t.Errorf("audit = %+v", last)
	}
}

func TestSendDryRun(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	res, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), DryRun: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	dry := res.GetDryRun()
	if dry == nil {
		t.Fatalf("result = %+v", res)
	}
	if dry.GetEnvelopeFrom() != "me@example.com" {
		t.Errorf("envelope_from = %q", dry.GetEnvelopeFrom())
	}
	if want := []string{"you@example.net", "cc@example.net", "hidden@example.net"}; !slices.Equal(dry.GetEnvelopeTo(), want) {
		t.Errorf("envelope_to = %v", dry.GetEnvelopeTo())
	}
	if subj, _ := header(t, dry.GetRaw()).Subject(); subj != "Quarterly numbers" {
		t.Errorf("Subject = %q", subj)
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
	if len(e.host.Appended()) != 0 {
		t.Error("a dry run stored a message")
	}
	if d := e.decisions(t, "send"); !slices.Equal(d, []string{"dry_run"}) {
		t.Errorf("decisions = %v", d)
	}
	e.checkAuditPrivate(t)
}

func TestSendDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.gmail.AddFolder("[Gmail]/Drafts", mailstore.RoleDrafts)
	srv := e.smtp(t, smtptest.Options{})
	tests := []struct {
		account string
		store   *fake.Store
	}{
		{"host", e.host},
		{"gmail", e.gmail},
	}
	for _, tt := range tests {
		spec := hostSpec()
		spec.Account = tt.account
		res, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, Draft: true})
		if err != nil {
			t.Fatalf("%s: Send: %v", tt.account, err)
		}
		d := res.GetDraft()
		if d == nil || d.GetMessageId() == "" {
			t.Fatalf("%s: result = %+v", tt.account, res)
		}
		r := parseRef(t, d.GetRef())
		if !slices.Contains(tt.store.Flags(r), `\Draft`) {
			t.Errorf("%s: draft flags = %v", tt.account, tt.store.Flags(r))
		}
		if id, _ := header(t, tt.store.RawMessage(r)).MessageID(); id != d.GetMessageId() {
			t.Errorf("%s: draft Message-ID = %q, want %q", tt.account, id, d.GetMessageId())
		}
		if last := lastAudit(t, e.Harness); last.Action != "draft.create" || last.Account != tt.account || last.Result != "ok" {
			t.Errorf("%s: audit = %+v", tt.account, last)
		}
	}
	// The host draft is in Drafts and the Gmail one carries the Drafts label.
	if got := e.host.Appended(); len(got) != 1 {
		t.Errorf("host appended %d", len(got))
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
	e.checkAuditPrivate(t)
}

func TestSendDuplicateKey(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	req := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "dup"}
	first, err := e.send(t, req)
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	second, err := e.send(t, req)
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if !second.GetSent().GetDuplicate() || second.GetSent().GetMessageId() != first.GetSent().GetMessageId() ||
		second.GetSent().GetSentRef() != first.GetSent().GetSentRef() {
		t.Errorf("second = %+v, first = %+v", second, first)
	}
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("captured %d messages, want 1", n)
	}
	if d := e.decisions(t, "send"); !slices.Equal(d, []string{"", "duplicate"}) {
		t.Errorf("decisions = %v", d)
	}

	// A settled key replays before the request is read, so the same key
	// with a different body gets the first outcome and sends nothing.
	other := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "dup"}
	other.Spec.Content.Body = markdown("something else")
	if res, err := e.send(t, other); err != nil || !res.GetSent().GetDuplicate() {
		t.Errorf("different body, settled key: %+v, %v, want the first result", res, err)
	}
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("captured %d messages, want 1", n)
	}

	// A key that may still send is checked against the request: a
	// different body there is refused and sends nothing.
	e.smtp(t, smtptest.Options{FailMail: &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "later"}})
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "dup-retry"}); reasonOf(err) != api.ReasonServerUnavailable {
		t.Fatalf("temporary failure: err = %v, want SERVER_UNAVAILABLE", err)
	}
	good := e.smtp(t, smtptest.Options{})
	other.IdempotencyKey = "dup-retry"
	if _, err := e.send(t, other); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("different body, retryable key: err = %v, want INVALID_ARGUMENT", err)
	}
	if n := len(good.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
}

func TestSendConcurrentDuplicatesSendOnce(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	req := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "race"}
	const n = 4
	results := make([]*chainmailv1.ComposeResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results[i], errs[i] = e.send(t, req) })
	}
	wg.Wait()
	dups := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Send %d: %v", i, errs[i])
		}
		if results[i].GetSent().GetDuplicate() {
			dups++
		}
	}
	if dups != n-1 || len(srv.Messages()) != 1 {
		t.Errorf("duplicates = %d, captured = %d, want %d and 1", dups, len(srv.Messages()), n-1)
	}
}

func TestSendRecipientRefused(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{FailRcpt: map[string]*smtp.SMTPError{
		"cc@example.net": {Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "secret server text"},
	}})
	req := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "rcpt"}
	_, err := e.send(t, req)
	if reasonOf(err) != api.ReasonInvalidArgument {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
	if !strings.Contains(err.Error(), "cc@example.net") || strings.Contains(err.Error(), "you@example.net") {
		t.Errorf("err = %v, want it to name the refused recipient only", err)
	}
	if strings.Contains(err.Error(), "secret server text") {
		t.Errorf("err = %v carries the server's text", err)
	}
	if e.idemState(t, "host", "rcpt") != "failed" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "rcpt"))
	}
	conns := e.proxy.accepts.Load()
	_, err = e.send(t, req)
	if reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("repeat err = %v, want INVALID_ARGUMENT", err)
	}
	if e.proxy.accepts.Load() != conns || len(srv.Messages()) != 0 {
		t.Error("the repeat connected again")
	}
	if d := e.decisions(t, "send"); !slices.Equal(d, []string{"", "duplicate"}) {
		t.Errorf("decisions = %v", d)
	}
	e.checkAuditPrivate(t)
}

func TestSendTemporaryFailureCanBeRetried(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.smtp(t, smtptest.Options{FailMail: &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "try later"}})
	req := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "temp"}
	_, err := e.send(t, req)
	if reasonOf(err) != api.ReasonServerUnavailable || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v, want SERVER_UNAVAILABLE", err)
	}
	if strings.Contains(err.Error(), "try later") {
		t.Errorf("err = %v carries the server's text", err)
	}
	if e.idemState(t, "host", "temp") != "retryable" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "temp"))
	}
	good := e.smtp(t, smtptest.Options{})
	res, err := e.send(t, req)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if res.GetSent().GetDuplicate() || len(good.Messages()) != 1 {
		t.Errorf("repeat = %+v, captured %d, want a real send", res, len(good.Messages()))
	}
}

func TestSendLostReplyFoundInSent(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	var stored atomic.Pointer[ref.Ref]
	e.smtp(t, smtptest.Options{
		DropAfterDataEnd: true,
		// The server filed the message before the connection was lost.
		OnMessage: func(env smtptest.Envelope) {
			r, err := e.host.Append(context.Background(), "Sent", []string{`\Seen`}, time.Time{}, env.Data)
			if err != nil {
				t.Errorf("filing the message: %v", err)
				return
			}
			stored.Store(&r)
		},
	})
	res, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "lost"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if r := stored.Load(); r == nil || res.GetSent().GetSentRef() != r.String() {
		t.Errorf("sent_ref = %q, want the filed copy %v", res.GetSent().GetSentRef(), r)
	}
	if n := len(e.host.Appended()); n != 1 {
		t.Errorf("appended %d messages, want only the server's own copy", n)
	}
	if e.idemState(t, "host", "lost") != "done" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "lost"))
	}
}

func TestSendLostReplyNotFoundIsUnknown(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{DropAfterDataEnd: true})
	req := &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "unknown"}
	_, err := e.send(t, req)
	if reasonOf(err) != api.ReasonSendOutcomeUnknown {
		t.Fatalf("err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}
	if n := countOps(e.host, "FindByMessageID"); n != 3 {
		t.Errorf("searched Sent %d times, want 3", n)
	}
	if len(e.host.Appended()) != 0 {
		t.Error("an unknown send was copied to Sent")
	}
	if e.idemState(t, "host", "unknown") != "unknown" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "unknown"))
	}
	conns := e.proxy.accepts.Load()
	if _, err := e.send(t, req); reasonOf(err) != api.ReasonSendOutcomeUnknown {
		t.Errorf("repeat err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}
	if e.proxy.accepts.Load() != conns || len(srv.Messages()) != 1 {
		t.Error("the repeat sent again")
	}
}

func TestSendGoogleDoesNotAppend(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	var stored atomic.Pointer[ref.Ref]
	e.smtp(t, smtptest.Options{
		// Gmail files sent mail itself.
		OnMessage: func(env smtptest.Envelope) {
			r, err := e.gmail.Append(context.Background(), "[Gmail]/Sent Mail", []string{`\Seen`}, time.Time{}, env.Data)
			if err != nil {
				t.Errorf("filing the message: %v", err)
				return
			}
			stored.Store(&r)
		},
	})
	spec := hostSpec()
	spec.Account = "gmail"
	res, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "g1"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := len(e.gmail.Appended()); n != 1 {
		t.Errorf("appended %d messages, want only Gmail's own copy", n)
	}
	if r := stored.Load(); r == nil || res.GetSent().GetSentRef() != r.String() {
		t.Errorf("sent_ref = %q, want the All Mail copy %v", res.GetSent().GetSentRef(), r)
	}
	if n := countOps(e.gmail, "FindByMessageID"); n != 1 {
		t.Errorf("searched %d times, want 1", n)
	}
}

func TestSendSentCopyFailure(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	e.host.FailWrite("Append", fmt.Errorf("%w: server said something private", mailstore.ErrUnavailable))
	res, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "copy"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := res.GetSent()
	if sent.GetSentCopyError() == "" || strings.Contains(sent.GetSentCopyError(), "private") || sent.GetSentRef() != "" {
		t.Errorf("result = %+v", sent)
	}
	if len(srv.Messages()) != 1 {
		t.Error("the message was not sent")
	}
}

func TestSendOversizedRefusedBeforeDialing(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	spec := hostSpec()
	spec.Account = "small"
	spec.Content.Body = markdown(strings.Repeat("x", 20<<10))
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "big"}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("Send: err = %v, want INVALID_ARGUMENT", err)
	}
	raw := rawMessage("small@example.com", strings.Repeat("y", 20<<10))
	if _, err := e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "small", Raw: raw, IdempotencyKey: "big-raw"}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("SendRaw: err = %v, want INVALID_ARGUMENT", err)
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
}

func rawMessage(from, body string) []byte {
	return []byte("From: Someone <" + from + ">\r\n" +
		"To: you@example.net\r\n" +
		"Bcc: hidden@example.net\r\n" +
		"Subject: Quarterly raw\r\n" +
		"Message-ID: <raw.1@example.com>\r\n" +
		"Date: Thu, 29 Oct 2026 12:00:00 +0000\r\n" +
		"\r\n" + body + "\r\n")
}

func TestSendRaw(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	res, err := e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "host", Raw: rawMessage("ME@example.com", "hi"), IdempotencyKey: "raw-1"})
	if err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	if res.GetSent().GetMessageId() != "raw.1@example.com" {
		t.Errorf("result = %+v", res)
	}
	m := srv.Messages()[0]
	if want := []string{"you@example.net", "hidden@example.net"}; !slices.Equal(m.To, want) || m.From != "me@example.com" {
		t.Errorf("envelope = %s %v", m.From, m.To)
	}
	if bytes.Contains(m.Data, []byte("Bcc")) || bytes.Contains(m.Data, []byte("hidden@")) {
		t.Errorf("the Bcc header was sent:\n%s", m.Data)
	}
	if !bytes.Contains(m.Data, []byte("Subject: Quarterly raw")) {
		t.Errorf("message = %s", m.Data)
	}
	if got := e.host.Appended(); len(got) != 1 || bytes.Contains(got[0], []byte("hidden@")) {
		t.Errorf("Sent copy = %q", got)
	}
	if last := lastAudit(t, e.Harness); last.Action != "send_raw" || last.Result != "ok" || !strings.Contains(last.Targets, "raw.1@example.com") {
		t.Errorf("audit = %+v", last)
	}

	// envelope_to replaces the header recipients.
	raw2 := bytes.Replace(rawMessage("me@example.com", "two"), []byte("raw.1"), []byte("raw.2"), 1)
	if _, err := e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "host", Raw: raw2, EnvelopeTo: []string{"only@example.net"}, IdempotencyKey: "raw-2"}); err != nil {
		t.Fatalf("SendRaw with envelope_to: %v", err)
	}
	if to := srv.Messages()[1].To; !slices.Equal(to, []string{"only@example.net"}) {
		t.Errorf("RCPT TO = %v", to)
	}
	e.checkAuditPrivate(t, "only@")
}

func TestSendRawRefusals(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	tests := map[string][]byte{
		"From is another address": rawMessage("someone@example.org", "hi"),
		"no Message-ID":           bytes.Replace(rawMessage("me@example.com", "hi"), []byte("Message-ID: <raw.1@example.com>\r\n"), nil, 1),
		"no From":                 bytes.Replace(rawMessage("me@example.com", "hi"), []byte("From: Someone <me@example.com>\r\n"), nil, 1),
		"not a message":           []byte("garbage without a header break"),
	}
	for name, raw := range tests {
		_, err := e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "host", Raw: raw, IdempotencyKey: "r"})
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%s: err = %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if len(srv.Messages()) != 0 {
		t.Error("a refused message was sent")
	}
}

func TestSendRawTwoMiB(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	body := strings.Repeat(strings.Repeat("z", 76)+"\r\n", (2<<20)/78)
	if _, err := e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "host", Raw: rawMessage("me@example.com", body), IdempotencyKey: "two-mib"}); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	if got := srv.Messages(); len(got) != 1 || len(got[0].Data) < 2<<20-1024 {
		t.Errorf("captured %d messages", len(got))
	}
}

func TestUploadAttachment(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	data := bytes.Repeat([]byte("PDF bytes "), 1000)
	sum := sha256.Sum256(data)
	up, err := e.upload(t, &chainmailv1.UploadHeader{Account: "host", Filename: "report.pdf", ContentType: "application/pdf"}, data[:4000], data[4000:])
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	if !strings.HasPrefix(up.GetHandle(), "up_") || up.GetSha256() != hex.EncodeToString(sum[:]) || up.GetSize() != int64(len(data)) {
		t.Errorf("upload = %+v", up)
	}
	if last := lastAudit(t, e.Harness); last.Action != "upload" || last.Account != "host" || last.Result != "ok" || strings.Contains(last.Targets, "report") {
		t.Errorf("audit = %+v", last)
	}

	spec := hostSpec()
	spec.Content.Attachments = []string{up.GetHandle()}
	req := &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "attach"}
	if _, err := e.send(t, req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	parts := leaves(t, srv.Messages()[0].Data)
	if parts["application/pdf"] != string(data) {
		t.Errorf("attachment = %d bytes, want %d", len(parts["application/pdf"]), len(data))
	}

	// The same bytes under a new handle are the same request.
	again, err := e.upload(t, &chainmailv1.UploadHeader{Account: "host", Filename: "report.pdf", ContentType: "application/pdf"}, data)
	if err != nil {
		t.Fatal(err)
	}
	req.Spec.Content.Attachments = []string{again.GetHandle()}
	res, err := e.send(t, req)
	if err != nil || !res.GetSent().GetDuplicate() {
		t.Errorf("same bytes, new handle: %+v, %v, want a duplicate", res, err)
	}

	// A handle is only good for its own account.
	other := hostSpec()
	other.Account = "small"
	other.Content.Attachments = []string{up.GetHandle()}
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: other, DryRun: true}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("another account's handle: err = %v, want INVALID_ARGUMENT", err)
	}
	unknown := hostSpec()
	unknown.Content.Attachments = []string{"up_aaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: unknown, DryRun: true}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("unknown handle: err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestUploadInlineImage(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.smtp(t, smtptest.Options{})
	png := []byte("\x89PNG\r\n\x1a\n fake image")
	up, err := e.upload(t, &chainmailv1.UploadHeader{Account: "host", Filename: "chart.png", ContentType: "image/png"}, png)
	if err != nil {
		t.Fatal(err)
	}
	spec := hostSpec()
	spec.Content.Body = markdown("See ![chart](chart.png)")
	spec.Content.Inline = []*chainmailv1.InlineImage{{Name: "chart.png", Upload: up.GetHandle()}}
	res, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, DryRun: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	parts := leaves(t, res.GetDryRun().GetRaw())
	if parts["image/png"] != string(png) || !strings.Contains(parts["text/html"], "cid:") {
		t.Errorf("parts = %v", slices.Collect(func(yield func(string) bool) {
			for k := range parts {
				if !yield(k) {
					return
				}
			}
		}))
	}
}

func TestUploadRefusals(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	tests := []struct {
		name   string
		header *chainmailv1.UploadHeader
		chunks [][]byte
	}{
		{"too large for the account", &chainmailv1.UploadHeader{Account: "small", Filename: "a.bin"}, [][]byte{make([]byte, 10<<10), make([]byte, 10<<10)}},
		{"no header", nil, [][]byte{[]byte("data first")}},
		{"unknown account", &chainmailv1.UploadHeader{Account: "nobody", Filename: "a.bin"}, [][]byte{[]byte("x")}},
		{"no account", &chainmailv1.UploadHeader{Filename: "a.bin"}, [][]byte{[]byte("x")}},
		{"bad content type", &chainmailv1.UploadHeader{Account: "host", Filename: "a.bin", ContentType: "not a type"}, [][]byte{[]byte("x")}},
		{"no filename", &chainmailv1.UploadHeader{Account: "host"}, [][]byte{[]byte("x")}},
	}
	for _, tt := range tests {
		_, err := e.upload(t, tt.header, tt.chunks...)
		if err == nil {
			t.Errorf("%s: upload succeeded", tt.name)
			continue
		}
		if r := reasonOf(err); r != api.ReasonInvalidArgument && r != api.ReasonNotFound {
			t.Errorf("%s: err = %v, want INVALID_ARGUMENT or NOT_FOUND", tt.name, err)
		}
	}
}

// smtpProxy forwards TCP connections to target and counts them.
type smtpProxy struct {
	ln      net.Listener
	target  atomic.Value // string
	accepts atomic.Int64
}

func startProxy(t *testing.T) *smtpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &smtpProxy{ln: ln}
	p.target.Store("127.0.0.1:1")
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() }) // the accept loop ends on the close error
	return p
}

func (p *smtpProxy) port() int {
	addr, ok := p.ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

func (p *smtpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepts.Add(1)
		go p.pipe(c)
	}
}

// pipe copies both ways until either side closes, then closes both, so a
// server dropping the connection drops the client too.
func (p *smtpProxy) pipe(c net.Conn) {
	defer func() { _ = c.Close() }() // test plumbing; nothing to report
	target, _ := p.target.Load().(string)
	b, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer func() { _ = b.Close() }() // as above
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, b); done <- struct{}{} }()
	<-done
}

func TestSendCredentialsRejected(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := smtptest.StartWith(t, e.ca, provider.SecurityStartTLS, smtptest.Options{}, smtptest.User{Username: "me@example.com", Password: "other"})
	e.proxy.target.Store(srv.Endpoint().Addr())
	_, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "auth"})
	if reasonOf(err) != api.ReasonAuthRejected {
		t.Fatalf("err = %v, want AUTH_REJECTED", err)
	}
	if e.idemState(t, "host", "auth") != "retryable" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "auth"))
	}
	if a := listAccounts(t, e.Harness)["host"]; a.GetAuthState() != chainmailv1.AuthState_AUTH_STATE_REQUIRED {
		t.Errorf("auth state = %v, want the account parked", a.GetAuthState())
	}
	// Parked credentials are not tried again.
	conns := e.proxy.accepts.Load()
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "auth"}); reasonOf(err) != api.ReasonAuthRequired {
		t.Errorf("repeat err = %v, want AUTH_REQUIRED", err)
	}
	if e.proxy.accepts.Load() != conns {
		t.Error("the repeat connected with parked credentials")
	}
}

// TestSendInputBoundCountsEveryField checks that the size bound taken from
// the request before dialing counts the subject and an html_text
// alternative, not only the body source: each alone is over the limit, so
// the finished message certainly is.
func TestSendInputBoundCountsEveryField(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	long := strings.Repeat("z", 17<<10)
	subject := hostSpec()
	subject.Account = "small"
	subject.Subject = long
	htmlText := hostSpec()
	htmlText.Account = "small"
	htmlText.Content.Body = &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Html{Html: "<p>hi</p>"}, HtmlText: long}
	for key, spec := range map[string]*chainmailv1.ComposeSpec{"long-subject": subject, "long-html-text": htmlText} {
		if _, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: key}); reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%s: err = %v, want INVALID_ARGUMENT", key, err)
		}
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
}

// TestSendBuiltSizeCheckedBeforeSubmitting sends a Markdown body under the
// input bound whose text and HTML parts together exceed the limit. The
// exact size, checked after the build, refuses it before anything is
// submitted, and the key stays retryable.
func TestSendBuiltSizeCheckedBeforeSubmitting(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	spec := hostSpec()
	spec.Account = "small"
	spec.Content.Body = markdown(strings.Repeat("word ", (9<<10)/5))
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "expands"}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("Send: err = %v, want INVALID_ARGUMENT", err)
	}
	if len(srv.Messages()) != 0 {
		t.Errorf("messages = %d, want none", len(srv.Messages()))
	}
	if got := e.idemState(t, "small", "expands"); got != "retryable" {
		t.Errorf("idempotency state = %q, want retryable", got)
	}
}

// settableClock is a daemon clock a test moves forward.
type settableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settableClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestSendReplaysAfterUploadExpires repeats keyed sends after their upload
// expired. A settled key replays its outcome without connecting. A new key
// with the expired handle is refused.
func TestSendReplaysAfterUploadExpires(t *testing.T) {
	t.Parallel()
	clock := &settableClock{now: time.Now()}
	e := startSend(t, func(o *daemontest.Options) { o.Now = clock.Now })
	e.smtp(t, smtptest.Options{})
	up, err := e.upload(t, &chainmailv1.UploadHeader{Account: "host", Filename: "a.txt", ContentType: "text/plain"}, []byte("attached"))
	if err != nil {
		t.Fatal(err)
	}
	spec := hostSpec()
	spec.Content.Attachments = []string{up.GetHandle()}
	done := &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "expire-done"}
	first, err := e.send(t, done)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	lost := e.smtp(t, smtptest.Options{DropAfterDataEnd: true})
	unknown := &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "expire-unknown"}
	if _, err := e.send(t, unknown); reasonOf(err) != api.ReasonSendOutcomeUnknown {
		t.Fatalf("lost reply: err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}

	clock.Add(2 * time.Hour)
	conns := e.proxy.accepts.Load()
	res, err := e.send(t, done)
	if err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != first.GetSent().GetMessageId() {
		t.Errorf("done after expiry: %+v, %v, want the first result as a duplicate", res, err)
	}
	if _, err := e.send(t, unknown); reasonOf(err) != api.ReasonSendOutcomeUnknown {
		t.Errorf("unknown after expiry: err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}
	if e.proxy.accepts.Load() != conns || len(lost.Messages()) != 1 {
		t.Error("a repeat after expiry connected")
	}
	if d := e.decisions(t, "send"); len(d) < 2 || d[len(d)-1] != "duplicate" || d[len(d)-2] != "duplicate" {
		t.Errorf("decisions = %v, want the repeats audited as duplicates", d)
	}
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "expire-new"}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("new key, expired handle: err = %v, want INVALID_ARGUMENT", err)
	}
}

// syncBuffer is a bytes.Buffer safe for the daemon's concurrent logging.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestSendKeepsServerTextOutOfLogs makes the SMTP server answer with
// distinctive text at AUTH, MAIL, RCPT and the end of DATA, with debug
// logging on. The text reaches no log line, client error or audit row.
func TestSendKeepsServerTextOutOfLogs(t *testing.T) {
	t.Parallel()
	const private = "PRIVATE-SERVER-TEXT"
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := startSend(t, func(o *daemontest.Options) { o.Logger = logger })
	reply := func(code int) *smtp.SMTPError {
		return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{code / 100, 0, 0}, Message: private + " you@example.net"}
	}
	cases := map[string]smtptest.Options{
		"mail": {FailMail: reply(451)},
		"rcpt": {FailRcpt: map[string]*smtp.SMTPError{"you@example.net": reply(550)}},
		"data": {FailData: reply(554)},
	}
	for key, opts := range cases {
		e.smtp(t, opts)
		if _, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: key}); err == nil || strings.Contains(err.Error(), private) {
			t.Errorf("%s: err = %v, want an error without the server's text", key, err)
		}
	}
	users := slices.Clone(smtpUsers)
	users[0].AuthError = reply(454)
	srv := smtptest.StartWith(t, e.ca, provider.SecurityStartTLS, smtptest.Options{}, users...)
	e.proxy.target.Store(srv.Endpoint().Addr())
	if _, err := e.send(t, &chainmailv1.SendRequest{Spec: hostSpec(), IdempotencyKey: "auth"}); err == nil || strings.Contains(err.Error(), private) {
		t.Errorf("auth: err = %v, want an error without the server's text", err)
	}

	if !strings.Contains(logs.String(), "level=DEBUG") {
		t.Fatal("no debug logs were written; the test proves nothing")
	}
	if strings.Contains(logs.String(), private) {
		t.Errorf("the logs hold the server's text:\n%s", logs.String())
	}
	e.checkAuditPrivate(t, private)
}

// hookStore is a fake store with methods a test can intercept. A nil hook
// leaves the fake's own method. Hooks are set before the daemon uses the
// store and not changed after.
type hookStore struct {
	*fake.Store
	append  func(ctx context.Context, folder string, flags []string, date time.Time, raw []byte) (ref.Ref, error)
	fetch   func(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult
	stream  func(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error)
	expunge func(ctx context.Context, refs []ref.Ref) []mailstore.RefResult
	find    func(ctx context.Context, folder, messageID string) ([]ref.Ref, error)
}

func (h *hookStore) Append(ctx context.Context, folder string, flags []string, date time.Time, raw []byte) (ref.Ref, error) {
	if h.append != nil {
		return h.append(ctx, folder, flags, date, raw)
	}
	return h.Store.Append(ctx, folder, flags, date, raw)
}

func (h *hookStore) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	if h.fetch != nil {
		return h.fetch(ctx, refs)
	}
	return h.Store.FetchMessages(ctx, refs)
}

func (h *hookStore) StreamPart(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
	if h.stream != nil {
		return h.stream(ctx, r, part)
	}
	return h.Store.StreamPart(ctx, r, part)
}

func (h *hookStore) Expunge(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	if h.expunge != nil {
		return h.expunge(ctx, refs)
	}
	return h.Store.Expunge(ctx, refs)
}

func (h *hookStore) FindByMessageID(ctx context.Context, folder, messageID string) ([]ref.Ref, error) {
	if h.find != nil {
		return h.find(ctx, folder, messageID)
	}
	return h.Store.FindByMessageID(ctx, folder, messageID)
}

// hooked serves account name from h, wrapped around the fake startSend made
// for it. The hooks reach that fake as h.Store, the same store as sendEnv's.
func hooked(name string, h *hookStore) func(*daemontest.Options) {
	return func(o *daemontest.Options) {
		prev := o.NewStore
		var once sync.Once
		o.NewStore = func(a config.ResolvedAccount) mailstore.Store {
			if a.Name != name {
				return prev(a)
			}
			once.Do(func() { h.Store = prev(a).(*fake.Store) })
			return h
		}
	}
}

// TestSendReplaysBeforeCheckingInputs repeats settled Send and SendRaw calls
// with content now over the account's limit, as after max_message_size was
// lowered. The stored outcome comes back as a duplicate without a size
// check or a connection.
func TestSendReplaysBeforeCheckingInputs(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	spec := hostSpec()
	spec.Account = "small"
	first, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: "fits"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	firstRaw, err := e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "small", Raw: rawMessage("small@example.com", "short"), IdempotencyKey: "raw-fits"})
	if err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	conns := e.proxy.accepts.Load()

	big := hostSpec()
	big.Account = "small"
	big.Content.Body = markdown(strings.Repeat("x", 20<<10))
	res, err := e.send(t, &chainmailv1.SendRequest{Spec: big, IdempotencyKey: "fits"})
	if err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != first.GetSent().GetMessageId() {
		t.Errorf("Send repeat: %+v, %v, want the first result as a duplicate", res, err)
	}
	raw := rawMessage("small@example.com", strings.Repeat("y", 20<<10))
	res, err = e.sendRaw(t, &chainmailv1.SendRawRequest{Account: "small", Raw: raw, IdempotencyKey: "raw-fits"})
	if err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != firstRaw.GetSent().GetMessageId() {
		t.Errorf("SendRaw repeat: %+v, %v, want the first result as a duplicate", res, err)
	}
	if e.proxy.accepts.Load() != conns || len(srv.Messages()) != 2 {
		t.Error("a repeat connected or sent")
	}
	for _, action := range []string{"send", "send_raw"} {
		if d := e.decisions(t, action); len(d) != 2 || d[1] != "duplicate" {
			t.Errorf("%s decisions = %v, want the repeat audited as a duplicate", action, d)
		}
	}
}

// TestSendSizeBoundIsALowerBound sends messages whose finished form fits the
// account's limit, though a bound that counted text at base64 size or
// counted Bcc would refuse them: a long ASCII text body, and a short message
// with many Bcc recipients, which go only in the envelope.
func TestSendSizeBoundIsALowerBound(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	text := hostSpec()
	text.Account = "small"
	text.Content.Body = &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Text{Text: strings.Repeat("a line of plain text\n", (13<<10)/21)}}
	bcc := hostSpec()
	bcc.Account = "small"
	for i := range 700 {
		bcc.Bcc = append(bcc.Bcc, fmt.Sprintf("rcpt%03d@example.net", i))
	}
	for key, spec := range map[string]*chainmailv1.ComposeSpec{"long-text": text, "many-bcc": bcc} {
		dry, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, DryRun: true})
		if err != nil {
			t.Errorf("%s: dry run: %v", key, err)
			continue
		}
		if n := len(dry.GetDryRun().GetRaw()); n > 16<<10 {
			t.Fatalf("%s: the finished message is %d bytes, over the limit; the test proves nothing", key, n)
		}
		if _, err := e.send(t, &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: key}); err != nil {
			t.Errorf("%s: Send: %v", key, err)
		}
	}
	if n := len(srv.Messages()); n != 2 {
		t.Errorf("captured %d messages, want 2", n)
	}
}
