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
	"regexp"
	"sync"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/idempotency"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/imapstore"
	"github.com/jamesainslie/chainmail/internal/policy"
	"github.com/jamesainslie/chainmail/internal/upload"
	"github.com/jamesainslie/chainmail/internal/vault"
	"github.com/jamesainslie/chainmail/internal/watch"
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
	// NewStore builds the mail store for an account, once, on its first read.
	// Nil means IMAP through internal/mailstore/imapstore. Tests pass mailstore/fake.
	NewStore func(config.ResolvedAccount) mailstore.Store
	// Logger defaults to discarding everything.
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// StateDir holds the daemon's files. Uploads live in its uploads
	// directory. Empty means uploads are refused.
	StateDir string
	// OutcomeSearchInterval is the wait between the tries of the Sent
	// search for a send whose outcome is unknown. Zero means 5 seconds,
	// which makes 3 tries over 10 seconds. Tests shorten it.
	OutcomeSearchInterval time.Duration
	// WatchHeartbeat is how often a Subscribe stream with nothing to send
	// sends a heartbeat. Zero means 30 seconds. Tests shorten it.
	WatchHeartbeat time.Duration
	// DisableWatch starts no watch supervisors, so no account is watched or
	// synced. The WatchService handlers and the event log still work. Tests
	// that count mail store calls set it.
	DisableWatch bool
}

const (
	// maxRequestBytes caps a request message. Secrets and redirect URLs are far smaller.
	maxRequestBytes = 1 << 20
	// composeMaxRequestBytes caps a ComposeService request, which can hold a
	// whole message for SendRaw or one part of an upload. The largest
	// provider limit is 35 MiB; the rest is room for the request's other
	// fields. The handler still enforces the account's own message size.
	composeMaxRequestBytes = 40 << 20
	// composeMaxResponseBytes caps a ComposeService response, which can hold
	// a whole message built as a dry run.
	composeMaxResponseBytes = 40 << 20
	// auditTimeout bounds an audit write that outlives its request.
	auditTimeout = 5 * time.Second
	// shutdownTimeout bounds the graceful part of a shutdown, after which
	// connections are closed.
	shutdownTimeout = 10 * time.Second
	// idleTimeout closes keep-alive connections nobody is using.
	idleTimeout = 2 * time.Minute
)

// Server is the chainmail daemon.
type Server struct {
	opts    Options
	auth    *auth.Manager
	audit   *audit.Writer
	started time.Time
	httpSrv *http.Server
	// auditReader reads the rows audit writes, for ReadAudit.
	auditReader *audit.Reader
	// accountLocks serializes account tests, one account at a time.
	accountLocks keyedLock
	handlers     inflight
	stores       *storeSet
	// idem records send-type calls by idempotency key.
	idem *idempotency.Store
	// uploads holds attachment bytes for later sends. Nil without a StateDir.
	uploads *upload.Store
	compose *composeService
	// newStore builds an account's store with the pool the daemon chose.
	newStore func(config.ResolvedAccount, storeParams) mailstore.Store
	// lim keeps connection limits and runs background probes.
	lim *limitState
	// policy decides which sends are held and which destructive actions
	// are refused.
	policy *policy.Policy
	// watchLog is the durable event log WatchService reads.
	watchLog *watch.Log
	// watchState holds each synced folder's checkpoint and status.
	watchState *watch.State
	// watchWaitUnit is one ReadEvents wait_seconds: a second. Internal
	// tests shorten it.
	watchWaitUnit time.Duration
	// watchAfterTail, when set, runs in a watch read between reading the
	// tail and recovering a group left past it. Internal tests land an
	// event or a cancellation in that window.
	watchAfterTail func(context.Context)
	// watchAfterWait, when set, runs in a ReadEvents wait each time the log's
	// Wait returns. Internal tests make the wakeup late with it.
	watchAfterWait func(context.Context)
	// watchSendHook, when set, runs before each Subscribe send and fails
	// it with any error it returns. Internal tests fail a send with it while
	// the stream is still live.
	watchSendHook func(*chainmailv1.SubscribeResponse) error
	// shutdown is the Done channel of Serve's base context. Serve sets it
	// once, before any handler runs, and it stays the same channel from
	// then on; it closes when the daemon begins to shut down, before it
	// drains. It is nil on a server that is not serving, as in internal
	// tests, and a nil channel never closes.
	shutdown <-chan struct{}
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
	if opts.WatchHeartbeat <= 0 {
		opts.WatchHeartbeat = defaultWatchHeartbeat
	}
	pol, err := policy.New(opts.Config)
	if err != nil {
		return nil, fmt.Errorf("daemon: policy: %w", err)
	}
	authOpts := append([]auth.Option{auth.WithClock(opts.Now)}, opts.AuthOptions...)
	s := &Server{
		opts:          opts,
		auth:          auth.NewManager(opts.Config, opts.Vault, authOpts...),
		audit:         audit.NewWriter(opts.StateDB, opts.Now),
		auditReader:   audit.NewReader(opts.StateDB),
		started:       opts.Now(),
		policy:        pol,
		watchLog:      watch.NewLog(opts.StateDB, opts.Now),
		watchState:    watch.NewState(opts.StateDB),
		watchWaitUnit: time.Second,
	}
	s.lim = newLimitState(opts.StateDB, opts.Now)
	s.newStore = func(acct config.ResolvedAccount, p storeParams) mailstore.Store {
		return imapstore.New(acct, s.auth, imapstore.Options{
			RootCAs: opts.RootCAs, Logger: opts.Logger, Now: opts.Now,
			PoolSize: p.poolSize, OnConnLimit: p.onConnLimit,
		})
	}
	if opts.NewStore != nil {
		s.newStore = func(acct config.ResolvedAccount, _ storeParams) mailstore.Store { return opts.NewStore(acct) }
	}
	s.stores = &storeSet{build: s.buildStore, buildSized: s.buildSized, firstUse: s.probeIfStale, logger: opts.Logger}
	if err := s.openSendState(); err != nil {
		return nil, err
	}
	s.compose = newComposeService(s)
	mux := http.NewServeMux()
	handlerOpts := connect.WithReadMaxBytes(maxRequestBytes)
	mux.Handle(chainmailv1connect.NewAccountServiceHandler(&accountService{s: s}, handlerOpts))
	mux.Handle(chainmailv1connect.NewAdminServiceHandler(&adminService{s: s}, handlerOpts))
	mux.Handle(chainmailv1connect.NewMessageServiceHandler(&messageService{s: s}, handlerOpts, connect.WithSendMaxBytes(maxResponseBytes)))
	composePath, composeHandler := chainmailv1connect.NewComposeServiceHandler(s.compose,
		connect.WithReadMaxBytes(composeMaxRequestBytes), connect.WithSendMaxBytes(composeMaxResponseBytes))
	// UploadAttachment ends a stalled stream's body read itself.
	mux.Handle(composePath, withReadAbort(composeHandler))
	mux.Handle(chainmailv1connect.NewOrganizeServiceHandler(&organizeService{s: s}, handlerOpts, connect.WithSendMaxBytes(maxResponseBytes)))
	mux.Handle(chainmailv1connect.NewWatchServiceHandler(&watchService{s: s}, handlerOpts, connect.WithSendMaxBytes(maxResponseBytes)))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	// ReadTimeout and WriteTimeout stay unset. The socket is local, and a
	// unary call such as accounts test legitimately runs for up to a
	// minute, which a whole-request timeout would cut off, and a watch
	// Subscribe stream stays open for as long as its watcher runs. Handlers
	// bound their own work instead, and shutdown cancels it.
	s.httpSrv = &http.Server{
		Handler:           s.handlers.track(opts.Authorizer.Middleware(mux)),
		ConnContext:       connContext,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idleTimeout,
		Protocols:         protocols,
	}
	return s, nil
}

