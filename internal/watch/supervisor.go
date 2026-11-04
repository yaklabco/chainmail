package watch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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

// reasonFolderNotFound is the status detail and sync_folders last_error of
// a synced folder the account does not have.
const reasonFolderNotFound = "FOLDER_NOT_FOUND"

// Fallbacks for settings left zero. config.Parse always fills them; these
// only keep a hand-built SupervisorOptions from sweeping or polling in a
// tight loop.
const (
	fallbackExpungeSweep     = time.Hour
	fallbackIdleFallbackPoll = 30 * time.Second
	fallbackGoogleCheck      = 5 * time.Second
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
	// Clock is what the supervisor waits on. Nil means the system clock.
	Clock Clock
	// Jitter returns a value in [0, 1). Nil means math/rand/v2.
	Jitter func() float64
	// Logger gets one line per failure, without error text. Nil discards.
	Logger *slog.Logger
}

// Supervisor watches and syncs one account: one watcher loop per watched
// folder, each on its own connection, and one poller for the synced folders
// that are not watched. On a SyncAuto account the poller's folders are the
// ones a default search covers, less the watched ones, resolved when each
// session starts and every expunge_sweep. Spec section 5 describes its
// failure handling. Auth parks the whole account: every loop stops, every
// known folder's row says auth_required, and one account-wide status event
// carries the hint.
type Supervisor struct {
	o          SupervisorOptions
	clock      Clock
	jitter     func() float64
	logger     *slog.Logger
	settings   config.WatchSettings
	syncPoll   time.Duration
	sweepFloor time.Duration

	// writeStatus and publishEvents are State.SetStatus and Log.Publish.
	// Tests replace them to fail a status write.
	writeStatus   func(ctx context.Context, account, folder string, mode Mode, state Status, lastError string) error
	publishEvents func(ctx context.Context, events ...Event) error

	// mu guards statuses and auto. statuses holds the row last written and
	// the status last published per folder, keyed by name, "" for the
	// account-wide status; its folders other than "" are the ones a parking
	// records. auto is a SyncAuto account's polled set as last resolved,
	// which the next session's poller starts from.
	mu       sync.Mutex
	statuses map[string]folderStatus
	auto     []string
}

// NewSupervisor returns a supervisor for o.Account. A nil Clock, Jitter,
// Logger or AuthRequired gets the system clock, math/rand/v2, a discarding
// logger and "never required".
func NewSupervisor(o SupervisorOptions) *Supervisor {
	s := &Supervisor{
		o: o, clock: o.Clock, jitter: o.Jitter, logger: o.Logger,
		settings: o.Settings, syncPoll: o.Account.SyncPoll, statuses: map[string]folderStatus{},
		auto: polledFolders(o.Account),
	}
	if s.clock == nil {
		s.clock = realClock
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
	if s.settings.GoogleCheck <= 0 {
		s.settings.GoogleCheck = fallbackGoogleCheck
	}
	if s.syncPoll <= 0 {
		s.syncPoll = fallbackSyncPoll
	}
	s.writeStatus, s.publishEvents = o.State.SetStatus, o.Log.Publish
	if !o.Account.Provider.Gmail {
		for _, f := range o.Account.Watch {
			s.know(f, ModeIdle)
		}
		for _, f := range polledFolders(o.Account) {
			s.know(f, ModePoll)
		}
	}
	s.sweepFloor = o.SweepFloor
	if s.sweepFloor <= 0 {
		s.sweepFloor = ExpungeSweepFloor
	}
	return s
}

// Run watches and syncs until ctx ends, then closes every connection. It
// returns at once when the account has sync disabled, and when the store
// cannot watch. While the account needs a human every loop is stopped and
// the account is parked; the loops start again once the auth state clears.
func (s *Supervisor) Run(ctx context.Context) {
	if !s.o.Account.SyncEnabled {
		return
	}
	for {
		need, hint := s.o.AuthRequired(s.o.Account.Name)
		if !need {
			var park bool
			if park, hint = s.active(ctx); !park || ctx.Err() != nil {
				return
			}
		}
		if !s.park(ctx, hint) {
			return
		}
	}
}

// parker lets any loop of a running account ask Run to park it. The first
// request's hint wins, and every request stops every loop.
type parker struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	asked  bool
	hint   string
}

