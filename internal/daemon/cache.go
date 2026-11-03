package daemon

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// cacheFile is the cache database's name in Options.CacheDir.
const cacheFile = "cache.db"

// openCache opens cache.db before anything uses it. It opens nothing with
// DisableCache, without a CacheDir, or when no account has the cache
// enabled. A failure is logged and kept in cacheErr: the daemon then runs
// on the server path, as before SP7. cache.Open replaces a file that is
// corrupt or of another schema version itself.
func (s *Server) openCache(ctx context.Context) {
	if s.opts.DisableCache || s.opts.CacheDir == "" {
		return
	}
	if _, ok := s.firstCacheAccount(); !ok {
		s.opts.Logger.Info("the cache is disabled for every account")
		return
	}
	if err := os.MkdirAll(s.opts.CacheDir, 0o700); err != nil {
		s.cacheOpenFailed(err)
		return
	}
	st, err := cache.Open(ctx, filepath.Join(s.opts.CacheDir, cacheFile), cache.OpenOptions{Now: s.opts.Now, Logger: s.opts.Logger})
	if err != nil {
		s.cacheOpenFailed(err)
		return
	}
	s.cache = st
}

// cacheOpenFailed records why cache.db did not open.
func (s *Server) cacheOpenFailed(err error) {
	s.cacheErr = err
	s.opts.Logger.Error("cache.db could not be opened; searches and reads use the mail servers", "err", err)
}

// closeCache closes cache.db, once nothing uses it any more.
func (s *Server) closeCache() {
	if s.cache == nil {
		return
	}
	if err := s.cache.Close(); err != nil {
		s.opts.Logger.Warn("closing cache.db", "err", err)
	}
}

// firstCacheAccount returns the first configured account, in name order,
// with the cache enabled.
func (s *Server) firstCacheAccount() (string, bool) {
	cfg := s.opts.Config
	for _, name := range cfg.AccountNames() {
		if cfg.CacheSettings(name).Enabled {
			return name, true
		}
	}
	return "", false
}

// cacheService implements chainmailv1connect.CacheServiceHandler.
type cacheService struct {
	// UnimplementedCacheServiceHandler answers RebuildCache and EvictCache
	// until their cycles replace it.
	chainmailv1connect.UnimplementedCacheServiceHandler
	s *Server
}

var _ chainmailv1connect.CacheServiceHandler = (*cacheService)(nil)

// GetCacheStatus implements chainmailv1connect.CacheServiceHandler. It
// reports the cache of every account, or of those named, and the file's
// size against max_size. Like GetWatchStatus it is read-only, returns no
// message content and is deliberately not audited.
func (c *cacheService) GetCacheStatus(ctx context.Context, req *connect.Request[chainmailv1.GetCacheStatusRequest]) (*connect.Response[chainmailv1.GetCacheStatusResponse], error) {
	accts, err := c.s.resolveAccounts(req.Msg.GetAccounts())
	if err != nil {
		return nil, toConnectError(err)
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	resp := &chainmailv1.GetCacheStatusResponse{
		MaxSizeBytes: c.s.opts.Config.CacheMaxSize(),
		Accounts:     c.s.cacheStatuses(ctx, accts, true),
	}
	if c.s.cache != nil {
		size, err := c.s.cache.SizeBytes(ctx)
		if err != nil {
			return nil, toConnectError(c.s.cacheFailure(err))
		}
		resp.SizeBytes = size
	}
	return connect.NewResponse(resp), nil
}

// cacheFailure logs a cache error the daemon cannot recover from in the
// call, and returns it with fixed client text.
func (s *Server) cacheFailure(err error) error {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.opts.Logger.Error("cache call failed", "err", err)
	}
	return &fixedTextError{msg: "the cache failed", err: err}
}

// cacheStatuses is cacheStatus for each of accts, in their order.
func (s *Server) cacheStatuses(ctx context.Context, accts []config.ResolvedAccount, withFolders bool) []*chainmailv1.AccountCacheStatus {
	out := make([]*chainmailv1.AccountCacheStatus, 0, len(accts))
	for _, acct := range accts {
		out = append(out, s.cacheStatus(ctx, acct, withFolders))
	}
	return out
}

