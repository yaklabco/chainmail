package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
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
	probe, ok := s.firstCacheAccount()
	if !ok {
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
	s.cacheProbeAccount = probe
	cfg := s.opts.Config
	s.cacheEvictor = cache.NewEvictor(cache.EvictorOptions{
		Store:     st,
		Window:    func(account string) time.Duration { return cfg.CacheSettings(account).PrefetchBodies },
		Retention: func(account string) time.Duration { return cfg.CacheSettings(account).ReadRetention },
		MaxSize:   cfg.CacheMaxSize,
		Clock:     s.cacheClock,
		Logger:    s.opts.Logger,
		OnError:   s.noteCacheErr,
	})
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
	s.noteCacheErr(err)
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
	case s.cacheErr != nil || s.cacheDown.Load():
		out.Enabled, out.State, out.LastError = true, cache.StateError, api.ReasonInternal
		return out
	case ca == nil:
		// Not synced, or watching is off: the cache needs the event log.
		return out
	}
	st, err := ca.Status(ctx)
	if err != nil {
		s.noteCacheErr(err)
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
		Clock:  s.cacheClock,
		Logger: s.opts.Logger,
	}
}

// allFolders is the audit target of a rebuild of every folder.
const allFolders = "folder=(all)"

// errCacheOff refuses a cache action on an account that has no cache worker.
var errCacheOff = &argError{
	msg:  "the account's cache is not running",
	hint: "the cache needs [cache] enabled and sync on the account; run: chainmail cache status",
}

// RebuildCache implements chainmailv1connect.CacheServiceHandler. It drops
// the cached rows of one folder, or of every folder the account's cache
// tracks, and the backfill fills them again. It records cache.rebuild with
// the folder, or folder=(all), and how many folders it dropped.
func (c *cacheService) RebuildCache(ctx context.Context, req *connect.Request[chainmailv1.RebuildCacheRequest]) (*connect.Response[chainmailv1.RebuildCacheResponse], error) {
	caller, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	account, rebuildErr := accountTarget(req.Msg.GetAccount())
	folder := req.Msg.GetFolder()
	target := allFolders
	if folder != "" {
		target = folderTarget(folder)
	}
	var folders []string
	if rebuildErr == nil {
		folders, rebuildErr = c.s.rebuildCache(ctx, account, folder)
	}
	entry := audit.Entry{Account: account, Action: "cache.rebuild", Targets: []string{target, tally("rebuilt", len(folders))}}
	if err := c.s.record(ctx, caller, entry, rebuildErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.RebuildCacheResponse{Folders: folders}), nil
}

// rebuildCache rebuilds folder, or every tracked folder when it is "", of
// account's cache. A folder the cache does not track is the worker's
// *mailstore.FolderNotFoundError, NOT_FOUND.
func (s *Server) rebuildCache(ctx context.Context, account, folder string) ([]string, error) {
	acct, err := s.opts.Config.Account(account)
	if err != nil {
		return nil, err
	}
	ca := s.cacheAccounts[acct.Name]
	if ca == nil {
		return nil, errCacheOff
	}
	if folder != "" {
		if err := checkName("folder", folder); err != nil {
			return nil, err
		}
	}
	folders, err := ca.Rebuild(ctx, folder)
	if _, ok := errors.AsType[*mailstore.FolderNotFoundError](err); ok {
		return nil, err
	}
	if err != nil {
		return nil, s.cacheFailure(err)
	}
	s.opts.Logger.Info("cache rebuild", "account", acct.Name, "folders", len(folders))
	return folders, nil
}

// errCacheNotOpen refuses an eviction when cache.db is not open.
var errCacheNotOpen = &argError{
	msg:  "the cache is not running",
	hint: "the cache is disabled in the config, or cache.db did not open; the daemon log says which",
}

