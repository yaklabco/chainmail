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
	// OnError, when set, receives the error of every failed worker step and
	// every cache.db failure the prefetcher and the read path log, as
	// EvictorOptions.OnError does, so the daemon can turn the cache off on
	// a full disk at once. Nil drops them after logging.
	OnError func(err error)
	// Started is when the daemon began this run, taken before any
	// supervisor starts. A folder is fresh only once a pass at or after it
	// has synced the folder. Zero means NewAccount's time on Clock.
	Started time.Time
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

	// wmu serializes the account's cache writes: a one-slot semaphore,
	// taken by lock or lockContext and given back by unlock, so the read
	// path can stop waiting for it when its context ends. epoch counts the
	// folder drops made under it: work fetched under an older epoch is
	// discarded instead of written over a drop.
	wmu   chan struct{}
	epoch atomic.Uint64
	// keepTimeout bounds the read path's bookkeeping after a download,
	// keepReadTimeout unless a test shortens it.
	keepTimeout time.Duration
	// inflight is the backfill batch being fetched with wmu released, or
	// nil. The builder notes in it every flags or expunged change it
	// applies to the batch's folder, and commitBatch fetches the batch
	// again when a change hit one of its UIDs: the header it fetched may
	// predate the change. Guarded by wmu.
	inflight *inflightBatch
	// readsMu guards reads, the guards of the read path's downloads now
	// running with wmu released. noteChange marks each flags or expunged
	// change the builder applies in every one, and the read path writes
	// a download back only for messages its guard saw no change of.
	readsMu sync.Mutex
	reads   map[*readGuard]struct{}

	// applied is the builder's acknowledged event ID, -1 until the builder
	// has opened its group.
	applied atomic.Int64
	// reread is whether the builder has read the messages recorded as
	// unreadable again in this run. Only the builder uses it.
	reread bool
	// started is the run's start, AccountOptions.Started or else when
	// NewAccount ran, on the account's clock. A folder is fresh only once
	// a pass of this run has synced it: a sync row from a previous run
	// says nothing of the mail that arrived while the daemon was down.
	started time.Time

	// throttled is the pause the server's quota or bandwidth refusal of a
	// backfill or prefetch fetch starts, until 00:00 UTC.
	throttled throttle

	// errMu guards lastErr, the reason code of the latest failed step. The
	// worker that failed clears it once it succeeds again.
	errMu   sync.Mutex
	lastErr string

	// wake tells an idle backfill that a folder was tracked or reset.
	wake chan struct{}
	// prefetchWake tells an idle prefetcher that a folder's backfill
	// finished, so its window bodies are ready to fetch.
	prefetchWake chan struct{}

	// afterGroup, when set, runs once the builder has opened its group and
	// before it records baselines. Tests set it.
	afterGroup func()
	// beforeKeepWrite, when set, runs in the read path after a downloaded
	// message is charged and before its write takes wmu. Tests set it.
	beforeKeepWrite func()
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

// systemClock is watch.Clock over package time, for options that leave
// Clock nil (AccountOptions, prefetchOptions, EvictorOptions).
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewAccount returns the cache of o.Account. Run starts its workers.
func NewAccount(o AccountOptions) *Account {
	a := &Account{
		o:            o,
		name:         o.Account.Name,
		group:        watch.CacheGroup(o.Account.Name),
		clock:        o.Clock,
		logger:       o.Logger,
		wake:         make(chan struct{}, 1),
		prefetchWake: make(chan struct{}, 1),
		wmu:          make(chan struct{}, 1),

		keepTimeout: keepReadTimeout,
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
	a.started = o.Started
	if a.started.IsZero() {
		a.started = a.clock.Now()
	}
	return a
}

// Run runs the builder, the backfill and the body prefetcher, joined, until
// ctx ends. Body eviction is daemon-wide (Evictor), since the size cap
// covers the whole file. Run returns once its workers have all stopped, and
// at once when the account's cache is disabled.
func (a *Account) Run(ctx context.Context) {
	if !a.settings().Enabled {
		return
	}
	var wg sync.WaitGroup
	wg.Go(func() { a.runBuilder(ctx) })
	wg.Go(func() { a.runBackfill(ctx) })
	wg.Go(func() { a.runPrefetch(ctx) })
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
	case errors.Is(err, mailstore.ErrMalformed):
		return api.ReasonMalformedResponse
	default:
		return api.ReasonInternal
	}
}

// noteErr hands err to OnError, when set.
func (a *Account) noteErr(err error) {
	if err != nil && a.o.OnError != nil {
		a.o.OnError(err)
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

// runPrefetch runs the account's body prefetcher until ctx ends. It shares
// the account's options, including the clock and logger NewAccount
// defaulted, and its throttle, and pauses as the other workers do
// (integration note 12).
func (a *Account) runPrefetch(ctx context.Context) {
	o := a.o
	newPrefetcher(prefetchOptions{
		Account:  a.name,
		Settings: o.Settings,
		MaxSize:  o.MaxSize,
		Store:    o.Store,
		State:    o.State,
		Mail: func(ctx context.Context) (mailstore.Store, func(), error) {
			ms, release, err := o.Mail(ctx)
			if err != nil {
				return nil, nil, err
			}
			return ms, release, nil
		},
		Wake:     a.prefetchWake,
		Throttle: &a.throttled,
		Clock:    o.Clock,
		Logger:   o.Logger,
		OnError:  o.OnError,
	}).run(ctx)
}

// writeEpoch returns the count of folder drops so far, without waiting for
// wmu. Work fetched now is written only while it is unchanged, which the
// writer checks again under wmu, where every drop increments it.
func (a *Account) writeEpoch() uint64 { return a.epoch.Load() }

// lock takes wmu, waiting as long as it takes.
func (a *Account) lock() { a.wmu <- struct{}{} }

// lockContext takes wmu, or gives up with ctx's error when ctx ends
// first, leaving wmu with its holder.
func (a *Account) lockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case a.wmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock gives wmu back. The caller holds it.
func (a *Account) unlock() { <-a.wmu }
