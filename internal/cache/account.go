package cache

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// MailStore is what the cache needs of an account's mail store: the read
// methods of mailstore.Store, header fetches and search scopes.
type MailStore interface {
	mailstore.Store
	mailstore.HeaderFetcher
	mailstore.Scoper
}

// AccountOptions configure an Account.
type AccountOptions struct {
	// Account is the mail account the cache follows.
	Account config.ResolvedAccount
	// Settings returns the account's cache settings. It is read at every
	// use, so a changed setting applies from the next batch.
	Settings func() config.CacheSettings
	// MaxSize returns the size cap of the whole cache file, in bytes.
	MaxSize func() int64
	// Store is the cache database every account shares.
	Store *Store
	// Log is the daemon's event log. The builder reads it through the
	// account's internal group.
	Log *watch.Log
	// State is the daemon's sync state: which folders the account syncs,
	// their checkpoints and their status.
	State *watch.State
	// Mail returns the account's store for one batch, as
	// SupervisorOptions.Store does, with a release the cache calls when
	// the batch ends.
	Mail func(ctx context.Context) (MailStore, func(), error)
	// Clock times pauses, backoffs and the budget day. Nil means the
	// system clock.
	Clock watch.Clock
	// Logger receives progress and failures as fixed reason codes, never
	// message content or server text. Nil discards.
	Logger *slog.Logger
}

// Account keeps one mail account's part of the cache. Its builder applies
// the account's events from the log, read through the internal group
// watch.CacheGroup(account), and its backfill fetches the headers of mail
// that was in a folder before the cache tracked it. It reports freshness
// and status, rebuilds on request, and hands out a View for reads. Its
// methods are safe for concurrent use.
type Account struct {
	o      AccountOptions
	name   string
	group  string
	clock  watch.Clock
	logger *slog.Logger

	// wmu serializes the account's cache writes. epoch counts the folder
	// drops made under it: work fetched under an older epoch is discarded
	// instead of written over a drop.
	wmu   sync.Mutex
	epoch uint64
	// inflight is the backfill batch being fetched with wmu released, or
	// nil. The builder notes in it every flags or expunged change it
	// applies to the batch's folder, and commitBatch fetches the batch
	// again when a change hit one of its UIDs: the header it fetched may
	// predate the change. Guarded by wmu.
	inflight *inflightBatch

	// applied is the builder's acknowledged event ID, -1 until the builder
	// has opened its group.
	applied atomic.Int64
	// overCap is set by the evictor while headers alone exceed MaxSize.
	overCap atomic.Bool

	// errMu guards lastErr, the reason code of the latest failed step. The
	// worker that failed clears it once it succeeds again.
	errMu   sync.Mutex
	lastErr string

	// wake tells an idle backfill that a folder was tracked or reset.
	wake chan struct{}

	// afterGroup, when set, runs once the builder has opened its group and
	// before it records baselines. Tests set it.
	afterGroup func()
}

// Work timing and accounting, from spec sections 4 and 8 and amendment 14.
const (
	// headerBudgetBytes is what one fetched header counts against the
	// daily download budget, since the store does not report wire bytes.
	headerBudgetBytes = 2048
	// pauseRecheck is the shortest wait before work paused by a folder's
	// sync state checks it again.
	pauseRecheck = time.Second
	// busyRetryFloor is the shortest wait before work retries after
	// mailstore.ErrBusy, so a backfill_pause of 0 never spins.
	busyRetryFloor = 50 * time.Millisecond
	// workRetryBase and workRetryMax bound the backoff after a failed
	// builder or backfill step: 1s doubling to 5m.
	workRetryBase = time.Second
	workRetryMax  = 5 * time.Minute
)

// systemClock is watch.Clock over package time, which a nil
// AccountOptions.Clock means.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewAccount returns the cache of o.Account. Run starts its workers.
func NewAccount(o AccountOptions) *Account {
	a := &Account{
		o:      o,
		name:   o.Account.Name,
		group:  watch.CacheGroup(o.Account.Name),
		clock:  o.Clock,
		logger: o.Logger,
		wake:   make(chan struct{}, 1),
	}
	if a.clock == nil {
		a.clock = systemClock{}
	}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}
	a.logger = a.logger.With("account", a.name)
	// The View methods (T11, T12) read a.o.Clock and a.o.Logger, so they
	// carry the same defaults.
	a.o.Clock, a.o.Logger = a.clock, a.logger
	a.applied.Store(-1)
	return a
}

// Run runs the account's cache workers until ctx ends and returns once they
// have all stopped. It returns at once when the account's cache is disabled.
func (a *Account) Run(ctx context.Context) {
	if !a.settings().Enabled {
		return
	}
	var wg sync.WaitGroup
	wg.Go(func() { a.runBuilder(ctx) })
	wg.Go(func() { a.runBackfill(ctx) })
	wg.Wait()
}

// settings reads the account's cache settings now.
func (a *Account) settings() config.CacheSettings { return a.o.Settings() }

// day is the current budget day, the UTC date.
func (a *Account) day() string { return a.clock.Now().UTC().Format(time.DateOnly) }

// syncRows returns the account's sync_folders rows that have a
// UIDVALIDITY, ordered by folder: the folders the cache tracks
// (amendment 8).
func (a *Account) syncRows(ctx context.Context) ([]watch.FolderRow, error) {
	all, err := a.o.State.Folders(ctx)
	if err != nil {
		return nil, err
	}
	var out []watch.FolderRow
	for _, r := range all {
		if r.Account == a.name && r.UIDValidity != 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// Folders returns the folders the cache tracks: the account's sync_folders
// rows with a UIDVALIDITY, from watch.State.
func (a *Account) Folders(ctx context.Context) ([]string, error) {
	rows, err := a.syncRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Folder
	}
	return out, nil
}

// progressMap returns the account's cache progress rows by folder.
func (a *Account) progressMap(ctx context.Context) (map[string]Progress, error) {
	all, err := a.o.Store.AllProgress(ctx, a.name)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Progress, len(all))
	for _, p := range all {
		out[p.Folder] = p
	}
	return out, nil
}

// workBackoff is the wait after n failed steps in a row (0-based): 1s
// doubling, capped at 5 minutes. A negative n counts as 0.
func workBackoff(n int) time.Duration {
	d := workRetryBase
	for i := 0; i < n && d < workRetryMax; i++ {
		d *= 2
	}
	return min(d, workRetryMax)
}

// untilUTCMidnight is how long from now until the next 00:00 UTC, when a
// spent budget renews.
func untilUTCMidnight(now time.Time) time.Duration {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(u)
}

// reasonCode is the api reason code for a failed step: what status and
// logs report, since error text may hold server text.
func reasonCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return api.ReasonTimeout
	case errors.Is(err, auth.ErrAuthRequired):
		return api.ReasonAuthRequired
	case errors.Is(err, auth.ErrRejected):
		return api.ReasonAuthRejected
	case errors.Is(err, mailstore.ErrStaleRef):
		return api.ReasonStaleRef
	case errors.Is(err, mailstore.ErrNotFound):
		return api.ReasonNotFound
	case errors.Is(err, mailstore.ErrUnavailable), errors.Is(err, mailstore.ErrBusy):
		return api.ReasonServerUnavailable
	default:
		return api.ReasonInternal
	}
}

func (a *Account) setError(reason string) {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	a.lastErr = reason
}

func (a *Account) clearError() { a.setError("") }

func (a *Account) lastError() string {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	return a.lastErr
}
