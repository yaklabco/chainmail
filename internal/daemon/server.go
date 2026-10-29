package daemon

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/vault"
)

// Options configures a Server. Config, Vault, StateDB, Listener and Authorizer are required.
type Options struct {
	Config     *config.Config
	Vault      *vault.Vault
	StateDB    *sql.DB
	Listener   net.Listener
	Authorizer *Authorizer
	DevMode    bool
	// RootCAs replaces the system roots for IMAP and SMTP connections. Tests use it.
	RootCAs *x509.CertPool
	// AuthOptions are passed to auth.NewManager. Tests use them to point OAuth2 at a fake server.
	AuthOptions []auth.Option
	// Logger defaults to discarding everything.
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

const (
	// maxRequestBytes caps a request message. Secrets and redirect URLs are far smaller.
	maxRequestBytes = 1 << 20
	// auditTimeout bounds an audit write that outlives its request.
	auditTimeout = 5 * time.Second
)

// Server is the chainmail daemon.
type Server struct {
	opts    Options
	auth    *auth.Manager
	audit   *audit.Writer
	started time.Time
	httpSrv *http.Server
	// accountLocks serializes account tests, one account at a time.
	accountLocks keyedLock
}

// New wires up a Server. Call Serve to run it.
func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.Vault == nil || opts.StateDB == nil || opts.Listener == nil || opts.Authorizer == nil {
		return nil, errors.New("daemon: Config, Vault, StateDB, Listener and Authorizer are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	authOpts := append([]auth.Option{auth.WithClock(opts.Now)}, opts.AuthOptions...)
	s := &Server{
		opts:    opts,
		auth:    auth.NewManager(opts.Config, opts.Vault, authOpts...),
		audit:   audit.NewWriter(opts.StateDB, opts.Now),
		started: opts.Now(),
	}
	mux := http.NewServeMux()
	handlerOpts := connect.WithReadMaxBytes(maxRequestBytes)
	mux.Handle(chainmailv1connect.NewAccountServiceHandler(&accountService{s: s}, handlerOpts))
	mux.Handle(chainmailv1connect.NewAdminServiceHandler(&adminService{s: s}, handlerOpts))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	s.httpSrv = &http.Server{
		Handler:           opts.Authorizer.Middleware(mux),
		ConnContext:       connContext,
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         protocols,
	}
	return s, nil
}

// Serve handles requests until ctx ends, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.httpSrv.Serve(s.opts.Listener) }()
	s.opts.Logger.Info("daemon listening", "addr", s.opts.Listener.Addr().String(), "dev", s.opts.DevMode)
	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

type caller struct {
	uid    uint32
	client string
}

// callerOf identifies who sent a request. The middleware refuses requests
// without a peer, so a missing one here is a wiring bug, and the request
// fails rather than being audited as uid 0.
func callerOf(ctx context.Context, h http.Header) (caller, error) {
	p, ok := PeerFromContext(ctx)
	if !ok {
		return caller{}, api.New(connect.CodePermissionDenied, api.ReasonForbidden, "", errors.New("the socket peer is unknown"))
	}
	c := caller{uid: p.UID, client: h.Get(api.HeaderClient)}
	if c.client == "" {
		c.client = "unknown"
	}
	return c, nil
}

// record writes an audit entry for an action that has run, then returns the
// action's own error as a Connect error. If e.Result is empty it is derived
// from actionErr. A failed audit write fails the request: chainmail must not
// report success for something it could not record.
func (s *Server) record(ctx context.Context, c caller, e audit.Entry, actionErr error) error {
	e.CallerUID = c.uid
	e.Client = c.client
	var cerr *connect.Error
	if actionErr != nil {
		cerr = toConnectError(actionErr)
		e.Reason = api.Detail(cerr).GetReason()
	}
	if e.Result == "" {
		e.Result = audit.ResultOK
		if actionErr != nil {
			e.Result = audit.ResultError
		}
	}
	// The action has already run, so its row is written even if the client
	// has gone away.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := s.audit.Record(auditCtx, e); err != nil {
		s.opts.Logger.Error("audit write failed", "action", e.Action, "err", err)
		return api.New(connect.CodeInternal, api.ReasonInternal, "", fmt.Errorf("%s finished but the audit write failed: %w", e.Action, err))
	}
	if cerr != nil {
		return cerr
	}
	return nil
}