// park asks Run to park the account with hint and stops every loop.
func (p *parker) park(hint string) {
	p.mu.Lock()
	if !p.asked {
		p.asked, p.hint = true, hint
	}
	p.mu.Unlock()
	p.cancel()
}

// requested reports whether a loop asked to park, and with which hint.
func (p *parker) requested() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked, p.hint
}

// active runs the watcher loops and the poller until ctx ends, every loop
// has returned, or the account needs a human, asking AuthRequired every
// authRecheck meanwhile. It returns once every loop has returned and closed
// its connection, so no pass is running, and reports whether to park.
func (s *Supervisor) active(ctx context.Context) (park bool, hint string) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &parker{cancel: cancel}
	var loops sync.WaitGroup
	if s.o.Account.Provider.Gmail {
		// One watcher on the \All folder covers every Gmail label, so a
		// Gmail account polls nothing.
		loops.Go(func() { s.watchLoop(actx, p, "") })
	} else {
		for _, folder := range s.o.Account.Watch {
			loops.Go(func() { s.watchLoop(actx, p, folder) })
		}
		folders := polledFolders(s.o.Account)
		if s.o.Account.SyncAuto {
			// The poller resolves the set again before its first poll; it
			// starts from the last set when that fails.
			folders = s.autoSet()
		}
		if len(folders) > 0 || s.o.Account.SyncAuto {
			loops.Go(func() { s.pollLoop(actx, p, folders) })
		}
	}
	done := make(chan struct{})
	var join sync.WaitGroup
	join.Go(func() {
		loops.Wait()
		close(done)
	})
	s.checkAuth(actx, done, p)
	join.Wait()
	return p.requested()
}

// checkAuth asks AuthRequired every authRecheck until done closes or ctx
// ends, and parks the account the first time the answer is yes.
func (s *Supervisor) checkAuth(ctx context.Context, done <-chan struct{}, p *parker) {
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-s.clock.After(authRecheck):
		}
		if need, hint := s.o.AuthRequired(s.o.Account.Name); need {
			p.park(hint)
			return
		}
	}
}

