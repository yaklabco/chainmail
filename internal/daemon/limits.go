package daemon

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/limits"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/imapstore"
)

const (
	// probeTimeout bounds one account's probe, waiting for its calls in
	// progress included. Sixteen connections 250 ms apart take seconds, but
	// each login can take longer.
	probeTimeout = 2 * time.Minute
	// connsReserved are the connections left for the user's other mail
	// clients: a refusal at a pool of n records a cap of n plus these.
	connsReserved = 2
	// daemonClient is the audit client of what the daemon does on its own.
	daemonClient = "daemon"
	// actionProbe is the audit action of a probe.
	actionProbe = "limits.probe"
	// maxProbes bounds the probes running at once across accounts, explicit
	// and background together. Each can hold up to 16 connections.
	maxProbes = 4
)

// errProbeUnsupported is a store that cannot open connections for a probe,
// such as a fake.
var errProbeUnsupported = errors.New("probe not supported")

// prober is a store that can open connections outside its pool, which
// limits.Probe counts. imapstore.Store is one.
type prober interface {
	ProbeDial(ctx context.Context) (io.Closer, error)
}

// connCounter is a store that reports its pooled connections still open,
// a stream reader's included. imapstore.Store is one. A probe adds them to
// what it opened, because they hold slots on the server while it counts.
type connCounter interface {
	OpenConns() int
}

// connPeaker is a store that keeps the most connections it held open at
// once since it was last reset, those that have closed since included.
// imapstore.Store is one. A probe resets it before its first dial and adds
// the peak after its last, so a call's connection that came and went while
// it dialed still counts.
type connPeaker interface {
	ResetConnPeak()
	ConnPeak() int
}

// The real store is probed and counted through these interfaces.
var (
	_ prober      = (*imapstore.Store)(nil)
	_ connCounter = (*imapstore.Store)(nil)
	_ connPeaker  = (*imapstore.Store)(nil)
)

// openConns is how many connections st holds open, or 0 when it cannot say.
func openConns(st mailstore.Store) int {
	if c, ok := st.(connCounter); ok {
		return c.OpenConns()
	}
	return 0
}

// storeParams are what the daemon chooses for an account's store.
type storeParams struct {
	// poolSize is the most connections the store opens at once.
	poolSize int
	// onConnLimit hears the pool's new cap when the server refuses a
	// connection for being over its limit.
	onConnLimit func(newMax int)
}

// limitState keeps the stored connection limits and the background probes.
type limitState struct {
	store *limits.Store
	// probeOpts tunes every probe. Tests shorten the interval.
	probeOpts limits.ProbeOptions

	// connMu serializes recording connection-limit refusals, which read the
	// stored cap before lowering it, and guards counting.
	connMu sync.Mutex
	// counting are the accounts a probe is measuring, from before its first
	// dial until it has installed the store sized from its result. A
	// refusal then may come from the probe's own connections filling the
	// server, so it is not recorded: the probe's result stands.
	counting map[string]bool

	// slots bounds the probes running at once, maxProbes of them.
	slots chan struct{}

	mu      sync.Mutex
	ctx     context.Context // ends when the daemon stops
	cancel  context.CancelFunc
	stopped bool
	probing map[string]bool // accounts with a background probe running
	wg      sync.WaitGroup
}

func newLimitState(db *sql.DB, now func() time.Time) *limitState {
	ctx, cancel := context.WithCancel(context.Background())
	return &limitState{
		store:    limits.NewStore(db, now),
		slots:    make(chan struct{}, maxProbes),
		counting: map[string]bool{},
		ctx:      ctx,
		cancel:   cancel,
		probing:  map[string]bool{},
	}
}

// beginCount marks account as being probed, so runtime refusals are not
// recorded, until the returned end runs. A refusal being recorded as it
// starts finishes first, and the probe's result then replaces it.
func (l *limitState) beginCount(account string) (end func()) {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	l.counting[account] = true
	return func() {
		l.connMu.Lock()
		defer l.connMu.Unlock()
		delete(l.counting, account)
	}
}

