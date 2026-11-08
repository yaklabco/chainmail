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
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/idempotency"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/imapstore"
	"github.com/yaklabco/chainmail/internal/policy"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/upload"
	"github.com/yaklabco/chainmail/internal/vault"
	"github.com/yaklabco/chainmail/internal/watch"
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
	// synced, and pools are not shrunk for watcher connections. The
	// WatchService handlers, the event log and its trimmer still work.
	// Tests that count mail store calls set it, through daemontest.
	DisableWatch bool
	// WatchSweepFloor is the shortest wait between expunge sweeps of a
	// watched folder after the server reports an EXPUNGE. Zero means 5
	// minutes (watch.ExpungeSweepFloor). Integration tests shorten it.
	WatchSweepFloor time.Duration
	// CacheDir is the directory of cache.db, created with mode 0700 when
	// missing. Empty runs without a cache, so every search and read uses
	// the mail servers. The CLI passes [cache] dir, or the state directory.
	CacheDir string
	// DisableCache runs without a cache whatever the config says.
	// daemontest sets it unless a test asks for the cache, so tests that
	// count mail store calls keep the server path.
	DisableCache bool
	// NewCalendarStore builds the calendar store of an account whose
	// calendar store is not none, once, in New. Nil means the real backend:
	// calstore/google or calstore/caldav over the account's credentials.
	// Returning nil gives the account no store. Tests pass calstore/fake.
	NewCalendarStore func(config.ResolvedAccount) calstore.Store
	// DisableCalendar runs without calendars whatever the config says: no
	// account has a calendar store, calendar.db is not opened and nothing
	// syncs. daemontest sets it unless a test asks for calendars.
	DisableCalendar bool
	// CalendarPoll replaces the [calendar] poll interval. Zero keeps the
	// config's. Tests shorten it.
	CalendarPoll time.Duration
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
	// engine turns sync passes into events and checkpoints.
	engine *watch.Engine
	// items keeps the work items, their criteria, blockers and thread
	// links in state.db, and appends their events to watchLog.
	items *items.Store
	// supervisors watch and sync the accounts while Serve runs.
	supervisors supervisorSet
	// trimInterval is how often the event log is trimmed. Tests shorten it.
	trimInterval time.Duration
	// itemsTrimTick, when set, replaces the ticker that runs the item
	// trim every trimInterval, so a test can trigger each trim.
	itemsTrimTick <-chan time.Time
	// cache is cache.db, or nil when the cache is off or did not open.
	// Serve opens it before any handler, background loop or supervisor
	// runs, and closes it after all of them have stopped.
	cache *cache.Store
	// cacheErr is why cache.db did not open, or nil.
	cacheErr error
	// cacheAccounts are the cache workers of the watched accounts with the
	// cache enabled, by account. startWatch fills it before any handler
	// runs, and it never changes after.
	cacheAccounts map[string]*cache.Account
	// cacheEvictor applies body retention and the size cap to the whole
	// cache file. Nil without a cache.
	cacheEvictor *cache.Evictor
	// cacheDown is set while cache.db cannot be written because its disk
	// is full. Searches and reads then use the mail servers, and status
	// says error.
	cacheDown atomic.Bool
	// cacheProbeAccount is the account whose budget row the write probe
	// touches: the first with the cache enabled.
	cacheProbeAccount string
	// cacheProbe tries one write to cache.db. Internal tests replace it.
	cacheProbe func(context.Context) error
	// cacheClock is the time of the cache workers, the evictor and the
	// housekeeping: Options.Now, the time cache.db stamps rows with, and
	// time.After. Internal tests replace it before the cache opens.
	cacheClock watch.Clock
	// itemThreads resolves item: search terms into thread keys. Nil when
	// the daemon has no item store, which makes item: an invalid argument.
	itemThreads itemResolver
	// rules keeps the rule definitions, their revisions and runs, and the
	// act-once table in state.db, and appends rule events to watchLog.
	rules *rules.Store
	// rulesClock is the time of the rule runners' waits, the rule
	// scheduler and the due checker: Options.Now and time.After. Internal
	// tests replace it before a runner is made or a loop starts.
	rulesClock watch.Clock
	// ruleApplied reads the rules' act-once table, for the skipped count
	// of a dry-run. Nil until the rule store is built, which makes every
	// dry-run skip nothing.
	ruleApplied appliedLookup
	// dueChecker writes the item due events: at each due time, once at
	// start-up, and inside every items write that changes a due date,
	// state or deletion, through the items store's due hook.
	dueChecker *rules.DueChecker
	// ruleRunners are the rule runners of the accounts with a cache
	// worker, by account. startWatch fills it before startBackground starts
	// the rule scheduler and before any handler runs, and it never changes
	// after. An account without a runner acts on no rule: its scheduled and
	// manual runs are recorded as cache_not_ready with the reason
	// cache_disabled.
	ruleRunners map[string]*ruleRunner
	// rulesWake wakes the rule scheduler after a rule change so it plans
	// again. It holds at most one wakeup.
	rulesWake chan struct{}
	// ruleScheduler stops the rule scheduler that startBackground runs and
	// joins it, with the scheduled run it is in. drain calls it before it
	// closes the mail stores.
	ruleScheduler schedulerStop
	// calcache is calendar.db, or nil when calendars are disabled, there is
	// no cache directory, or it did not open. Serve opens it before any
	// handler, cache worker or syncer runs, and closes it after all of
	// them have stopped.
	calcache *calcache.Store
	// invites is the invite hook over calcache that the cache workers and
	// the invite retry loop share, built on first use (serverInviteHook).
	// Guarded by invitesMu.
	invitesMu sync.Mutex
	invites   *inviteHook
	// calendars holds every configured account's calendar runtime, by
	// account. New fills it with the stores and Serve adds the syncers
	// before any handler or cache worker runs; it never changes after.
	calendars map[string]*calAccount
	// calSettings is the [calendar] table, resolved.
	calSettings config.CalendarSettings
	// calClock is the time of the calendar syncers: Options.Now and
	// time.After. Internal tests replace it before Serve.
	calClock watch.Clock
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
	if opts.WatchSweepFloor <= 0 {
		opts.WatchSweepFloor = watch.ExpungeSweepFloor
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
		watchLog:      watch.NewLog(opts.StateDB, opts.Now, opts.Logger),
		watchState:    watch.NewState(opts.StateDB),
		watchWaitUnit: time.Second,
		trimInterval:  eventTrimInterval,
	}
	s.cacheProbe = s.probeCache
	s.cacheClock = daemonClock{now: opts.Now}
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
	s.engine = watch.NewEngine(opts.StateDB, s.watchLog, s.watchState, watch.EngineOptions{Now: opts.Now, Snippet: snippetOf, OnNew: s.itemMail})
	s.items = items.NewStore(opts.StateDB, s.watchLog, opts.Now)
	// item: searches resolve item IDs to thread keys through the store.
	s.itemThreads = s.items
	// Rules live in state.db beside the items. at schedules and due times
	// follow the [rules] time zone.
	rs := opts.Config.RulesSettings()
	s.rules = rules.NewStore(rules.StoreOptions{DB: opts.StateDB, Log: s.watchLog, Now: opts.Now, Location: rs.Location})
	s.ruleApplied = s.rules
	s.dueChecker = rules.NewDueChecker(opts.StateDB, s.watchLog, opts.Now, rules.DueSettings{
		Location: rs.Location, At: rules.ClockTime{Hour: rs.DueHour, Minute: rs.DueMinute}, SoonDays: rs.DueSoonDays,
	})
	// Every items write that changes a due date, state or deletion checks
	// its items' due stages in its own transaction.
	s.items.SetDueHook(s.dueChecker.Hook)
	s.dueChecker.SetLogger(opts.Logger)
	s.rulesClock = daemonClock{now: opts.Now}
	s.rulesWake = make(chan struct{}, 1)
	if err := s.openSendState(); err != nil {
		return nil, err
	}
	s.calClock = daemonClock{now: opts.Now}
	s.buildCalendars()
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
	mux.Handle(chainmailv1connect.NewCacheServiceHandler(&cacheService{s: s}, handlerOpts, connect.WithSendMaxBytes(maxResponseBytes)))
	mux.Handle(chainmailv1connect.NewItemsServiceHandler(&itemsService{s: s}, handlerOpts, connect.WithSendMaxBytes(maxResponseBytes)))
	mux.Handle(chainmailv1connect.NewRulesServiceHandler(&rulesService{s: s}, handlerOpts, connect.WithSendMaxBytes(maxResponseBytes)))
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
// Before it accepts a connection it creates the Admin items that are
// missing, then recovers interrupted sends; either failing ends Serve.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.ensureAdmin(ctx); err != nil {
		_ = s.opts.Listener.Close() // the Admin items error is the one to report
		if ctx.Err() != nil {
			return nil // stopped before serving: a shutdown, not a failure
		}
		return err
	}
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
	// cache.db opens before the background loops and the supervisors that
	// use it, and closes after both have stopped: deferred calls run last
	// in, first out, and drain has stopped the supervisors before Serve
	// returns.
	s.openCache(base)
	defer s.closeCache()
	// calendar.db opens beside it, before the cache workers whose invite
	// hook writes to it and the syncers, and closes before cache.db, after
	// both have stopped.
	s.openCalendar(base)
	defer s.closeCalendar()
	// Supervisors start once sends are recovered, on base, so a shutdown
	// cancels them with the requests. drain joins them. They start before
	// the background loops, since the rule scheduler among those reads
	// the rule runners startWatch records.
	s.startWatch(base)
	// The background loops stop and are joined when Serve returns, after
	// drain and before closeCache, so none outlives state.db, which the
	// caller closes after Serve. The rule scheduler among them is stopped
	// and joined sooner, by drain before it closes the mail stores, since
	// its runs use them.
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

