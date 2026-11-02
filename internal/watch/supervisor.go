package watch

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// ReasonFolderNotFound is the status detail and sync_folders last_error of
// a synced folder the account does not have.
const ReasonFolderNotFound = "FOLDER_NOT_FOUND"

// Pass modes, as PassOptions.Mode and the sync_folders mode column record them.
const (
	passModeIdle = "idle"
	passModePoll = "poll"
)

// Fallbacks for settings left zero. config.Parse always fills them; these
// only keep a hand-built SupervisorOptions from sweeping or polling in a
// tight loop.
const (
	fallbackExpungeSweep     = time.Hour
	fallbackIdleFallbackPoll = 30 * time.Second
	fallbackSyncPoll         = 5 * time.Minute
)

// idleGrace is how long past an IDLE's limit the supervisor waits before it
// cancels an IDLE that is still running. A store ends IDLE at the limit it is
// given, so the cancel is a backstop for a stuck connection, and the way a
// test clock, which the store cannot see, ends an IDLE.
const idleGrace = 5 * time.Second

// errNoWatcher means the account's store cannot watch or sync folders.
var errNoWatcher = errors.New("the account's store does not implement mailstore.Watcher")

// SupervisorOptions configure a Supervisor.
type SupervisorOptions struct {
	// Account is the account to watch and sync.
	Account config.ResolvedAccount
	// Settings are the resolved [watch] values.
	Settings config.WatchSettings
	// SweepFloor is the shortest time from one sweep to the next one a
	// server EXPUNGE asks for. 0 means ExpungeSweepFloor. The daemon passes
	// a short floor through for integration tests on Dovecot.
	SweepFloor time.Duration
	// Store opens the account's store and returns its release. The
	// supervisor releases it as soon as it has opened a watch connection
	// or finished a pooled call, never holding it while idle.
	Store func(ctx context.Context) (mailstore.Store, func(), error)
	// Engine runs every sync pass.
	Engine *Engine
	// Log receives status events.
	Log *Log
	// State holds the sync_folders rows.
	State *State
	// AuthRequired reports whether the account needs a human, and the hint.
	AuthRequired func(account string) (bool, string)
	// Clock is what the supervisor waits on. Nil means RealClock.
	Clock Clock
	// Jitter returns a value in [0, 1). Nil means math/rand/v2.
	Jitter func() float64
	// Logger gets one line per failure, without error text. Nil discards.
	Logger *slog.Logger
}

// Supervisor watches and syncs one account: one watcher loop per watched
// folder, each on its own connection, and one poller for the synced folders
// that are not watched. Spec section 5 describes its failure handling.
type Supervisor struct {
	o          SupervisorOptions
	clock      Clock
	jitter     func() float64
	logger     *slog.Logger
	settings   config.WatchSettings
	syncPoll   time.Duration
	sweepFloor time.Duration

	// mu guards states, the last status recorded per folder.
	mu     sync.Mutex
	states map[string]string
}