// EvictCache implements chainmailv1connect.CacheServiceHandler. It runs a
// body eviction sweep now and records cache.evict with the bodies it
// removed and the bytes they held.
func (c *cacheService) EvictCache(ctx context.Context, req *connect.Request[chainmailv1.EvictCacheRequest]) (*connect.Response[chainmailv1.EvictCacheResponse], error) {
	caller, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	var res cache.EvictResult
	evictErr := error(errCacheNotOpen)
	if c.s.cacheEvictor != nil {
		// Sweep logs its own failures.
		if res, evictErr = c.s.cacheEvictor.Sweep(ctx); evictErr != nil {
			evictErr = &fixedTextError{msg: "the cache failed", err: evictErr}
		}
	}
	entry := audit.Entry{Action: "cache.evict", Targets: []string{
		fmt.Sprintf("bodies=%d", res.Bodies), fmt.Sprintf("freed=%d", res.FreedBytes),
	}}
	if err := c.s.record(ctx, caller, entry, evictErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.EvictCacheResponse{BodiesEvicted: res.Bodies, FreedBytes: res.FreedBytes}), nil
}

const (
	// cacheHousekeepInterval is how often the write probe and the ref
	// recovery pruning run.
	cacheHousekeepInterval = time.Hour
	// cacheBudgetDay formats a budget day, a UTC date.
	cacheBudgetDay = time.DateOnly
)

// cacheAccount returns the account's cache worker, for routing and reads,
// or nil when the account has none or the cache is down. A nil result
// means: use the mail server.
func (s *Server) cacheAccount(name string) *cache.Account {
	if s.cacheDown.Load() {
		return nil
	}
	return s.cacheAccounts[name]
}

// noteCacheErr turns the cache off when err says the disk holding cache.db
// is full. The housekeeping turns it back on once a write succeeds.
func (s *Server) noteCacheErr(err error) {
	if errors.Is(err, cache.ErrDiskFull) && s.cacheDown.CompareAndSwap(false, true) {
		s.opts.Logger.Warn("the disk holding cache.db is full; searches and reads use the mail servers until a write succeeds, retried hourly")
	}
}

// probeCache writes to cache.db: it adds 0 bytes to today's budget of the
// first account with the cache enabled, a row that account has anyway.
func (s *Server) probeCache(ctx context.Context) error {
	if s.cache == nil {
		return nil
	}
	_, err := s.cache.AddBudget(ctx, s.cacheProbeAccount, s.cacheClock.Now().UTC().Format(cacheBudgetDay), 0)
	return err
}

// runCacheHousekeeping runs cacheHousekeep at once, then every
// cacheHousekeepInterval on the cache clock, until ctx ends. The first run
// is at start so a daemon restarted more often than hourly still probes a
// full disk and prunes the recovery rows.
func (s *Server) runCacheHousekeeping(ctx context.Context) {
	for {
		s.cacheHousekeep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.cacheClock.After(cacheHousekeepInterval):
		}
	}
}

// cacheHousekeep probes a write, turning the cache off on a full disk and
// back on once a write works, then, with the cache on, prunes the
// Message-ID recovery rows older than the longest read_retention.
func (s *Server) cacheHousekeep(ctx context.Context) {
	err := s.cacheProbe(ctx)
	switch {
	case err == nil:
		if s.cacheDown.CompareAndSwap(true, false) {
			s.opts.Logger.Info("cache.db accepts writes again; the cache is back on")
		}
	case ctx.Err() != nil:
		return
	default:
		s.noteCacheErr(err)
		if !errors.Is(err, cache.ErrDiskFull) {
			s.opts.Logger.Warn("the cache write probe failed", "err", err)
		}
	}
	if s.cacheDown.Load() || s.cache == nil {
		return
	}
	if err := s.cache.PruneRecovery(ctx, s.cacheClock.Now().Add(-s.maxReadRetention())); err != nil && ctx.Err() == nil {
		s.noteCacheErr(err)
		s.opts.Logger.Warn("pruning cache ref recovery", "err", err)
	}
}

// maxReadRetention is the longest read_retention of any account, so no
// account's recovery rows go before its own retention.
func (s *Server) maxReadRetention() time.Duration {
	var longest time.Duration
	cfg := s.opts.Config
	for _, name := range cfg.AccountNames() {
		longest = max(longest, cfg.CacheSettings(name).ReadRetention)
	}
	return longest
}

// daemonClock is the cache's watch.Clock: Now is Options.Now, so budget
// days, the eviction window and read retention agree with the times
// cache.db stamps, and After is time.After.
type daemonClock struct {
	now func() time.Time
}

func (c daemonClock) Now() time.Time { return c.now() }

func (daemonClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