// drain waits for every handler to return, stops the rule scheduler and
// waits for the scheduled run it is in, stops the supervisors, the cache
// workers and the background probes, then logs out of the mail servers.
// The scheduler, the supervisors and the workers stop first, the workers
// closing their watch connections within watch.ShutdownLogout, so none
// holds a store closeAll is closing or opens one after it: a scheduled
// forward whose SMTP send is under way finishes it, and writes its Sent
// copy, before drain goes on. Each store logs out within 5 seconds, all
// in parallel.
func (s *Server) drain() {
	s.handlers.wait()
	s.ruleScheduler.stop()
	s.stopWatch()
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
// fails rather than being audited as uid 0. A peer that names one of the
// daemon's own client kinds, rule or daemon, is recorded as invalidTarget,
// so the audit log tells the daemon's actions from a client's.
func callerOf(ctx context.Context, h http.Header) (caller, error) {
	p, ok := PeerFromContext(ctx)
	if !ok {
		return caller{}, api.New(connect.CodePermissionDenied, api.ReasonForbidden, "", errors.New("the socket peer is unknown"))
	}
	c := caller{uid: p.UID, client: h.Get(api.HeaderClient)}
	switch {
	case c.client == "":
		c.client = "unknown"
	case !clientKindRE.MatchString(c.client), c.client == ruleClient, c.client == daemonClient:
		c.client = invalidTarget
	}
	return c, nil
}

// clientKindRE matches the client kinds the audit log records, such as
// "cli" or "mcp". The kind is self-reported, so anything else is recorded
// as invalidTarget. The peer's uid is the authoritative identity.
var clientKindRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// ruleClient is the audit client of the actions rules take.
const ruleClient = "rule"

// ruleCaller is who a rule's actions run as: the daemon's own uid, as its
// background probe records, with client kind rule. No socket peer can
// claim the kind, so audit --client rule shows only rule actions.
func ruleCaller() caller {
	return caller{uid: uint32(os.Getuid()), client: ruleClient}
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

// auditedCall runs one audited call the way the items and rules handlers
// do: it identifies the caller, gives the call timeout unless the client
// set an earlier deadline, and runs run, which sets the audit entry's
// targets as it validates and returns the response and the action's
// error. It then records the entry, so refusals are audited too, and
// returns the response or the action's Connect error. itemCall and
// ruleCall wrap it, so an audit or deadline change applies to both.
func auditedCall[Req, Resp any](ctx context.Context, s *Server, req *connect.Request[Req], action string, timeout time.Duration,
	run func(ctx context.Context, c caller, entry *audit.Entry) (*Resp, error),
) (*connect.Response[Resp], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, timeout)
	defer cancel()
	entry := audit.Entry{Action: action}
	resp, runErr := run(ctx, c, &entry)
	if err := s.record(ctx, c, entry, runErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
