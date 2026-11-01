package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gomail "github.com/emersion/go-message/mail"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/idempotency"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/mailstore/imapstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/sender"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
	"github.com/jamesainslie/chainmail/internal/upload"
)

// TestRunDialsWhileBuilding proves the SMTP dial and the build overlap:
// each waits for the other to have started, which only works when they
// run at the same time.
func TestRunDialsWhileBuilding(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, allowConfig)
	ctx := peerContext(t.Context())
	if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
		t.Fatal(err)
	}
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	dialing, building := make(chan struct{}), make(chan struct{})
	// meet announces mine and waits for theirs.
	meet := func(mine, theirs chan struct{}) bool {
		close(mine)
		select {
		case <-theirs:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	var dialSaw, buildSaw bool
	c := newComposeService(s)
	c.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
		dialSaw = meet(dialing, building)
		return nil, errors.New("the test dial stops here")
	}
	job := sendJob{
		acct:   acct,
		key:    "overlap",
		hash:   sha256.Sum256([]byte("overlap")),
		action: "send",
		caller: caller{uid: 1, client: "test"},
		build: func(context.Context) (compose.Message, error) {
			buildSaw = meet(building, dialing)
			return compose.Message{Raw: []byte("x"), MessageID: "a@example.com", EnvelopeFrom: acct.Address, EnvelopeTo: []string{"you@example.net"}}, nil
		},
	}
	_, err = c.run(ctx, job)
	if got := api.Detail(err).GetReason(); got != api.ReasonServerUnavailable {
		t.Errorf("err = %v, want SERVER_UNAVAILABLE", err)
	}
	if !dialSaw || !buildSaw {
		t.Errorf("dial saw the build: %v, build saw the dial: %v; want both", dialSaw, buildSaw)
	}
}

// TestRunBuildFailureStopsTheDial checks that a failed build ends the call
// with the build's error and does not wait for the dial to finish.
func TestRunBuildFailureStopsTheDial(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	ctx := peerContext(t.Context())
	if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
		t.Fatal(err)
	}
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	c := newComposeService(s)
	c.dial = func(ctx context.Context, _ provider.Endpoint, _ auth.Credentials, _ sender.DialOptions) (*sender.Conn, error) {
		<-ctx.Done() // a dial that only ends when told to
		return nil, ctx.Err()
	}
	job := sendJob{
		acct:   acct,
		key:    "build-fails",
		hash:   sha256.Sum256([]byte("build-fails")),
		action: "send",
		caller: caller{uid: 1, client: "test"},
		build: func(context.Context) (compose.Message, error) {
			return compose.Message{}, errors.Join(compose.ErrInvalid, errors.New("bad header"))
		},
	}
	start := time.Now()
	_, err = c.run(ctx, job)
	if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
		t.Errorf("err = %v, want INVALID_ARGUMENT", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("run waited for the dial")
	}
}

