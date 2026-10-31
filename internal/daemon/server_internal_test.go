package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"filippo.io/age"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/vault"
)

const passwordConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

// newTestServer builds a Server that is never served, so tests can call its
// handlers and helpers directly.
func newTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	return newTestServerConfig(t, passwordConfig)
}

// newTestServerConfig is newTestServer with the given config TOML.
func newTestServerConfig(t *testing.T, configTOML string) (*Server, *sql.DB) {
	t.Helper()
	return newTestServerOptions(t, configTOML, Options{})
}

// newTestServerOptions is newTestServerConfig with opts as the base of the
// Server's Options, such as RootCAs for in-process mail servers. The
// config, vault, state database, listener and authorizer are the test's.
func newTestServerOptions(t *testing.T, configTOML string, opts Options) (*Server, *sql.DB) {
	t.Helper()
	cfg, err := config.Parse([]byte(configTOML))
	if err != nil {
		t.Fatal(err)
	}
	dir := socketDir(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(filepath.Join(dir, "vault.age"), id)
	if err != nil {
		t.Fatal(err)
	}
	stateDB, err := db.OpenState(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() })
	ln, err := Listen(t.Context(), filepath.Join(dir, "d.sock"), -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	opts.Config, opts.Vault, opts.StateDB, opts.Listener = cfg, v, stateDB, ln
	opts.Authorizer = NewAuthorizer(uint32(os.Getuid()), -1)
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, stateDB
}

func peerContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, peerKey{}, Peer{UID: uint32(os.Getuid())})
}

func auditCount(t *testing.T, stateDB *sql.DB) int {
	t.Helper()
	var n int
	if err := stateDB.QueryRowContext(t.Context(), "SELECT count(*) FROM audit").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setSecretDirect(ctx context.Context, s *Server, name string, value []byte) error {
	a := &adminService{s: s}
	_, err := a.SetSecret(ctx, connect.NewRequest(&chainmailv1.SetSecretRequest{Name: name, Value: value}))
	return err
}

func requireAuthState(t *testing.T, s *Server, want bool) {
	t.Helper()
	st, err := s.auth.State("host")
	if err != nil {
		t.Fatal(err)
	}
	if st.Required != want {
		t.Errorf("auth required = %v, want %v (%s)", st.Required, want, st.Detail)
	}
}

func TestSetSecretFailureKeepsRejectedCredentialsParked(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	ctx := peerContext(t.Context())
	if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("old")); err != nil {
		t.Fatal(err)
	}
	creds, err := s.auth.Credentials(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	s.auth.Rejected("host", creds)
	requireAuthState(t, s, true)

	err = setSecretDirect(ctx, s, auth.PasswordSecret("host"), nil)
	if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
		t.Fatalf("empty value: reason = %s (err %v)", got, err)
	}
	requireAuthState(t, s, true)

	if err := setSecretDirect(ctx, s, auth.PasswordSecret("host"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	requireAuthState(t, s, false)
}

func TestRecordSurvivesCancelledRequest(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.record(ctx, caller{uid: 1, client: "test"}, audit.Entry{Action: "secret.set"}, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	if n := auditCount(t, stateDB); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

func TestRecordFailsRequestWhenAuditWriteFails(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	if err := stateDB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, actionErr := range []error{nil, vault.ErrInvalidName} {
		err := s.record(t.Context(), caller{client: "test"}, audit.Entry{Action: "secret.set"}, actionErr)
		if connect.CodeOf(err) != connect.CodeInternal || api.Detail(err).GetReason() != api.ReasonInternal {
			t.Errorf("action error %v: record = %v, want INTERNAL", actionErr, err)
		}
	}
}

func TestCallerOf(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set(api.HeaderClient, "cli")
	if _, err := callerOf(context.Background(), h); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("no peer: err = %v, want PermissionDenied", err)
	}
	c, err := callerOf(peerContext(context.Background()), h)
	if err != nil {
		t.Fatal(err)
	}
	if c.uid != uint32(os.Getuid()) || c.client != "cli" {
		t.Errorf("caller = %+v", c)
	}
	c, err = callerOf(peerContext(context.Background()), http.Header{})
	if err != nil || c.client != "unknown" {
		t.Errorf("caller = %+v, %v", c, err)
	}
	// The kind is self-reported and lands in the audit log, so anything
	// that is not a short plain name is recorded as a placeholder.
	for _, kind := range []string{strings.Repeat("x", 100<<10), "two words", "UPPER"} {
		h.Set(api.HeaderClient, kind)
		c, err = callerOf(peerContext(context.Background()), h)
		if err != nil || c.client != invalidTarget {
			t.Errorf("kind %.20q: caller = %.60v, %v; want %s", kind, c, err, invalidTarget)
		}
	}
	h.Set(api.HeaderClient, "live-test")
	if c, err = callerOf(peerContext(context.Background()), h); err != nil || c.client != "live-test" {
		t.Errorf("caller = %+v, %v", c, err)
	}
}

func TestHandlersRefuseMissingPeer(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	err := setSecretDirect(t.Context(), s, auth.PasswordSecret("host"), []byte("pw"))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("SetSecret without peer: err = %v", err)
	}
	if names := s.opts.Vault.Names(); len(names) != 0 {
		t.Errorf("vault names = %v, want none", names)
	}
	if n := auditCount(t, stateDB); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

func TestToConnectError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err    error
		code   connect.Code
		reason string
	}{
		{err: &auth.RequiredError{Account: "a", Hint: "h"}, code: connect.CodeUnauthenticated, reason: api.ReasonAuthRequired},
		{err: auth.ErrRejected, code: connect.CodeUnauthenticated, reason: api.ReasonAuthRejected},
		{err: config.ErrUnknownAccount, code: connect.CodeNotFound, reason: api.ReasonNotFound},
		{err: vault.ErrEmptyValue, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: context.DeadlineExceeded, code: connect.CodeDeadlineExceeded, reason: api.ReasonTimeout},
		{err: context.Canceled, code: connect.CodeCanceled, reason: api.ReasonTimeout},
		{err: fmt.Errorf("refreshing: %w", auth.ErrTokenEndpoint), code: connect.CodeUnavailable, reason: api.ReasonServerUnavailable},
		{err: fmt.Errorf("%w: %w", auth.ErrTokenEndpoint, context.DeadlineExceeded), code: connect.CodeDeadlineExceeded, reason: api.ReasonTimeout},
		{err: &query.SyntaxError{Pos: 3, Msg: "bad"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: has:attachment", mailstore.ErrUnsupportedQuery), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: x", ref.ErrInvalid), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: x", ref.ErrInvalidCursor), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: &argError{msg: "limit", hint: "h"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: labels", mailstore.ErrUnsupported), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: INBOX", mailstore.ErrRefused), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: Projects", mailstore.ErrExists), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{err: fmt.Errorf("%w: gone", mailstore.ErrStaleRef), code: connect.CodeNotFound, reason: api.ReasonStaleRef},
		{err: fmt.Errorf("%w: part 9", mailstore.ErrNotFound), code: connect.CodeNotFound, reason: api.ReasonNotFound},
		{err: fmt.Errorf("%w: down", mailstore.ErrUnavailable), code: connect.CodeUnavailable, reason: api.ReasonServerUnavailable},
		{err: fmt.Errorf("%w: %w", mailstore.ErrUnavailable, context.DeadlineExceeded), code: connect.CodeDeadlineExceeded, reason: api.ReasonTimeout},
		{err: errors.New("boom"), code: connect.CodeInternal, reason: api.ReasonInternal},
	}
	for _, tt := range tests {
		got := toConnectError(tt.err)
		if got.Code() != tt.code || api.Detail(got).GetReason() != tt.reason {
			t.Errorf("toConnectError(%v) = %v %s, want %v %s", tt.err, got.Code(), api.Detail(got).GetReason(), tt.code, tt.reason)
		}
	}
}