// Serve handles requests until ctx ends, then shuts down. Shutting down
// cancels every request in flight, so mail server probes stop and report
// TIMEOUT while their audit rows are still written. Serve returns only once
// every handler has returned, so the caller can close the state database.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.recoverSends(ctx); err != nil {
		_ = s.opts.Listener.Close() // the recovery error is the one to report
		if ctx.Err() != nil {
			return nil // stopped before serving: a shutdown, not a failure
		}
		return err
	}
	base, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	// Watch settlement watches base, so a shutdown ends it even after a
	// request's own context has ended with its deadline. Set before any
	// handler runs.
	s.shutdown = base.Done()
	defer s.startBackground(base)()
	s.httpSrv.BaseContext = func(net.Listener) context.Context { return base }
	errc := make(chan error, 1)
	go func() { errc <- s.httpSrv.Serve(s.opts.Listener) }()
	s.opts.Logger.Info("daemon listening", "addr", s.opts.Listener.Addr().String(), "dev", s.opts.DevMode)
	select {
	case err := <-errc:
		cancelBase()
		// Serve has stopped, so nothing will finish the connections it
		// left open but Close.
		closeErr := s.httpSrv.Close()
		s.drain()
		return errors.Join(fmt.Errorf("serving: %w", err), closeErr)
	case <-ctx.Done():
	}
	cancelBase()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	var err error
	if shutdownErr := s.httpSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		// Close drops the connections still open. It does not wait for
		// their handlers, which wait does below.
		err = errors.Join(fmt.Errorf("shutting down: %w", shutdownErr), s.httpSrv.Close())
	}
	// Shutdown and Close make Serve return http.ErrServerClosed, which is
	// the normal outcome. Anything else is reported.
	if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, fmt.Errorf("serving: %w", serveErr))
	}
	s.drain()
	return err
}

// drain waits for every handler to return, stops the background probes,
// then logs out of the mail servers. Each store logs out within 5 seconds,
// all in parallel.
func (s *Server) drain() {
	s.handlers.wait()
	s.lim.stop()
	if err := s.stores.closeAll(); err != nil {
		s.opts.Logger.Warn("logging out of mail servers", "reason", apiReason(err))
	}
}

// inflight counts running handlers so shutdown can wait for them. Once
// draining has begun it refuses new requests rather than race the wait.
type inflight struct {
	mu       sync.Mutex
	draining bool
	wg       sync.WaitGroup
}

func (f *inflight) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.draining {
			f.mu.Unlock()
			http.Error(w, "the daemon is shutting down", http.StatusServiceUnavailable)
			return
		}
		f.wg.Add(1)
		f.mu.Unlock()
		defer f.wg.Done()
		next.ServeHTTP(w, r)
	})
}

// wait refuses new requests and waits for the running ones to return.
func (f *inflight) wait() {
	f.mu.Lock()
	f.draining = true
	f.mu.Unlock()
	f.wg.Wait()
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
	switch {
	case c.client == "":
		c.client = "unknown"
	case !clientKindRE.MatchString(c.client):
		c.client = invalidTarget
	}
	return c, nil
}

// clientKindRE matches the client kinds the audit log records, such as
// "cli" or "mcp". The kind is self-reported, so anything else is recorded
// as invalidTarget. The peer's uid is the authoritative identity.
var clientKindRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

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