// start runs f in the background for account, unless one already runs for
// it or the daemon is stopping.
func (l *limitState) start(account string, f func(context.Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped || l.probing[account] {
		return
	}
	l.probing[account] = true
	l.wg.Go(func() {
		defer func() {
			l.mu.Lock()
			delete(l.probing, account)
			l.mu.Unlock()
		}()
		f(l.ctx)
	})
}

// acquireSlot waits for one of the maxProbes probe slots, or for ctx to
// end. The caller runs release once its probe is done.
func (l *limitState) acquireSlot(ctx context.Context) (release func(), err error) {
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// wait returns once every background probe started so far has finished.
func (l *limitState) wait() { l.wg.Wait() }

// stop cancels the background probes, starts no more, and waits for them.
func (l *limitState) stop() {
	l.mu.Lock()
	l.stopped = true
	l.cancel()
	l.mu.Unlock()
	l.wg.Wait()
}

// buildStore builds acct's store, sized from its stored limit.
func (s *Server) buildStore(acct config.ResolvedAccount) mailstore.Store {
	return s.newStore(acct, s.storeParams(acct))
}

// storeParams sizes acct's pool: pool from config, else from the stored
// limit, else the preset default, less what the account's watchers need
// (poolSize).
func (s *Server) storeParams(acct config.ResolvedAccount) storeParams {
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	var stored *limits.Limit
	switch l, ok, err := s.lim.store.Get(ctx, acct.Name); {
	case err != nil:
		s.opts.Logger.Warn("reading the connection limit; using the default pool", "account", acct.Name, "err", err)
	case ok:
		stored = &l
	}
	return s.paramsWithPool(acct, s.poolSize(acct, stored))
}

// buildSized builds acct's store with a pool of poolSize, which a probe
// gives from its own result rather than from what is stored.
func (s *Server) buildSized(acct config.ResolvedAccount, poolSize int) mailstore.Store {
	return s.newStore(acct, s.paramsWithPool(acct, poolSize))
}

// paramsWithPool are acct's store parameters with a pool of poolSize.
func (s *Server) paramsWithPool(acct config.ResolvedAccount, poolSize int) storeParams {
	return storeParams{
		poolSize:    poolSize,
		onConnLimit: func(newMax int) { s.recordConnLimit(acct, newMax) },
	}
}

// poolSize sizes acct's pool with limits.PoolSize, from config, the stored
// limit or the preset, then fits it into the connection budget the
// account's watchers leave (watchBudget). Every store the daemon builds or
// a probe installs is sized here.
func (s *Server) poolSize(acct config.ResolvedAccount, stored *limits.Limit) int {
	pool := limits.PoolSize(acct.Pool, stored, acct.Provider.DefaultPool)
	return watchBudget(pool, acct, stored, !s.opts.DisableWatch)
}

// watchBudget shrinks pool so that the account's watcher connections,
// which are dialed outside the pool, fit under its connection cap with it.
// The cap is the probed IMAPCap when one is stored, which counts watch
// connections already, else the preset's MaxConnections. The watchers
// never shrink, and the pool never drops below 1. A configured pool is
// capped too: pool plus watchers above the server's cap would make logins
// fail once every connection is in use, so config cannot ask for that. A
// daemon that runs no supervisors (watching false) has no watchers to make
// room for.
func watchBudget(pool int, acct config.ResolvedAccount, stored *limits.Limit, watching bool) int {
	watchers := 0
	if watching {
		watchers = acct.WatchCount()
	}
	if watchers == 0 {
		return pool
	}
	connCap := acct.Provider.MaxConnections
	if stored != nil {
		connCap = stored.IMAPCap
	}
	if connCap < 1 {
		return pool
	}
	return min(pool, max(1, connCap-watchers))
}

// recordConnLimit stores refusalCap(acct, newMax), or the stored cap when
// that is lower, after the server refused a connection and the pool shrank
// to newMax. A refusal never raises a stored
// cap, but it always records the time: the refusal is a fresh observation
// of the limit, so it postpones the next background probe. While a probe
// measures the account, nothing is recorded: the probe's own connections
// may be what filled the server, and its result is stored instead. The pool
// that was refused still shrank. This never waits for the probe, so a call
// can record a refusal while it holds the read lock a probe's replace waits
// for.
func (s *Server) recordConnLimit(acct config.ResolvedAccount, newMax int) {
	account := acct.Name
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	s.lim.connMu.Lock()
	defer s.lim.connMu.Unlock()
	if s.lim.counting[account] {
		s.opts.Logger.Info("server refused a connection while a probe measures the account; the probe's result stands", "account", account, "pool", newMax)
		return
	}
	newCap := s.refusalCap(acct, newMax)
	l, ok, err := s.lim.store.Get(ctx, account)
	if err != nil {
		s.opts.Logger.Warn("reading the connection limit", "account", account, "err", err)
		return
	}
	if ok {
		newCap = min(newCap, l.IMAPCap)
	}
	if err := s.lim.store.Put(ctx, limits.Limit{Account: account, IMAPCap: newCap, ProbedAt: s.opts.Now().UTC()}); err != nil {
		s.opts.Logger.Warn("storing the connection limit", "account", account, "err", err)
		return
	}
	s.opts.Logger.Info("server refused a connection; recorded the connection limit", "account", account, "imap_cap", newCap)
}

// refusalCap is the cap a refusal records for acct once its pool shrank to
// newMax. Without watchers it is newMax plus the reserved connections, so a
// store rebuilt from it gets a pool of newMax again (limits.PoolSize takes
// the reserve back off). With watchers it is what the server accepted:
// newMax pooled connections and the account's watcher connections, which
// are dialed outside the pool. watchBudget takes the watchers straight off
// the stored cap, a configured pool included, so adding the reserve there
// would let the rebuilt pool and its watchers exceed what the server just
// refused.
func (s *Server) refusalCap(acct config.ResolvedAccount, newMax int) int {
	watchers := 0
	if !s.opts.DisableWatch {
		watchers = acct.WatchCount()
	}
	if watchers == 0 {
		return newMax + connsReserved
	}
	return newMax + watchers
}

// probeIfStale starts a background probe of acct when it has no stored
// limit or a stale one. It runs on an account's first use and never blocks.
func (s *Server) probeIfStale(acct config.ResolvedAccount) {
	s.lim.start(acct.Name, func(ctx context.Context) {
		if st := s.stores.peek(acct); st != nil {
			if _, ok := st.(prober); !ok {
				return
			}
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		res := s.probeAccount(ctx, acct, true)
		if res.skipped || errors.Is(res.err, errProbeUnsupported) {
			return
		}
		if res.err != nil {
			s.opts.Logger.Warn("background connection probe failed", "account", acct.Name, "reason", apiReason(res.err))
		}
		c := caller{uid: uint32(os.Getuid()), client: daemonClient}
		if err := s.record(ctx, c, res.auditEntry(), nil); err != nil {
			s.opts.Logger.Warn("auditing the background connection probe", "account", acct.Name, "err", err)
		}
	})
}

// probeResult is the outcome of probing one account.
type probeResult struct {
	acct    config.ResolvedAccount
	limit   limits.Limit
	pool    int
	skipped bool // the limit was fresh by the time the probe could run
	err     error
}

// probeAccount measures acct's connection limit, stores it and rebuilds the
// store with the new pool size. It first takes one of the maxProbes probe
// slots, then the account's probe lock, so probes of one account take
// turns. It dials through a store of its own and holds no lock the
// account's calls need while it counts: they go on, on the current store.
// Only once the limit is stored does it take the account's write lock, to
// install a store sized from it. The stored limit, the installed pool, the
// result and its audit row all come from the one measurement: runtime
// refusals meanwhile are not recorded (recordConnLimit).
//
// The account's own connections, in its current and retired stores, hold
// server slots the probe cannot get, so they are added to its count: the
// most the current store held at once from before the first dial to after
// the last, and what the retired ones held at the start (storeSet.countConns).
// A connection that came and went while the probe dialed is counted even if
// it closed before the server refused the probe, so the cap can read high,
// which the next runtime refusal corrects, but not low.
// With onlyIfStale, a limit that is fresh once the probe lock is held is
// kept.
func (s *Server) probeAccount(ctx context.Context, acct config.ResolvedAccount, onlyIfStale bool) probeResult {
	res := probeResult{acct: acct}
	release, err := s.lim.acquireSlot(ctx)
	if err != nil {
		res.err = err
		return res
	}
	defer release()
	unlock, err := s.stores.lockProbe(ctx, acct)
	if err != nil {
		res.err = err
		return res
	}
	defer unlock()
	res.err = s.probeLocked(ctx, acct, onlyIfStale, &res)
	return res
}

// probeLocked is probeAccount once it holds the account's probe lock.
func (s *Server) probeLocked(ctx context.Context, acct config.ResolvedAccount, onlyIfStale bool, res *probeResult) error {
	if onlyIfStale {
		l, ok, err := s.lim.store.Get(ctx, acct.Name)
		if err != nil {
			return err
		}
		if ok && !limits.Stale(l, s.opts.Now()) {
			res.skipped = true
			return nil
		}
	}
	defer s.lim.beginCount(acct.Name)()
	// The probe's own store is never installed, so no call uses it and
	// its connections are not among the account's. It dials outside any
	// pool, so its pool size does not matter, and a refusal it meets is the
	// probe's result rather than one to record.
	st := s.newStore(acct, storeParams{poolSize: 1})
	defer func() {
		if err := st.Close(); err != nil {
			s.opts.Logger.Warn("logging out of the probe's store", "account", acct.Name, "reason", errReason(err))
		}
	}()
	p, ok := st.(prober)
	if !ok {
		return errProbeUnsupported
	}
	inUse := s.stores.countConns(acct)
	n, err := limits.Probe(ctx, p.ProbeDial, s.lim.probeOpts)
	if err != nil {
		// A cancelled probe returns no count, and a failed one no cap.
		return err
	}
	n += inUse()
	l := limits.Limit{Account: acct.Name, IMAPCap: n, ProbedAt: s.opts.Now().UTC()}
	pool := s.poolSize(acct, &l)
	if err := s.lim.store.Put(ctx, l); err != nil {
		return err
	}
	if err := s.stores.replace(ctx, acct, pool); err != nil {
		return err
	}
	res.limit = l
	res.pool = pool
	return nil
}

// auditEntry is the probe's audit row: the cap and pool on success, the
// reason on failure.
func (r probeResult) auditEntry() audit.Entry {
	e := audit.Entry{Account: r.acct.Name, Action: actionProbe}
	if r.err != nil {
		e.Result = audit.ResultError
		e.Reason = apiReason(r.err)
		return e
	}
	e.Targets = []string{tally("imap_cap", r.limit.IMAPCap), tally("pool", r.pool)}
	return e
}

// proto is the API form of the result.
func (r probeResult) proto() *chainmailv1.AccountLimit {
	out := &chainmailv1.AccountLimit{Account: r.acct.Name}
	if r.err != nil {
		out.Error = probeItemError(r.err)
		return out
	}
	out.ImapCap = uint32(r.limit.IMAPCap)
	out.PoolSize = uint32(r.pool)
	out.ProbedAt = timestamppb.New(r.limit.ProbedAt)
	return out
}

// probeText is the whole message a client sees for a failed probe, by
// reason. The cause can carry the mail server's text, so neither the client
// nor the daemon's logs see it: logs carry the reason only.
var probeText = map[string]string{
	api.ReasonServerUnavailable: "the mail server refused or dropped the first connection",
	api.ReasonTimeout:           "timed out",
	api.ReasonAuthRequired:      "the account needs authentication",
	api.ReasonAuthRejected:      "the server rejected the credentials",
	api.ReasonInternal:          "the probe failed",
}

// probeItemError is the API form of a failed probe, with fixed text.
func probeItemError(err error) *chainmailv1.ItemError {
	if errors.Is(err, errProbeUnsupported) {
		return &chainmailv1.ItemError{Reason: api.ReasonInternal, Message: errProbeUnsupported.Error()}
	}
	d := api.Detail(toConnectError(err))
	msg, ok := probeText[d.GetReason()]
	if !ok {
		msg = probeText[api.ReasonInternal]
	}
	return &chainmailv1.ItemError{Reason: d.GetReason(), Message: msg, Hint: d.GetHint()}
}

// probeTargets resolves the accounts a ProbeLimits call names, in the order
// given without repeats, or every account by name.
func (s *Server) probeTargets(names []string) ([]config.ResolvedAccount, error) {
	cfg := s.opts.Config
	if len(names) == 0 {
		names = cfg.AccountNames()
	}
	seen := make(map[string]bool, len(names))
	out := make([]config.ResolvedAccount, 0, len(names))
	for _, name := range names {
		if !config.ValidName(name) {
			return nil, errInvalidAccountName
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		acct, err := cfg.Account(name)
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
	}
	return out, nil
}

// probeLimits probes accts concurrently, at most maxProbes at once with
// background probes included, and records one audit row for each. A failed
// probe fails only its account.
func (s *Server) probeLimits(ctx context.Context, c caller, accts []config.ResolvedAccount) (*chainmailv1.ProbeLimitsResponse, error) {
	ctx, cancel := withDeadline(ctx, probeTimeout)
	defer cancel()
	results := make([]probeResult, len(accts))
	var wg sync.WaitGroup
	for i, acct := range accts {
		wg.Go(func() { results[i] = s.probeAccount(ctx, acct, false) })
	}
	wg.Wait()
	resp := &chainmailv1.ProbeLimitsResponse{}
	var auditErr error
	for _, r := range results {
		if r.err != nil {
			s.opts.Logger.Warn("connection probe failed", "account", r.acct.Name, "reason", apiReason(r.err))
		}
		if err := s.record(ctx, c, r.auditEntry(), nil); err != nil && auditErr == nil {
			auditErr = err
		}
		resp.Limits = append(resp.Limits, r.proto())
	}
	if auditErr != nil {
		return nil, auditErr
	}
	return resp, nil
}