func TestToConnectErrorWriteHints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		hint string
	}{
		{err: fmt.Errorf("%w: labels", mailstore.ErrUnsupported), hint: "this account does not support that action"},
		{err: fmt.Errorf("%w: INBOX", mailstore.ErrRefused), hint: "that folder cannot be changed that way"},
		{err: fmt.Errorf("%w: Projects", mailstore.ErrExists), hint: "a folder with that name exists"},
	}
	for _, tt := range tests {
		if got := api.Detail(toConnectError(tt.err)).GetHint(); got != tt.hint {
			t.Errorf("toConnectError(%v) hint = %q, want %q", tt.err, got, tt.hint)
		}
	}
}

// TestToConnectErrorWriteTextIsFixed checks that a write error's own text,
// which can carry the mail server's words, never reaches the client, while
// the cause stays on the error for the daemon's logs.
func TestToConnectErrorWriteTextIsFixed(t *testing.T) {
	t.Parallel()
	const secret = "SERVER-SAID-zq81-do-not-leak"
	tests := []struct {
		sentinel error
		msg      string
	}{
		{sentinel: mailstore.ErrUnsupported, msg: "the account does not support that action"},
		{sentinel: mailstore.ErrRefused, msg: "the mail server refused the change"},
		{sentinel: mailstore.ErrExists, msg: "the folder already exists"},
	}
	for _, tt := range tests {
		err := fmt.Errorf("%w: %s", tt.sentinel, secret)
		got := toConnectError(err)
		if got.Message() != tt.msg {
			t.Errorf("toConnectError(%v).Message() = %q, want %q", tt.sentinel, got.Message(), tt.msg)
		}
		if strings.Contains(got.Error(), secret) {
			t.Errorf("toConnectError(%v).Error() = %q, leaks the wrapped text", tt.sentinel, got.Error())
		}
		d := api.Detail(got)
		if strings.Contains(d.GetHint(), secret) || strings.Contains(d.GetReason(), secret) {
			t.Errorf("toConnectError(%v) detail = %v, leaks the wrapped text", tt.sentinel, d)
		}
		if !errors.Is(got, tt.sentinel) || !errors.Is(got, err) {
			t.Errorf("toConnectError(%v) dropped its cause", tt.sentinel)
		}
	}
}

func TestServerBuildsPolicyFromConfig(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, passwordConfig+"\n[policy]\nallow_expunge = true\n")
	if !s.policy.AllowExpunge() {
		t.Error("s.policy.AllowExpunge() = false, want true from allow_expunge = true")
	}
	if s.policy.AllowFolderDelete() {
		t.Error("s.policy.AllowFolderDelete() = true, want the default false")
	}
}

func TestNewFailsOnInvalidPolicy(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	opts := s.opts
	cfg := *opts.Config
	cfg.Policy.SendAllow = []string{"*@*"}
	opts.Config = &cfg
	if _, err := New(opts); err == nil {
		t.Error("New: want an error for an invalid send_allow entry")
	}
}