// TestRunLeftoverKeys checks a key another process left in progress: while
// it is in progress it is refused without sending, and once Recover settles
// it at startup it reads as of unknown outcome.
func TestRunLeftoverKeys(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	ctx := peerContext(t.Context())
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("leftover"))
	if _, err := stateDB.ExecContext(ctx,
		`INSERT INTO idempotency (account, key, request_hash, state, result, created_at, updated_at)
		 VALUES ('host', 'left', ?, 'in_progress', x'', '2026-10-30T00:00:00.000000000Z', '2026-10-30T00:00:00.000000000Z')`, hash[:]); err != nil {
		t.Fatal(err)
	}
	c := newComposeService(s)
	c.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
		t.Error("a leftover key dialed")
		return nil, errors.New("no dial")
	}
	job := sendJob{
		acct: acct, key: "left", hash: hash, action: "send", caller: caller{uid: 1, client: "test"},
		build: func(context.Context) (compose.Message, error) {
			t.Error("a leftover key built a message")
			return compose.Message{}, errors.New("no build")
		},
	}
	_, err = c.run(ctx, job)
	if got := api.Detail(err).GetReason(); got != api.ReasonServerUnavailable || !strings.Contains(err.Error(), "still in progress") {
		t.Errorf("in progress: err = %v, want SERVER_UNAVAILABLE, still in progress", err)
	}
	if _, err := s.idem.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = c.run(ctx, job)
	if got := api.Detail(err).GetReason(); got != api.ReasonSendOutcomeUnknown {
		t.Errorf("recovered: err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}
}

// TestRunCancelledWhileBuildingDoesNotSend cancels the call after the dial
// succeeded and while the build still runs. Once the build returns, the
// message must not be submitted: the connection is closed unused and the
// key settles as retryable.
func TestRunCancelledWhileBuildingDoesNotSend(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, allowConfig)
	ca := tlstest.New(t)
	srv := smtptest.StartWith(t, ca, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: "me@example.com", Password: "pw"})
	s.opts.RootCAs = ca.Pool
	if err := setSecretDirect(peerContext(t.Context()), s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
		t.Fatal(err)
	}
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	acct.SMTP = srv.Endpoint()

	ctx, cancel := context.WithCancel(peerContext(t.Context()))
	defer cancel()
	dialed, release := make(chan struct{}), make(chan struct{})
	var conn *sender.Conn
	var built atomic.Bool
	c := newComposeService(s)
	c.dial = func(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts sender.DialOptions) (*sender.Conn, error) {
		cn, err := sender.Dial(ctx, ep, creds, opts)
		conn = cn
		close(dialed)
		return cn, err
	}
	job := sendJob{
		acct: acct, key: "cancelled", hash: sha256.Sum256([]byte("cancelled")), action: "send",
		caller: caller{uid: 1, client: "test"},
		build: func(context.Context) (compose.Message, error) {
			<-release
			built.Store(true)
			return compose.Message{
				Raw:       []byte("Subject: hi\r\nMessage-ID: <c@example.com>\r\n\r\nhi\r\n"),
				MessageID: "c@example.com", EnvelopeFrom: acct.Address, EnvelopeTo: []string{"you@example.net"},
			}, nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.run(ctx, job)
		done <- err
	}()
	select {
	case <-dialed:
	case <-time.After(10 * time.Second):
		t.Fatal("the dial did not finish")
	}
	cancel()
	close(release)
	if err := <-done; err == nil {
		t.Error("run succeeded after its context was cancelled")
	}
	if !built.Load() {
		t.Error("run returned before the build did")
	}
	if n := len(srv.Messages()); n != 0 {
		t.Fatalf("the server got %d messages after cancellation, want none", n)
	}
	if conn == nil {
		t.Fatal("no connection was dialed")
	}
	// A connection run did not close would submit this one.
	if err := conn.Send(t.Context(), acct.Address, []string{"you@example.net"}, []byte("Subject: probe\r\n\r\nx\r\n")); err == nil || len(srv.Messages()) != 0 {
		t.Errorf("the dialed connection was left open: send err = %v, messages = %d", err, len(srv.Messages()))
	}
	row, ok, err := s.idem.Lookup(t.Context(), "host", "cancelled")
	if err != nil || !ok || row.State != idempotency.Retryable {
		t.Errorf("key = %+v, %v, %v, want retryable", row, ok, err)
	}
}

