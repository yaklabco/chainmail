package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/idempotency"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/sender"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// TestRunDialsWhileBuilding proves the SMTP dial and the build overlap:
// each waits for the other to have started, which only works when they
// run at the same time.
func TestRunDialsWhileBuilding(t *testing.T) {
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
	s, _ := newTestServer(t)
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