// NewSupervisor returns a supervisor for o.Account. A nil Clock, Jitter,
// Logger or AuthRequired gets the system clock, math/rand/v2, a discarding
// logger and "never required".
func NewSupervisor(o SupervisorOptions) *Supervisor {
	s := &Supervisor{
		o: o, clock: o.Clock, jitter: o.Jitter, logger: o.Logger,
		settings: o.Settings, syncPoll: o.Account.SyncPoll, states: map[string]string{},
	}
	if s.clock == nil {
		s.clock = RealClock
	}
	if s.jitter == nil {
		s.jitter = rand.Float64
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	s.logger = s.logger.With("account", o.Account.Name)
	if s.o.AuthRequired == nil {
		s.o.AuthRequired = func(string) (bool, string) { return false, "" }
	}
	if s.settings.ExpungeSweep <= 0 {
		s.settings.ExpungeSweep = fallbackExpungeSweep
	}
	if s.settings.IdleFallbackPoll <= 0 {
		s.settings.IdleFallbackPoll = fallbackIdleFallbackPoll
	}
	if s.syncPoll <= 0 {
		s.syncPoll = fallbackSyncPoll
	}
	s.sweepFloor = o.SweepFloor
	if s.sweepFloor <= 0 {
		s.sweepFloor = ExpungeSweepFloor
	}
	return s
}

// Run watches and syncs until ctx ends, then closes every connection. It
// returns at once when the account has sync disabled.
func (s *Supervisor) Run(ctx context.Context) {
	if !s.o.Account.SyncEnabled {
		return
	}
	var wg sync.WaitGroup
	if s.o.Account.Provider.Gmail {
		// One watcher on the \All folder covers every Gmail label, so a
		// Gmail account polls nothing.
		wg.Go(func() { s.watchLoop(ctx, "") })
	} else {
		for _, folder := range s.o.Account.Watch {
			wg.Go(func() { s.watchLoop(ctx, folder) })
		}
		if polled := polledFolders(s.o.Account); len(polled) > 0 {
			wg.Go(func() { s.pollLoop(ctx, polled) })
		}
	}
	wg.Wait()
}

// watcher is one watched folder's loop state. Only its own goroutine uses it.
type watcher struct {
	s *Supervisor
	// folder is "" on Gmail until the \All folder is resolved.
	folder string
	// mode is passModeIdle, or passModePoll on a server without IDLE.
	mode string
	// lastSweep is when the last sweep pass ran. expungeAt is when an
	// EXPUNGE asked for the next one, zero when none is pending.
	lastSweep time.Time
	expungeAt time.Time
}

// watchLoop keeps one folder watched until ctx ends. folder is "" on Gmail,
// which resolves its \All folder before each connection that lacks it.
// Auth failures park the loop, a missing folder retries every sync.poll,
// and anything else reconnects with backoff, reset by a successful pass.
func (s *Supervisor) watchLoop(ctx context.Context, folder string) {
	w := &watcher{s: s, folder: folder, mode: passModeIdle}
	failures := 0
	for {
		if !s.waitAuth(ctx, w.folder, w.mode) {
			return
		}
		healthy, err := w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			failures = 0
		}
		var wait time.Duration
		var notFound *mailstore.FolderNotFoundError
		switch {
		case errors.Is(err, errNoWatcher):
			s.logger.Error("the account's store cannot watch folders", "folder", w.folder)
			return
		case authFailure(err):
			s.logger.Warn("watching is parked until the account is authenticated", "folder", w.folder, "reason", failureReason(err))
			s.transition(ctx, w.folder, w.mode, StateAuthRequired, s.authHint(err), api.ReasonAuthRequired)
			wait = AuthRecheck
		case errors.As(err, &notFound):
			s.logger.Warn("a watched folder does not exist", "folder", w.folder)
			s.transition(ctx, w.folder, w.mode, StateError, ReasonFolderNotFound, ReasonFolderNotFound)
			if s.o.Account.Provider.Gmail {
				w.folder = "" // resolve the \All folder again
			}
			wait = s.syncPoll
		default:
			s.logger.Warn("watch connection failed", "folder", w.folder, "reason", failureReason(err))
			s.transition(ctx, w.folder, w.mode, StateReconnecting, "", api.ReasonServerUnavailable)
			wait = Backoff(failures, s.jitter())
			failures++
		}
		if !s.sleep(ctx, wait) {
			return
		}
	}
}

// waitAuth parks while the account needs a human: it records auth_required
// once, then asks again every AuthRecheck. It reports false when ctx ends.
func (s *Supervisor) waitAuth(ctx context.Context, folder, mode string) bool {
	for {
		need, hint := s.o.AuthRequired(s.o.Account.Name)
		if !need {
			return true
		}
		s.transition(ctx, folder, mode, StateAuthRequired, hint, api.ReasonAuthRequired)
		if !s.sleep(ctx, AuthRecheck) {
			return false
		}
	}
}

// authHint is what to run to authenticate the account: the auth state's
// hint, else the one err carries. Both are written by package auth, never
// by a server.
func (s *Supervisor) authHint(err error) string {
	if need, hint := s.o.AuthRequired(s.o.Account.Name); need && hint != "" {
		return hint
	}
	var required *auth.RequiredError
	if errors.As(err, &required) {
		return required.Hint
	}
	var rejected *auth.RejectedError
	if errors.As(err, &rejected) {
		return rejected.Hint
	}
	return ""
}