// TestSendReplaysAfterRestart repeats keyed sends whose uploads a restart
// lost: a fresh service has no upload owners and the upload store is empty.
// A key with a settled outcome replays it without dialing. A key that may
// still send needs its uploads.
func TestSendReplaysAfterRestart(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	ctx := peerContext(t.Context())
	sent, err := proto.Marshal(&chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Sent{Sent: &chainmailv1.Sent{MessageId: "first@example.com", SentRef: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	refused, err := proto.Marshal(&chainmailv1.ErrorDetail{Reason: api.ReasonInvalidArgument, Hint: "stored hint"})
	if err != nil {
		t.Fatal(err)
	}
	other := sha256.Sum256([]byte("whatever the first request hashed to"))
	for key, settle := range map[string]struct {
		state  idempotency.State
		result []byte
	}{
		"done":      {idempotency.Done, sent},
		"failed":    {idempotency.Failed, refused},
		"retryable": {idempotency.Retryable, nil},
	} {
		if _, err := s.idem.Begin(ctx, "host", key, other); err != nil {
			t.Fatal(err)
		}
		if err := s.idem.Finish(ctx, "host", key, settle.state, settle.result); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stateDB.ExecContext(ctx,
		`INSERT INTO idempotency (account, key, request_hash, state, result, created_at, updated_at)
		 VALUES ('host', 'unknown', ?, 'in_progress', x'', '2026-10-30T00:00:00.000000000Z', '2026-10-30T00:00:00.000000000Z')`, other[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.idem.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	c := newComposeService(s)
	c.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
		t.Error("a repeated send dialed")
		return nil, errors.New("no dial")
	}
	req := func(key string) *chainmailv1.SendRequest {
		return &chainmailv1.SendRequest{IdempotencyKey: key, Spec: &chainmailv1.ComposeSpec{
			Account: "host", To: []string{"you@example.net"}, Subject: "s",
			Content: &chainmailv1.Content{Attachments: []string{"up_aaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		}}
	}
	cl := caller{uid: 1, client: "test"}

	res, err := c.send(ctx, cl, req("done"))
	if err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != "first@example.com" {
		t.Errorf("done: %+v, %v, want the stored result as a duplicate", res, err)
	}
	_, err = c.send(ctx, cl, req("failed"))
	if d := api.Detail(err); d.GetReason() != api.ReasonInvalidArgument || d.GetHint() != "stored hint" {
		t.Errorf("failed: err = %v, want the stored refusal", err)
	}
	if _, err = c.send(ctx, cl, req("unknown")); api.Detail(err).GetReason() != api.ReasonSendOutcomeUnknown {
		t.Errorf("unknown: err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}
	for _, key := range []string{"retryable", "absent"} {
		_, err := c.send(ctx, cl, req(key))
		if d := api.Detail(err); d.GetReason() != api.ReasonInvalidArgument || !strings.Contains(d.GetHint(), "upload the file again") {
			t.Errorf("%s: err = %v, want the upload refused", key, err)
		}
	}
	var dups int
	if err := stateDB.QueryRowContext(ctx, "SELECT count(*) FROM audit WHERE decision = 'duplicate'").Scan(&dups); err != nil {
		t.Fatal(err)
	}
	if dups != 3 {
		t.Errorf("duplicate audit rows = %d, want 3", dups)
	}
}

// TestServeStoppedBeforeRecoveryIsCleanShutdown stops the daemon before the
// startup recovery of idempotency keys ran. That is a shutdown, not a
// failure.
func TestServeStoppedBeforeRecoveryIsCleanShutdown(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Serve(ctx); err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
}

// Recipient order is not part of what a send is: the CLI derives one key
// for any order, so the request hash must not change with it either.
func TestRequestHashIgnoresRecipientOrder(t *testing.T) {
	t.Parallel()
	spec := func(to, cc, bcc []string) *chainmailv1.SendRequest {
		return &chainmailv1.SendRequest{IdempotencyKey: "k", Spec: &chainmailv1.ComposeSpec{
			Account: "acct", To: to, Cc: cc, Bcc: bcc, Subject: "s",
		}}
	}
	a, err := requestHash(spec([]string{"a@x", "b@x"}, []string{"c@x", "d@x"}, []string{"e@x", "f@x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := requestHash(spec([]string{"b@x", "a@x"}, []string{"d@x", "c@x"}, []string{"f@x", "e@x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("reordered recipients changed the request hash")
	}
	moved, err := requestHash(spec([]string{"a@x"}, []string{"b@x", "c@x", "d@x"}, []string{"e@x", "f@x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if moved == a {
		t.Error("moving a recipient from To to Cc kept the request hash")
	}
	raw := func(env []string) *chainmailv1.SendRawRequest {
		return &chainmailv1.SendRawRequest{Account: "acct", Raw: []byte("x"), EnvelopeTo: env}
	}
	r1, _ := requestHash(raw([]string{"a@x", "b@x"}), nil)
	r2, _ := requestHash(raw([]string{"b@x", "a@x"}), nil)
	if r1 != r2 {
		t.Error("reordered envelope recipients changed the request hash")
	}
	req := spec([]string{"b@x", "a@x"}, nil, nil)
	if _, err := requestHash(req, nil); err != nil || req.GetSpec().GetTo()[0] != "b@x" {
		t.Error("requestHash reordered the caller's request")
	}
}

// TestSendReplaysWithoutReadingUploads repeats a settled send whose upload
// still lives but can no longer be read. The stored outcome is replayed
// before any upload is opened, so the read failure does not hide it.
func TestSendReplaysWithoutReadingUploads(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	ctx := peerContext(t.Context())
	dir := t.TempDir()
	var err error
	if s.uploads, err = upload.New(dir, uploadTTL, time.Now); err != nil {
		t.Fatal(err)
	}
	c := newComposeService(s)
	c.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
		t.Error("a repeated send dialed")
		return nil, errors.New("no dial")
	}
	info, err := s.uploads.Put(ctx, "a.txt", "text/plain", strings.NewReader("attached"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c.own(info.Handle, "host")
	// A directory in the file's place: the handle is live, its read fails.
	path := filepath.Join(dir, info.Handle)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.uploads.Open(info.Handle); err == nil || errors.Is(err, upload.ErrUnknownHandle) {
		t.Fatalf("Open = %v, want a read failure; the test proves nothing", err)
	}

	sent, err := proto.Marshal(&chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Sent{Sent: &chainmailv1.Sent{MessageId: "first@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.idem.Begin(ctx, "host", "done", sha256.Sum256([]byte("first"))); err != nil {
		t.Fatal(err)
	}
	if err := s.idem.Finish(ctx, "host", "done", idempotency.Done, sent); err != nil {
		t.Fatal(err)
	}
	req := &chainmailv1.SendRequest{IdempotencyKey: "done", Spec: &chainmailv1.ComposeSpec{
		Account: "host", To: []string{"you@example.net"}, Subject: "s",
		Content: &chainmailv1.Content{Attachments: []string{info.Handle}},
	}}
	res, err := c.send(ctx, caller{uid: 1, client: "test"}, req)
	if err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != "first@example.com" {
		t.Errorf("repeat = %+v, %v, want the stored result as a duplicate", res, err)
	}
	var dups int
	if err := stateDB.QueryRowContext(ctx, "SELECT count(*) FROM audit WHERE decision = 'duplicate'").Scan(&dups); err != nil {
		t.Fatal(err)
	}
	if dups != 1 {
		t.Errorf("duplicate audit rows = %d, want 1", dups)
	}
}

// allowConfig is passwordConfig with every example.net address allowed.
const allowConfig = passwordConfig + `
[policy]
send_allow = ["*@example.net"]
`

// TestPrepareReturnsAHoldAsAResult pins prepare's shape: a held build comes
// back as its decision with the built message, no connection and no error,
// even when the dial failed. An allowed build gets the dial's error.
func TestPrepareReturnsAHoldAsAResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		to          []string
		wantBlocked []string // nil means not held
	}{
		{name: "held", to: []string{"you@blocked.invalid", "ok@example.net"}, wantBlocked: []string{"you@blocked.invalid"}},
		{name: "allowed", to: []string{"ok@example.net"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerConfig(t, allowConfig)
			ctx := peerContext(t.Context())
			if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
				t.Fatal(err)
			}
			acct, err := s.opts.Config.Account("host")
			if err != nil {
				t.Fatal(err)
			}
			c := newComposeService(s)
			c.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
				return nil, errors.New("the mail server is down")
			}
			built := compose.Message{Raw: []byte("From: me@example.com\r\nSubject: hi\r\n\r\nhi\r\n"), MessageID: "p@example.com", EnvelopeFrom: acct.Address, EnvelopeTo: tt.to}
			job := sendJob{acct: acct, build: func(context.Context) (compose.Message, error) { return built, nil }}
			msg, held, conn, err := c.prepare(ctx, job)
			if conn != nil {
				t.Errorf("prepare returned a connection: %v", conn)
			}
			if tt.wantBlocked == nil {
				if held != nil || api.Detail(err).GetReason() != api.ReasonServerUnavailable {
					t.Fatalf("prepare = held %+v, err %v, want the dial's SERVER_UNAVAILABLE", held, err)
				}
				return
			}
			if err != nil || held == nil || held.Allowed || !slices.Equal(held.Blocked, tt.wantBlocked) {
				t.Fatalf("prepare = held %+v, err %v, want held with %v blocked and no error", held, err, tt.wantBlocked)
			}
			if msg.MessageID != built.MessageID {
				t.Errorf("held message %q, want the built %q", msg.MessageID, built.MessageID)
			}
		})
	}
}

// TestHeldBuildCancelsTheDial holds a send while its dial still runs. The
// hold cancels the dial instead of waiting for it, and the draft is the
// built message with the envelope recipients missing from To and Cc as its
// Bcc, or the built message as it is when there are none.
func TestHeldBuildCancelsTheDial(t *testing.T) {
	t.Parallel()
	const raw = "From: me@example.com\r\nTo: you@blocked.invalid\r\nCc: Ann <ann@blocked.invalid>\r\n" +
		"Subject: hi\r\nMessage-ID: <h@example.com>\r\n\r\nhi\r\n"
	withBcc, err := editHeader([]byte(raw), func(h *gomail.Header) {
		h.SetAddressList("Bcc", []*mail.Address{{Address: "extra@blocked.invalid"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		envelope []string
		want     []byte
	}{
		{"recipients beyond the header", []string{"you@blocked.invalid", "ANN@blocked.invalid", "extra@blocked.invalid"}, withBcc},
		{"only the header's recipients", []string{"you@blocked.invalid", "ann@blocked.invalid"}, []byte(raw)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServer(t)
			st := fake.New("host", false)
			s.newStore = func(config.ResolvedAccount, storeParams) mailstore.Store { return st }
			ctx := peerContext(t.Context())
			if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
				t.Fatal(err)
			}
			acct, err := s.opts.Config.Account("host")
			if err != nil {
				t.Fatal(err)
			}
			dialing := make(chan struct{})
			dialErr := make(chan error, 1)
			c := newComposeService(s)
			c.dial = func(ctx context.Context, _ provider.Endpoint, _ auth.Credentials, _ sender.DialOptions) (*sender.Conn, error) {
				close(dialing)
				select {
				case <-ctx.Done():
					dialErr <- ctx.Err()
				case <-time.After(time.Minute):
					dialErr <- errors.New("the dial was never cancelled")
				}
				return nil, errors.New("the test dial stops here")
			}
			job := sendJob{
				acct: acct, key: "held", hash: sha256.Sum256([]byte("held")), action: "send",
				caller: caller{uid: 1, client: "test"},
				build: func(context.Context) (compose.Message, error) {
					<-dialing
					return compose.Message{Raw: []byte(raw), MessageID: "h@example.com", EnvelopeFrom: acct.Address, EnvelopeTo: tt.envelope}, nil
				},
			}
			start := time.Now()
			res, err := c.run(ctx, job)
			if err != nil || res.GetHeld() == nil {
				t.Fatalf("run = %+v, %v, want held", res, err)
			}
			if time.Since(start) > 10*time.Second {
				t.Error("the hold waited for the dial")
			}
			if err := <-dialErr; !errors.Is(err, context.Canceled) {
				t.Errorf("dial ctx: %v, want cancelled", err)
			}
			if got := st.Appended(); len(got) != 1 || !bytes.Equal(got[0], tt.want) {
				t.Errorf("draft = %q, want %q", got, tt.want)
			}
			if row, ok, err := s.idem.Lookup(t.Context(), "host", "held"); err != nil || !ok || row.State != idempotency.Done {
				t.Errorf("key = %+v, %v, %v, want done", row, ok, err)
			}
		})
	}
}

// TestDialFailureDuringASlowBuild fails the SMTP dial at once while the
// build still runs. The dial failure does not cancel the build, and the
// call ends with what the build decided: a hold wins over the dial error,
// so a blocked message still reaches Drafts while SMTP is down; a failed
// build reports its own error; an allowed message reports the dial error.
// Each case runs in a synctest bubble, so the build can wait until the
// dial's goroutine has finished, with nothing left to cancel it, instead
// of sleeping in case a cancellation is still on its way.
func TestDialFailureDuringASlowBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		to         string
		buildErr   error
		wantReason string // empty means held
		wantState  idempotency.State
		wantDrafts int
	}{
		{name: "a held build wins", to: "you@blocked.invalid", wantState: idempotency.Done, wantDrafts: 1},
		{name: "an allowed build gets the dial error", to: "you@example.net", wantReason: api.ReasonServerUnavailable, wantState: idempotency.Retryable},
		{
			name: "a failed build gets its own error", to: "you@blocked.invalid",
			buildErr:   errors.Join(compose.ErrInvalid, errors.New("bad header")),
			wantReason: api.ReasonInvalidArgument, wantState: idempotency.Retryable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s, _ := newTestServerConfig(t, allowConfig)
				st := fake.New("host", false)
				s.newStore = func(config.ResolvedAccount, storeParams) mailstore.Store { return st }
				ctx := peerContext(t.Context())
				if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
					t.Fatal(err)
				}
				acct, err := s.opts.Config.Account("host")
				if err != nil {
					t.Fatal(err)
				}
				// dialReturning closes as the failing dial returns its error.
				dialReturning := make(chan struct{})
				c := newComposeService(s)
				c.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
					defer close(dialReturning)
					return nil, errors.New("the mail server is down")
				}
				raw := "From: me@example.com\r\nTo: " + tt.to + "\r\nSubject: hi\r\nMessage-ID: <slow@example.com>\r\n\r\nhi\r\n"
				job := sendJob{
					acct: acct, key: "slow", hash: sha256.Sum256([]byte("slow")), action: "send",
					caller: caller{uid: 1, client: "test"},
					// The build reads, as Reply and Forward read IMAP, with
					// ctx: it fails if the dial failure cancelled it.
					build: func(ctx context.Context) (compose.Message, error) {
						<-dialReturning
						// Every other goroutine in the bubble is now blocked:
						// the dial's has handled the error and ended, and run
						// waits for this build. Any cancellation the dial
						// failure caused has happened.
						synctest.Wait()
						if err := ctx.Err(); err != nil {
							return compose.Message{}, err
						}
						if tt.buildErr != nil {
							return compose.Message{}, tt.buildErr
						}
						return compose.Message{Raw: []byte(raw), MessageID: "slow@example.com", EnvelopeFrom: acct.Address, EnvelopeTo: []string{tt.to}}, nil
					},
				}
				res, err := c.run(ctx, job)
				if tt.wantReason == "" {
					if err != nil || res.GetHeld() == nil || !slices.Equal(res.GetHeld().GetBlocked(), []string{tt.to}) {
						t.Fatalf("run = %+v, %v, want held with %s blocked", res, err, tt.to)
					}
				} else if got := api.Detail(err).GetReason(); got != tt.wantReason {
					t.Fatalf("run = %+v, %v, want %s", res, err, tt.wantReason)
				}
				if got := st.Appended(); len(got) != tt.wantDrafts || (tt.wantDrafts == 1 && !bytes.Equal(got[0], []byte(raw))) {
					t.Errorf("appended %q, want %d drafts", got, tt.wantDrafts)
				}
				if row, ok, err := s.idem.Lookup(t.Context(), "host", "slow"); err != nil || !ok || row.State != tt.wantState {
					t.Errorf("key = %+v, %v, %v, want %v", row, ok, err, tt.wantState)
				}
			})
		})
	}
}

// fixedCredentials issues the same password credentials every time and
// ignores refusals, so a test can read the IMAP server with a store of its
// own whatever the daemon's auth manager has parked.
type fixedCredentials auth.Credentials

func (f fixedCredentials) Credentials(context.Context, string) (auth.Credentials, error) {
	return auth.Credentials(f), nil
}

func (fixedCredentials) Rejected(string, auth.Credentials) {}

// TestSMTPAuthRejectionAndHolds runs a send whose SMTP server refuses the
// account's credentials while its IMAP server takes them, through the real
// dial, auth manager and IMAP store. The build waits until the dial has
// been refused. A held build still saves its draft, through a new IMAP
// connection, and leaves the credentials unparked: the hold wins whatever
// the dial did. An allowed build gets AUTH_REJECTED with its hint, and the
// account is parked as before.
func TestSMTPAuthRejectionAndHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		to         string
		wantHeld   bool
		wantState  idempotency.State
		wantParked bool
		wantDrafts int
	}{
		{name: "a held build saves its draft", to: "you@blocked.invalid", wantHeld: true, wantState: idempotency.Done, wantDrafts: 1},
		{name: "an allowed build parks the account", to: "you@example.net", wantState: idempotency.Retryable, wantParked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const user, password = "me@example.test", "pw"
			ca := tlstest.New(t)
			imapEP := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: user, Password: password, Folders: []string{"Drafts"}})
			smtpSrv := smtptest.StartWith(t, ca, provider.SecurityStartTLS, smtptest.Options{}, smtptest.User{Username: user, Password: "not-" + password})
			cfg := fmt.Sprintf(`
[accounts.local]
provider = "custom"
address = "me@example.test"
imap = { host = "127.0.0.1", port = %d, security = "tls" }
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }

[policy]
send_allow = ["*@example.net"]
`, imapEP.Port, smtpSrv.Endpoint().Port)
			s, _ := newTestServerOptions(t, cfg, Options{RootCAs: ca.Pool})
			// Stop the background probe the store's first use starts, then log
			// out of the IMAP server and stop the pool's idle timers, as the
			// daemon does at shutdown.
			t.Cleanup(s.drain)
			ctx := peerContext(t.Context())
			if err := setSecretDirect(ctx, s, auth.PasswordSecret("local"), []byte(password)); err != nil {
				t.Fatal(err)
			}
			acct, err := s.opts.Config.Account("local")
			if err != nil {
				t.Fatal(err)
			}
			dialed := make(chan struct{})
			var dialErr error
			c := newComposeService(s)
			c.dial = func(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts sender.DialOptions) (*sender.Conn, error) {
				defer close(dialed)
				conn, err := sender.Dial(ctx, ep, creds, opts)
				dialErr = err
				return conn, err
			}
			const msgID = "refused@example.test"
			raw := "From: me@example.test\r\nTo: " + tt.to + "\r\nSubject: hi\r\nMessage-ID: <" + msgID + ">\r\n\r\nhi\r\n"
			job := sendJob{
				acct: acct, key: "refused", hash: sha256.Sum256([]byte("refused")), action: "send",
				caller: caller{uid: 1, client: "test"},
				build: func(ctx context.Context) (compose.Message, error) {
					select {
					case <-dialed:
					case <-ctx.Done():
						return compose.Message{}, ctx.Err()
					}
					return compose.Message{Raw: []byte(raw), MessageID: msgID, EnvelopeFrom: acct.Address, EnvelopeTo: []string{tt.to}}, nil
				},
			}
			res, err := c.run(ctx, job)
			if !errors.Is(dialErr, auth.ErrRejected) {
				t.Fatalf("dial err = %v, want the SMTP server to refuse the credentials", dialErr)
			}
			if tt.wantHeld {
				if err != nil || res.GetHeld() == nil {
					t.Errorf("run = %+v, %v, want held", res, err)
				}
			} else {
				d := api.Detail(err)
				wantHint := "the server rejected the password; run: chainmail secret set " + auth.PasswordSecret("local")
				if d.GetReason() != api.ReasonAuthRejected || d.GetHint() != wantHint {
					t.Errorf("run = %+v, %v, detail %+v, want AUTH_REJECTED with hint %q", res, err, d, wantHint)
				}
			}
			if row, ok, err := s.idem.Lookup(t.Context(), "local", "refused"); err != nil || !ok || row.State != tt.wantState {
				t.Errorf("key = %+v, %v, %v, want %v", row, ok, err, tt.wantState)
			}
			if st, err := s.auth.State("local"); err != nil || st.Required != tt.wantParked {
				t.Errorf("auth state = %+v, %v, want parked %v", st, err, tt.wantParked)
			}
			if got := smtpSrv.Messages(); len(got) != 0 {
				t.Errorf("SMTP got %d messages, want none", len(got))
			}
			// A store of the test's own, which the IMAP server lets in with
			// the same credentials, counts the drafts.
			check := imapstore.New(acct, fixedCredentials{Method: provider.AuthPassword, Username: user, Secret: password}, imapstore.Options{RootCAs: ca.Pool})
			t.Cleanup(func() { _ = check.Close() }) // nothing to do about a close error at test end
			refs, err := findIn(t.Context(), check, mailstore.RoleDrafts, msgID)
			if err != nil || len(refs) != tt.wantDrafts {
				t.Errorf("drafts = %v, %v, want %d", refs, err, tt.wantDrafts)
			}
		})
	}
}