// cacheStatus reports one account's cache. It never fails: a status the
// worker cannot read is state error with an INTERNAL reason, and the
// cause goes to the log.
func (s *Server) cacheStatus(ctx context.Context, acct config.ResolvedAccount, withFolders bool) *chainmailv1.AccountCacheStatus {
	set := s.opts.Config.CacheSettings(acct.Name)
	out := &chainmailv1.AccountCacheStatus{Account: acct.Name, State: cache.StateDisabled, BudgetBytes: set.DailyDownload}
	ca := s.cacheAccounts[acct.Name]
	switch {
	case !set.Enabled || s.opts.DisableCache:
		return out
	case s.cacheErr != nil:
		out.Enabled, out.State, out.LastError = true, cache.StateError, api.ReasonInternal
		return out
	case ca == nil:
		// Not synced, or watching is off: the cache needs the event log.
		return out
	}
	st, err := ca.Status(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.opts.Logger.Warn("reading an account's cache status", "account", acct.Name, "err", err)
		}
		out.Enabled, out.State, out.LastError = true, cache.StateError, api.ReasonInternal
		return out
	}
	out = accountCacheProto(st, withFolders)
	if s.cache.OverCap() && (out.State == cache.StateReady || out.State == cache.StatePrefetching) {
		out.State = cache.StateOverCap
	}
	return out
}

// accountCacheProto builds the API form of a worker's status, with its
// folders when withFolders is set.
func accountCacheProto(st cache.AccountStatus, withFolders bool) *chainmailv1.AccountCacheStatus {
	out := &chainmailv1.AccountCacheStatus{
		Account: st.Account, Enabled: st.Enabled, State: st.State,
		BudgetUsedBytes: st.BudgetUsed, BudgetBytes: st.BudgetBytes, LastError: st.LastError,
	}
	if withFolders {
		out.Folders = make([]*chainmailv1.FolderCacheStatus, 0, len(st.Folders))
		for _, f := range st.Folders {
			out.Folders = append(out.Folders, folderCacheProto(f))
		}
	}
	return out
}

// folderCacheProto builds the API form of one folder's cache status.
// FolderStatus embeds Progress and FolderStats, which both name the folder.
func folderCacheProto(f cache.FolderStatus) *chainmailv1.FolderCacheStatus {
	return &chainmailv1.FolderCacheStatus{
		Folder:         cmp.Or(f.Progress.Folder, f.FolderStats.Folder),
		State:          f.State,
		Headers:        f.Headers,
		BackfillDone:   f.BackfillDone,
		BaselineUid:    f.BaselineUIDNext,
		BackfillLowUid: f.BackfillLowUID,
		Bodies:         f.Bodies,
		BodyBytes:      f.BodyBytes,
		WindowMessages: f.WindowMessages,
		WindowBodies:   f.WindowBodies,
		WindowDone:     f.WindowDone,
		BodyFailures:   f.BodyFailures,
		LastAppliedAt:  timestamp(f.LastAppliedAt),
	}
}

// startCacheAccounts starts a cache worker for each of accts with the
// cache enabled, on wg, and records them in cacheAccounts. startWatch calls
// it with the supervisors' context and WaitGroup, so the workers stop with
// the supervisors, before drain closes the stores. It does no IO.
func (s *Server) startCacheAccounts(ctx context.Context, accts []config.ResolvedAccount, wg *sync.WaitGroup) {
	if s.cache == nil {
		return
	}
	cfg := s.opts.Config
	workers := make(map[string]*cache.Account, len(accts))
	var names []string
	for _, acct := range accts {
		if !cfg.CacheSettings(acct.Name).Enabled {
			continue
		}
		ca := cache.NewAccount(s.cacheAccountOptions(acct))
		workers[acct.Name] = ca
		names = append(names, acct.Name)
		wg.Go(func() { ca.Run(ctx) })
	}
	s.cacheAccounts = workers
	if len(names) > 0 {
		s.opts.Logger.Info("caching accounts", "accounts", names)
	}
}

// cacheAccountOptions wires acct's cache worker to the daemon. Like the
// supervisor, the worker takes the account entry's read lock through
// acquire, one batch at a time, so a probe's replace gets through between
// batches. The entry is taken once, here, for the reason
// supervisorOptions gives.
func (s *Server) cacheAccountOptions(acct config.ResolvedAccount) cache.AccountOptions {
	e := s.stores.entry(acct)
	cfg := s.opts.Config
	name := acct.Name
	return cache.AccountOptions{
		Account:  acct,
		Settings: func() config.CacheSettings { return cfg.CacheSettings(name) },
		MaxSize:  cfg.CacheMaxSize,
		Store:    s.cache,
		Log:      s.watchLog,
		State:    s.watchState,
		Mail: func(ctx context.Context) (cache.MailStore, func(), error) {
			st, release, err := e.acquire(ctx)
			if err != nil {
				return nil, nil, err
			}
			ms, ok := st.(cache.MailStore)
			if !ok {
				release()
				return nil, nil, mailstore.ErrUnsupported
			}
			return ms, release, nil
		},
		Logger: s.opts.Logger,
	}
}