// authFailure reports whether err needs a human rather than a retry.
func authFailure(err error) bool {
	return errors.Is(err, auth.ErrAuthRequired) || errors.Is(err, auth.ErrRejected)
}

// polledFolders are the synced folders that are not watched, in config order.
func polledFolders(a config.ResolvedAccount) []string {
	var out []string
	for _, f := range a.SyncFolders {
		if !slices.Contains(a.Watch, f) {
			out = append(out, f)
		}
	}
	return out
}

// pollLoop checks folders every sync.poll until ctx ends, starting at once
// so they catch up from their checkpoints. A folder's failure stays its
// own, except auth, which parks every folder.
func (s *Supervisor) pollLoop(ctx context.Context, folders []string) {
	lastSweep := make(map[string]time.Time, len(folders))
	for {
		wait := s.syncPoll
		if need, hint := s.o.AuthRequired(s.o.Account.Name); need {
			s.parkAll(ctx, folders, hint)
			wait = AuthRecheck
		} else {
			for _, f := range folders {
				err := s.pollFolder(ctx, f, lastSweep)
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, errNoWatcher) {
					s.logger.Error("the account's store cannot sync folders")
					return
				}
				if authFailure(err) {
					s.logger.Warn("polling is parked until the account is authenticated", "reason", failureReason(err))
					s.parkAll(ctx, folders, s.authHint(err))
					wait = AuthRecheck
					break
				}
			}
		}
		if !s.sleep(ctx, wait) {
			return
		}
	}
}

// parkAll records every polled folder as needing a human.
func (s *Supervisor) parkAll(ctx context.Context, folders []string, hint string) {
	for _, f := range folders {
		s.transition(ctx, f, passModePoll, StateAuthRequired, hint, api.ReasonAuthRequired)
	}
}

// pollFolder polls one folder and records its status. A missing folder and
// other failures stay with the folder; the error is returned so pollLoop
// can stop for auth or a store that cannot sync. The first poll of a folder
// sweeps, then one every ExpungeSweep.
func (s *Supervisor) pollFolder(ctx context.Context, folder string, lastSweep map[string]time.Time) error {
	sweep := !s.clock.Now().Before(lastSweep[folder].Add(s.settings.ExpungeSweep))
	res, err := s.pollPass(ctx, folder, sweep)
	if err == nil {
		if sweep || res.Baselined {
			lastSweep[folder] = s.clock.Now()
		}
		s.settled(ctx, folder, passModePoll, res)
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, errNoWatcher) || authFailure(err) {
		return err // pollLoop handles these for every folder at once
	}
	var notFound *mailstore.FolderNotFoundError
	if errors.As(err, &notFound) {
		s.transition(ctx, folder, passModePoll, StateError, ReasonFolderNotFound, ReasonFolderNotFound)
	} else {
		s.logger.Warn("polling a folder failed", "folder", folder, "reason", failureReason(err))
	}
	return err
}

// pollPass runs STATUS on folder and then a pass when a sweep is due, the
// folder has no CONDSTORE or no checkpoint, or its UIDVALIDITY, UIDNEXT or
// HIGHESTMODSEQ moved. Each call holds the store only while it runs.
func (s *Supervisor) pollPass(ctx context.Context, folder string, sweep bool) (PassResult, error) {
	w, release, err := s.watcherStore(ctx)
	if err != nil {
		return PassResult{}, err
	}
	st, err := w.FolderState(ctx, folder)
	release()
	if err != nil {
		return PassResult{}, err
	}
	if !sweep && st.CondStore {
		row, ok, rerr := s.o.State.Folder(ctx, s.o.Account.Name, folder)
		if rerr != nil {
			return PassResult{}, rerr
		}
		if ok && row.UIDValidity == st.UIDValidity && row.UIDNext == st.UIDNext && row.HighestModSeq == st.HighestModSeq {
			return PassResult{}, nil
		}
	}
	w, release, err = s.watcherStore(ctx)
	if err != nil {
		return PassResult{}, err
	}
	defer release()
	return s.o.Engine.Pass(ctx, s.o.Account.Name, folder, StoreSource{W: w, Folder: folder}, PassOptions{Sweep: sweep, Mode: passModePoll})
}