// park records the account as needing a human, then asks AuthRequired
// every authRecheck until it clears. It waits at least one authRecheck, so
// a refusal is never retried sooner, and reports false when ctx ends.
func (s *Supervisor) park(ctx context.Context, hint string) bool {
	s.logger.Warn("watching is parked until the account is authenticated")
	recorded := false
	for {
		// Armed before the status is recorded, so whoever sees the status
		// finds the recheck already waiting on the clock.
		recheck := s.clock.After(authRecheck)
		if !recorded {
			if err := s.recordParked(ctx, hint); err != nil {
				s.logger.Warn("recording that the account is parked", "reason", failureReason(err))
			} else {
				recorded = true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-recheck:
		}
		if need, _ := s.o.AuthRequired(s.o.Account.Name); !need {
			// The next account-wide status is a new one.
			s.forget("")
			return true
		}
	}
}

// recordParked writes auth_required to every known folder's row, then
// publishes one account-wide status event with the hint, which stands for
// every folder's, so each folder publishes its own status again when it
// resumes. Every loop has returned, so no pass can overwrite a row. A
// failure leaves the rest for the next call.
func (s *Supervisor) recordParked(ctx context.Context, hint string) error {
	folders := s.knownFolders()
	for _, f := range folders {
		if err := s.setStatus(ctx, f.name, f.row.mode, StateAuthRequired, api.ReasonAuthRequired); err != nil {
			return err
		}
	}
	if err := s.transition(ctx, "", "", StateAuthRequired, hint, api.ReasonAuthRequired); err != nil {
		return err
	}
	for _, f := range folders {
		s.update(f.name, func(st *folderStatus) { st.published = StateAuthRequired })
	}
	return nil
}

// watcher is one watched folder's loop state. Only its own goroutine uses it.
type watcher struct {
	s *Supervisor
	// folder is "" on Gmail until the \All folder is resolved.
	folder string
	// mode is ModeIdle, or ModePoll on a server without IDLE.
	mode Mode
	// lastSweep is when the last sweep pass ran. expungeAt is when an
	// EXPUNGE asked for the next one, zero when none is pending.
	lastSweep time.Time
	expungeAt time.Time
	// shutdownAt is the one deadline for tearing the connection down once
	// ctx has ended: ShutdownLogout after the watcher first saw it end,
	// across DONE and LOGOUT both. Zero until then.
	shutdownAt time.Time
}

// watchLoop keeps one folder watched until ctx ends. folder is "" on Gmail,
// which resolves its \All folder before each connection that lacks it. An
// auth failure parks the whole account through p, a missing folder retries
// every sync.poll, and anything else reconnects with backoff, reset by a
// successful pass.
func (s *Supervisor) watchLoop(ctx context.Context, p *parker, folder string) {
	w := &watcher{s: s, folder: folder, mode: ModeIdle}
	failures := 0
	for {
		if need, hint := s.o.AuthRequired(s.o.Account.Name); need {
			p.park(hint)
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
			s.logger.Warn("a watch connection needs the account authenticated", "folder", w.folder, "reason", failureReason(err))
			p.park(s.authHint(err))
			return
		case errors.As(err, &notFound):
			s.logger.Warn("a watched folder does not exist", "folder", w.folder)
			s.note(ctx, w.folder, w.mode, StateError, reasonFolderNotFound, reasonFolderNotFound)
			if s.o.Account.Provider.Gmail {
				w.folder = "" // resolve the \All folder again
			}
			wait = s.syncPoll
		default:
			s.logger.Warn("watch connection failed", "folder", w.folder, "reason", failureReason(err))
			s.note(ctx, w.folder, w.mode, StateReconnecting, "", api.ReasonServerUnavailable)
			wait = backoff(failures, s.jitter())
			failures++
		}
		if !s.sleep(ctx, wait) {
			return
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

// polled is one polled folder's schedule. Only the poller's goroutine uses it.
type polled struct {
	folder string
	// next is when the folder is due, lastSweep when its last sweep ran.
	next      time.Time
	lastSweep time.Time
	// failures counts failed polls since the last success.
	failures int
}

// pollLoop polls folders until ctx ends, each every sync.poll, starting at
// once so they catch up from their checkpoints. On a SyncAuto account it
// first resolves the polled set again, and again every expunge_sweep
// (refreshAuto), so folders is only where it starts. A folder's failure
// stays its own: a missing folder retries every sync.poll and an outage
// with backoff capped at sync.poll. An auth failure parks the account
// through p.
func (s *Supervisor) pollLoop(ctx context.Context, p *parker, folders []string) {
	sched := make([]*polled, 0, len(folders))
	for _, f := range folders {
		sched = append(sched, &polled{folder: f})
	}
	var listedAt time.Time // when refreshAuto last ran; zero runs it at once
	for {
		if need, hint := s.o.AuthRequired(s.o.Account.Name); need {
			p.park(hint)
			return
		}
		if s.o.Account.SyncAuto && !s.clock.Now().Before(listedAt.Add(s.settings.ExpungeSweep)) {
			listedAt = s.clock.Now()
			sched = s.refreshAuto(ctx, sched)
			if ctx.Err() != nil {
				return
			}
		}
		for _, pf := range sched {
			// Each folder reads the clock for itself, so one that took
			// long to poll does not hand its start time to the next.
			if pf.next.After(s.clock.Now()) {
				continue
			}
			err := s.pollFolder(ctx, pf)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errNoWatcher) {
				s.logger.Error("the account's store cannot sync folders")
				return
			}
			if authFailure(err) {
				s.logger.Warn("a poll needs the account authenticated", "folder", pf.folder, "reason", failureReason(err))
				p.park(s.authHint(err))
				return
			}
		}
		if !s.sleep(ctx, s.nextPoll(sched, listedAt).Sub(s.clock.Now())) {
			return
		}
	}
}

// pollFolder polls one due folder, records its status and schedules its
// next poll: sync.poll after this one started when it succeeds, and after
// the failure, once the call and its status write are done, when it fails,
// so a call that is slow to fail still waits its whole backoff. The first
// poll of a folder sweeps, then one every ExpungeSweep. A failed status
// write counts as a failed poll. The error is returned so pollLoop can stop
// for auth or a store that cannot sync.
func (s *Supervisor) pollFolder(ctx context.Context, pf *polled) error {
	now := s.clock.Now()
	sweep := !now.Before(pf.lastSweep.Add(s.settings.ExpungeSweep))
	res, err := s.pollPass(ctx, pf.folder, sweep)
	if err == nil {
		if sweep || res.Baselined {
			pf.lastSweep = now
		}
		err = s.settled(ctx, pf.folder, ModePoll, res)
	}
	if err == nil {
		pf.failures = 0
		pf.next = now.Add(s.syncPoll)
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, errNoWatcher) || authFailure(err) {
		return err // pollLoop handles these for every folder at once
	}
	var notFound *mailstore.FolderNotFoundError
	if errors.As(err, &notFound) {
		s.note(ctx, pf.folder, ModePoll, StateError, reasonFolderNotFound, reasonFolderNotFound)
		pf.next = s.clock.Now().Add(s.syncPoll)
		return err
	}
	s.logger.Warn("polling a folder failed", "folder", pf.folder, "reason", failureReason(err))
	s.note(ctx, pf.folder, ModePoll, StateReconnecting, "", api.ReasonServerUnavailable)
	pf.next = s.clock.Now().Add(min(backoff(pf.failures, s.jitter()), s.syncPoll))
	pf.failures++
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
	return s.o.Engine.Pass(ctx, s.o.Account.Name, folder, storeSource{W: w, Folder: folder}, PassOptions{Sweep: sweep, Mode: ModePoll})
}

// session runs one connection on the folder: open, sync, then IDLE or poll
// and sync after every wake, until an error or the end of ctx. healthy
// reports whether a pass succeeded on it, which ends an outage.
func (w *watcher) session(ctx context.Context) (healthy bool, err error) {
	if w.folder == "" {
		if w.folder, err = w.s.resolveAll(ctx); err != nil {
			return false, err
		}
		w.s.know(w.folder, ModeIdle)
	}
	conn, err := w.s.openWatch(ctx, w.folder)
	if err != nil {
		return false, err
	}
	defer w.closeConn(ctx, conn)
	w.mode = ModePoll
	if conn.IdleSupported() {
		w.mode = ModeIdle
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
// polling. A sweep, or a baseline, restarts the sweep schedule. A status
// that cannot be recorded fails the pass like a database error.
func (w *watcher) pass(ctx context.Context, conn mailstore.WatchConn, sweep bool) error {
	res, err := w.s.o.Engine.Pass(ctx, w.s.o.Account.Name, w.folder, conn, PassOptions{Sweep: sweep, Mode: w.mode})
	if err != nil {
		return err
	}
	if sweep || res.Baselined {
		w.lastSweep = w.s.clock.Now()
		w.expungeAt = time.Time{}
	}
	return w.s.settled(ctx, w.folder, w.mode, res)
}

// wait blocks until the folder needs another pass and reports whether that
// pass must sweep. With IDLE it waits for the server, the idleRestart or a
// due sweep, and on the Google presets for google_check at most, since
// Gmail can tell an IDLE about a change 30 seconds or more after it; without
// IDLE, for idle_fallback_poll or a due sweep.
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
	limit = min(limit, idleRestart)
	if w.s.o.Account.Provider.Gmail {
		limit = min(limit, w.s.settings.GoogleCheck)
	}
	wake, err := w.idle(ctx, conn, limit)
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
//
// Once the IDLE is cancelled, Idle still ends it with DONE, which a store
// bounds only by its command timeout. So a cancelled Idle has until a
// deadline on the supervisor's clock to return, after which the connection
// is torn down under it (abortConn). When ctx ended, that deadline is the
// one shutdown deadline (startShutdown), which closeConn then holds LOGOUT
// to as well. When the supervisor's own timer cancelled it, the deadline
// is ShutdownLogout from then, and a ctx that ends meanwhile starts the
// shutdown deadline at that moment.
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
	// stop cancels the Idle and waits for it until expiry, then aborts the
	// connection. ended, when not nil, is a ctx that may end meanwhile.
	stop := func(expiry func() <-chan time.Time, ended <-chan struct{}) result {
		cancel()
		select {
		case r := <-done:
			return r // no timer for an Idle that has returned already
		default:
		}
		timer := expiry()
		for {
			select {
			case r := <-done:
				return r
			case <-ended:
				w.startShutdown() // later than timer, which stays the bound here
				ended = nil
			case <-timer:
				w.s.abortConn(conn, w.folder)
				return <-done
			}
		}
	}
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		w.startShutdown()
		r = stop(w.shutdownTimer, nil)
	case <-w.s.clock.After(limit + idleGrace):
		r = stop(func() <-chan time.Time { return w.s.clock.After(ShutdownLogout) }, ctx.Done())
		if ctx.Err() == nil && errors.Is(r.err, context.Canceled) {
			r.err = nil // the supervisor's own timer ended the IDLE
		}
	}
	wg.Wait()
	return r.wake, r.err
}

// startShutdown fixes the shutdown deadline at ShutdownLogout from now, the
// first time it is called. Callers call it as soon as they see ctx end.
func (w *watcher) startShutdown() {
	if w.shutdownAt.IsZero() {
		w.shutdownAt = w.s.clock.Now().Add(ShutdownLogout)
	}
}

// shutdownTimer fires at the shutdown deadline, starting it if needed, and
// at once when it has passed.
func (w *watcher) shutdownTimer() <-chan time.Time {
	w.startShutdown()
	return w.s.clock.After(w.shutdownAt.Sub(w.s.clock.Now()))
}

// closeConn logs out of the session's connection. While ctx runs, Close
// bounds itself as the store does. Once ctx has ended, Close runs in a
// joined goroutine with only what is left of the shutdown deadline, and a
// connection still logging out then is aborted (mailstore.Aborter), so DONE
// and LOGOUT together take at most ShutdownLogout. A connection that cannot
// abort gets the store's own bound.
func (w *watcher) closeConn(ctx context.Context, conn mailstore.WatchConn) {
	a, ok := conn.(mailstore.Aborter)
	if ctx.Err() == nil || !ok {
		w.s.closeConn(conn, w.folder)
		return
	}
	closed := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(closed)
		w.s.closeConn(conn, w.folder)
	})
	select {
	case <-closed:
	case <-w.shutdownTimer():
		w.s.logger.Debug("aborting a watch connection still logging out at the shutdown deadline", "folder", w.folder)
		a.Abort()
	}
	wg.Wait()
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
	pool, open := budgetOf(w)
	conn, err := w.OpenWatch(ctx, folder)
	if errors.Is(err, mailstore.ErrConnLimit) {
		s.logger.Warn("server refused a watch connection for its connection limit",
			"folder", folder, "pool", pool, "open", open, "watchers", s.o.Account.WatchCount())
	}
	return conn, err
}

// budgeter is a store that reports its connection budget: the pool's cap,
// and the connections holding a slot on the server, pooled and watch.
// imapstore.Store is one.
type budgeter interface {
	PoolSize() int
	OpenConns() int
}

// budgetOf is st's pool cap and open connections, or -1 for each when st
// cannot say. openWatch takes it as a dial starts, so a refusal for the
// server's connection limit logs the budget the server refused (spec
// section 5), before the store makes room for the watcher's next try, with
// the watcher connections the account needs. The fields are fixed and
// never carry the server's text.
func budgetOf(st mailstore.Watcher) (pool, open int) {
	if b, ok := st.(budgeter); ok {
		return b.PoolSize(), b.OpenConns()
	}
	return -1, -1
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

// abortConn tears conn down under whatever is using it: Abort when it has
// one (mailstore.Aborter), else Close, which a store makes safe while Idle
// runs.
func (s *Supervisor) abortConn(conn mailstore.WatchConn, folder string) {
	if a, ok := conn.(mailstore.Aborter); ok {
		a.Abort()
		return
	}
	s.closeConn(conn, folder)
}

// closeConn logs out of a watch connection. Close bounds itself at
// ShutdownLogout.
func (s *Supervisor) closeConn(conn mailstore.WatchConn, folder string) {
	if err := conn.Close(); err != nil {
		s.logger.Debug("closing a watch connection", "folder", folder, "reason", failureReason(err))
	}
}

// settled records a successful pass as watching or polling, by mode. The
// pass wrote the row's mode, and a baseline also wrote the baselined state
// and published its event, so the steady state is recorded after it. A
// failed write is returned, for the caller to back off as from a database
// error and record it again after its next pass.
func (s *Supervisor) settled(ctx context.Context, folder string, mode Mode, res PassResult) error {
	s.passed(folder, mode, res.Baselined)
	state := StateWatching
	if mode == ModePoll {
		state = StatePolling
	}
	return s.transition(ctx, folder, mode, state, "", "")
}

// transition records a folder's status: the row through State.SetStatus
// first, so a reader that sees the event also sees the row, then a status
// event. Each half is skipped when it already holds, and remembered only
// once it lands, so a transition that half failed is completed by the next
// one to the same state, and a return to the published state still rewrites
// a row a failed transition left different. A row that cannot be written is
// never announced. An empty folder is an account-wide status, such as a
// Gmail one from before the \All folder is resolved, with no row.
func (s *Supervisor) transition(ctx context.Context, folder string, mode Mode, state Status, detail, lastError string) error {
	if folder != "" {
		if err := s.setStatus(ctx, folder, mode, state, lastError); err != nil {
			return err
		}
	}
	if s.status(folder).published == state {
		return nil
	}
	ev := Event{At: s.clock.Now(), Kind: KindStatus, Account: s.o.Account.Name, Folder: folder, State: state, Detail: detail}
	if err := s.publishEvents(ctx, ev); err != nil {
		return fmt.Errorf("publishing sync status: %w", err)
	}
	s.update(folder, func(st *folderStatus) { st.published = state })
	return nil
}

// note is transition for a failure path, which goes on to back off
// whatever happens: a failed write is logged and completed by the next
// transition.
func (s *Supervisor) note(ctx context.Context, folder string, mode Mode, state Status, detail, lastError string) {
	if err := s.transition(ctx, folder, mode, state, detail, lastError); err != nil {
		s.logger.Warn("recording a folder's sync status", "folder", folder, "reason", failureReason(err))
	}
}

// setStatus writes a folder's row, without an event, unless the row last
// written already holds mode, state and lastError, and remembers it once
// written.
func (s *Supervisor) setStatus(ctx context.Context, folder string, mode Mode, state Status, lastError string) error {
	want := rowStatus{mode: mode, state: state, lastError: lastError}
	if s.status(folder).row == want {
		return nil
	}
	if err := s.writeStatus(ctx, s.o.Account.Name, folder, mode, state, lastError); err != nil {
		return fmt.Errorf("recording sync status: %w", err)
	}
	s.update(folder, func(st *folderStatus) { st.row = want })
	return nil
}

// rowStatus is what the supervisor last wrote, or a pass last left, in a
// folder's sync_folders row.
type rowStatus struct {
	mode      Mode
	state     Status
	lastError string
}

// folderStatus is what is durably recorded for a folder: its row, and the
// state of the last status event published for it. The two are kept apart
// because a transition can land one and not the other.
type folderStatus struct {
	name      string
	row       rowStatus
	published Status
}

// status returns what is recorded for folder.
func (s *Supervisor) status(folder string) folderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[folder]
}

// update changes what is recorded for folder.
func (s *Supervisor) update(folder string, change func(*folderStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.statuses[folder]
	st.name = folder
	change(&st)
	s.statuses[folder] = st
}

// passed records what a successful pass wrote: the row's mode, and on a
// baseline the baselined state and its event. A pass carries the row's
// last_error over, so that stays as recorded.
func (s *Supervisor) passed(folder string, mode Mode, baselined bool) {
	s.update(folder, func(st *folderStatus) {
		st.row.mode = mode
		if baselined {
			st.row.state, st.published = StateBaselined, StateBaselined
		}
	})
}

// know adds folder, in mode, to the folders a parking records, unless it is
// already known.
func (s *Supervisor) know(folder string, mode Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.statuses[folder]; !ok {
		s.statuses[folder] = folderStatus{name: folder, row: rowStatus{mode: mode}}
	}
}

// forget drops what was recorded for folder.
func (s *Supervisor) forget(folder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.statuses, folder)
}

// knownFolders are the folders with a row to keep, sorted by name.
func (s *Supervisor) knownFolders() []folderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []folderStatus
	for name, st := range s.statuses {
		if name != "" {
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b folderStatus) int { return cmp.Compare(a.name, b.name) })
	return out
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
	case errors.Is(err, errNoScoper):
		return "no_scoper"
	default:
		return "other"
	}
}
