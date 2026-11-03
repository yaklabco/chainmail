package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// eventTrimInterval is how often the event log is trimmed.
const eventTrimInterval = time.Hour

// supervisorSet runs one watch.Supervisor per synced account while the
// daemon serves.
type supervisorSet struct {
	mu sync.Mutex
	// cancel ends the supervisors' context. Nil until startWatch starts one.
	cancel context.CancelFunc
	// stopped is set by stopWatch, after which startWatch starts nothing.
	stopped bool
	// accounts are the accounts with a supervisor, in name order.
	accounts []string
	wg       sync.WaitGroup
}

// watchedAccounts are the accounts to supervise: every account with sync
// enabled, in name order, or none with DisableWatch.
func (s *Server) watchedAccounts() []config.ResolvedAccount {
	if s.opts.DisableWatch {
		return nil
	}
	cfg := s.opts.Config
	var out []config.ResolvedAccount
	for _, name := range cfg.AccountNames() {
		acct, err := cfg.Account(name)
		if err != nil {
			// A validated config resolves every account it names.
			s.opts.Logger.Warn("not watching an account that does not resolve", "account", name, "err", err)
			continue
		}
		if acct.SyncEnabled {
			out = append(out, acct)
		}
	}
	return out
}

// startWatch starts a supervisor for each watched account, and a cache
// worker for each of them with the cache enabled. They run until ctx ends
// or stopWatch runs. It does no IO: each supervisor and worker opens its
// account's store from its own goroutine, and the store's first use only
// builds it and hands any connection probe to the background
// (probeIfStale), so a slow or unreachable mail server never holds up
// Serve.
func (s *Server) startWatch(ctx context.Context) {
	accts := s.watchedAccounts()
	set := &s.supervisors
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.stopped || len(accts) == 0 {
		return
	}
	ctx, set.cancel = context.WithCancel(ctx)
	settings := s.opts.Config.WatchSettings()
	for _, acct := range accts {
		sup := watch.NewSupervisor(s.supervisorOptions(acct, settings))
		set.accounts = append(set.accounts, acct.Name)
		set.wg.Go(func() { sup.Run(ctx) })
	}
	s.opts.Logger.Info("watching accounts", "accounts", set.accounts)
	s.startCacheAccounts(ctx, accts, &set.wg)
}

// stopWatch cancels the supervisors and the cache workers and waits for
// them to close their connections, which each bounds at
// watch.ShutdownLogout. startWatch starts nothing after it. drain runs it
// before closing the stores, so no watcher or worker still holds a store
// being closed.
func (s *Server) stopWatch() {
	set := &s.supervisors
	set.mu.Lock()
	set.stopped = true
	if set.cancel != nil {
		set.cancel()
	}
	set.mu.Unlock()
	set.wg.Wait()
}

// supervisorOptions wires acct's supervisor to the daemon.
//
// The supervisor needs the store itself, to assert mailstore.Watcher on it,
// so it takes the account entry's read lock directly through acquire rather
// than through an accountStore handle, which wraps every call. It holds the
// lock only across one dial or pooled call, so a probe's replace gets
// through between them. The entry is taken once, here: closeAll forgets
// entries, and a lookup after it would build a fresh store for a daemon
// that is shutting down, where this one refuses with errStoresClosed.
func (s *Server) supervisorOptions(acct config.ResolvedAccount, settings config.WatchSettings) watch.SupervisorOptions {
	e := s.stores.entry(acct)
	return watch.SupervisorOptions{
		Account:      acct,
		Settings:     settings,
		Store:        e.acquire,
		Engine:       s.engine,
		Log:          s.watchLog,
		State:        s.watchState,
		AuthRequired: s.authRequired,
		SweepFloor:   s.opts.WatchSweepFloor,
		Logger:       s.opts.Logger,
	}
}

// authRequired reports whether account needs a human, and what to run, for
// its supervisor. A state that cannot be read counts as not required: the
// next connection attempt then reports what is wrong.
func (s *Server) authRequired(account string) (bool, string) {
	st, err := s.auth.State(account)
	if err != nil {
		s.opts.Logger.Warn("reading the auth state of a watched account", "account", account, "reason", apiReason(err))
		return false, ""
	}
	return st.Required, st.Detail
}