// session runs one connection on the folder: open, sync, then IDLE or poll
// and sync after every wake, until an error or the end of ctx. healthy
// reports whether a pass succeeded on it, which ends an outage.
func (w *watcher) session(ctx context.Context) (healthy bool, err error) {
	if w.folder == "" {
		if w.folder, err = w.s.resolveAll(ctx); err != nil {
			return false, err
		}
	}
	conn, err := w.s.openWatch(ctx, w.folder)
	if err != nil {
		return false, err
	}
	defer w.s.closeConn(conn, w.folder)
	w.mode = passModePoll
	if conn.IdleSupported() {
		w.mode = passModeIdle
	}
	sweep := true // the first pass after a connect always sweeps
	for {
		if err = w.pass(ctx, conn, sweep); err != nil {
			return healthy, err
		}
		healthy = true
		if sweep, err = w.wait(ctx, conn); err != nil {
			return healthy, err
		}
	}
}

// pass runs one engine pass on conn and records the folder as watching or
// polling. A sweep, or a baseline, restarts the sweep schedule.
func (w *watcher) pass(ctx context.Context, conn mailstore.WatchConn, sweep bool) error {
	res, err := w.s.o.Engine.Pass(ctx, w.s.o.Account.Name, w.folder, conn, PassOptions{Sweep: sweep, Mode: w.mode})
	if err != nil {
		return err
	}
	if sweep || res.Baselined {
		w.lastSweep = w.s.clock.Now()
		w.expungeAt = time.Time{}
	}
	w.s.settled(ctx, w.folder, w.mode, res)
	return nil
}

// wait blocks until the folder needs another pass and reports whether that
// pass must sweep. With IDLE it waits for the server, the IdleRestart or a
// due sweep; without, for idle_fallback_poll or a due sweep.
func (w *watcher) wait(ctx context.Context, conn mailstore.WatchConn) (sweep bool, err error) {
	if w.sweepDue() {
		return true, nil
	}
	limit := w.nextSweep().Sub(w.s.clock.Now())
	if !conn.IdleSupported() {
		if !w.s.sleep(ctx, min(limit, w.s.settings.IdleFallbackPoll)) {
			return false, ctx.Err()
		}
		return w.sweepDue(), nil
	}
	wake, err := w.idle(ctx, conn, min(limit, IdleRestart))
	if err != nil {
		return false, err
	}
	if wake.Expunged && w.expungeAt.IsZero() {
		at := w.lastSweep.Add(w.s.sweepFloor)
		if now := w.s.clock.Now(); now.After(at) {
			at = now
		}
		w.expungeAt = at
	}
	return w.sweepDue(), nil
}

// idle runs conn.Idle until the server wakes it, the store's own timer for limit
// passes, or the supervisor's clock passes limit plus idleGrace, when it
// cancels the IDLE. Idle runs in a joined goroutine; the connection is used
// by one goroutine at a time.
func (w *watcher) idle(ctx context.Context, conn mailstore.WatchConn, limit time.Duration) (mailstore.Wake, error) {
	idleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		wake mailstore.Wake
		err  error
	}
	done := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		wake, err := conn.Idle(idleCtx, limit)
		done <- result{wake: wake, err: err}
	})
	var r result
	select {
	case r = <-done:
	case <-w.s.clock.After(limit + idleGrace):
		cancel()
		r = <-done
		if ctx.Err() == nil && errors.Is(r.err, context.Canceled) {
			r.err = nil // the supervisor's own timer ended the IDLE
		}
	}
	wg.Wait()
	return r.wake, r.err
}

// nextSweep is when the folder's next sweep is due: ExpungeSweep after the
// last one, or sooner when an EXPUNGE asked for one.
func (w *watcher) nextSweep() time.Time {
	due := w.lastSweep.Add(w.s.settings.ExpungeSweep)
	if !w.expungeAt.IsZero() && w.expungeAt.Before(due) {
		due = w.expungeAt
	}
	return due
}

