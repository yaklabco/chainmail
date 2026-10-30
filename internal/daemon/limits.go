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
)

// errProbeUnsupported is a store that cannot open connections for a probe,
// such as a fake.
var errProbeUnsupported = errors.New("probe not supported")

// prober is a store that can open connections outside its pool, which
// limits.Probe counts. imapstore.Store is one.
type prober interface {
	ProbeDial(ctx context.Context) (io.Closer, error)
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
	// stored cap before lowering it.
	connMu sync.Mutex

	mu      sync.Mutex
	ctx     context.Context // ends when the daemon stops
	cancel  context.CancelFunc
	stopped bool
	probing map[string]bool // accounts with a background probe running
	wg      sync.WaitGroup
}

func newLimitState(db *sql.DB, now func() time.Time) *limitState {
	ctx, cancel := context.WithCancel(context.Background())
	return &limitState{store: limits.NewStore(db, now), ctx: ctx, cancel: cancel, probing: map[string]bool{}}
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
// limit, else the preset default.
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
	return storeParams{
		poolSize:    limits.PoolSize(acct.Pool, stored, acct.Provider.DefaultPool),
		onConnLimit: func(newMax int) { s.recordConnLimit(acct.Name, newMax) },
	}
}

// recordConnLimit stores a cap of newMax plus the reserved connections
// after the server refused a connection and the pool shrank to newMax. A
// refusal never raises a stored cap.
func (s *Server) recordConnLimit(account string, newMax int) {
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	s.lim.connMu.Lock()
	defer s.lim.connMu.Unlock()
	newCap := newMax + connsReserved
	l, ok, err := s.lim.store.Get(ctx, account)
	if err != nil {
		s.opts.Logger.Warn("reading the connection limit", "account", account, "err", err)
		return
	}
	if ok && l.IMAPCap <= newCap {
		return
	}
	if err := s.lim.store.Put(ctx, limits.Limit{Account: account, IMAPCap: newCap}); err != nil {
		s.opts.Logger.Warn("storing the connection limit", "account", account, "err", err)
		return
	}
	s.opts.Logger.Info("server refused a connection; lowered the stored connection limit", "account", account, "imap_cap", newCap)
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
			s.opts.Logger.Warn("background connection probe failed", "account", acct.Name, "reason", apiReason(res.err), "err", res.err)
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
// store with the new pool size. It holds the account's write lock: calls in
// progress finish first, the store is closed so its connections do not
// count against the cap, and new calls wait until the new store is in.
// With onlyIfStale, a limit that is fresh once the lock is held is kept.
func (s *Server) probeAccount(ctx context.Context, acct config.ResolvedAccount, onlyIfStale bool) probeResult {
	res := probeResult{acct: acct}
	res.err = s.stores.exclusive(ctx, acct, func(cur mailstore.Store, replace func() mailstore.Store) error {
		if _, ok := cur.(prober); !ok {
			return errProbeUnsupported
		}
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
		// A fresh store has no connections open, and only the probe uses it.
		p, ok := replace().(prober)
		if !ok {
			return errProbeUnsupported
		}
		n, err := limits.Probe(ctx, p.ProbeDial, s.lim.probeOpts)
		if err != nil {
			// A cancelled probe returns no count, and a failed one no cap.
			return err
		}
		l := limits.Limit{Account: acct.Name, IMAPCap: n, ProbedAt: s.opts.Now().UTC()}
		if err := s.lim.store.Put(ctx, l); err != nil {
			return err
		}
		replace()
		res.limit = l
		res.pool = limits.PoolSize(acct.Pool, &l, acct.Provider.DefaultPool)
		return nil
	})
	return res
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
// reason. The cause can carry the mail server's text, so it stays in the
// daemon's logs.
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

// probeLimits probes accts concurrently, one goroutine per account, and
// records one audit row for each. A failed probe fails only its account.
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
			s.opts.Logger.Warn("connection probe failed", "account", r.acct.Name, "reason", apiReason(r.err), "err", r.err)
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
