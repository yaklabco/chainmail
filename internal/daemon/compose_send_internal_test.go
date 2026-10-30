package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/sender"
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