// sweepDue reports whether the next pass must sweep.
func (w *watcher) sweepDue() bool { return !w.nextSweep().After(w.s.clock.Now()) }

// watcherStore opens the account's store as a mailstore.Watcher. The caller
// releases it as soon as its call returns.
func (s *Supervisor) watcherStore(ctx context.Context) (mailstore.Watcher, func(), error) {
	st, release, err := s.o.Store(ctx)
	if err != nil {
		return nil, nil, err
	}
	w, ok := st.(mailstore.Watcher)
	if !ok {
		release()
		return nil, nil, errNoWatcher
	}
	return w, release, nil
}

// openWatch opens a watch connection on folder. The store is released as
// soon as OpenWatch returns: the connection lives outside the pool.
func (s *Supervisor) openWatch(ctx context.Context, folder string) (mailstore.WatchConn, error) {
	w, release, err := s.watcherStore(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return w.OpenWatch(ctx, folder)
}

// resolveAll finds a Gmail account's \All folder by its role, never by name.
func (s *Supervisor) resolveAll(ctx context.Context) (string, error) {
	st, release, err := s.o.Store(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if f.Role == mailstore.RoleAll && f.Selectable {
			return f.Name, nil
		}
	}
	return "", &mailstore.FolderNotFoundError{Name: `\All`}
}

// closeConn logs out of a watch connection. Close bounds itself at
// ShutdownLogout.
func (s *Supervisor) closeConn(conn mailstore.WatchConn, folder string) {
	if err := conn.Close(); err != nil {
		s.logger.Debug("closing a watch connection", "folder", folder, "reason", failureReason(err))
	}
}

// settled records a successful pass as watching or polling, by mode. A
// baseline published its own status with the pass, so the steady state is
// recorded after it.
func (s *Supervisor) settled(ctx context.Context, folder, mode string, res PassResult) {
	if res.Baselined {
		s.mu.Lock()
		s.states[folder] = StateBaselined
		s.mu.Unlock()
	}
	state := StateWatching
	if mode == passModePoll {
		state = StatePolling
	}
	s.transition(ctx, folder, mode, state, "", "")
}

// transition records a folder's status, only when state differs from the
// last one recorded for the folder: the row through State.SetStatus first,
// so a reader that sees the event also sees the row, then a status event.
// A Gmail status from before the \All folder is resolved has an empty
// folder and no row.
func (s *Supervisor) transition(ctx context.Context, folder, mode, state, detail, lastError string) {
	s.mu.Lock()
	same := s.states[folder] == state
	s.states[folder] = state
	s.mu.Unlock()
	if same {
		return
	}
	account := s.o.Account.Name
	if folder != "" {
		if err := s.o.State.SetStatus(ctx, account, folder, mode, state, lastError); err != nil {
			s.logger.Warn("recording a folder's sync status", "folder", folder, "state", state, "reason", failureReason(err))
		}
	}
	ev := Event{At: s.clock.Now(), Kind: KindStatus, Account: account, Folder: folder, State: state, Detail: detail}
	if err := s.o.Log.Publish(ctx, ev); err != nil {
		s.logger.Warn("publishing a sync status", "folder", folder, "state", state, "reason", failureReason(err))
	}
}

// sleep waits d on the supervisor's clock. It reports false when ctx ended first.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.clock.After(d):
		return true
	}
}

// failureReason is err's class, from a fixed set, for logs. Store errors can
// quote the server, which can echo anything it was sent, so their text is
// never logged.
func failureReason(err error) string {
	var notFound *mailstore.FolderNotFoundError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, auth.ErrAuthRequired):
		return "auth_required"
	case errors.Is(err, auth.ErrRejected):
		return "auth_rejected"
	case errors.As(err, &notFound):
		return "folder_not_found"
	case errors.Is(err, mailstore.ErrNotFound):
		return "not_found"
	case errors.Is(err, mailstore.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, errNoWatcher):
		return "no_watcher"
	default:
		return "other"
	}
}
