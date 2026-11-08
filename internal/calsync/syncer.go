// Package calsync keeps one account's calendars in calendar.db. A Syncer
// lists the account's calendars, keeps those its include and exclude
// settings allow, syncs each from its stored token, turns what changed into
// watch events, and writes the changes. It runs a cycle every poll, sooner
// when poked, and backs off after failures.
//
// Watch events are published to state.db before the changes are committed
// to calendar.db: the two files cannot share a transaction, and a crash
// between the two repeats the diff on the next cycle, so the events are
// delivered at least once.
//
// No error, status or log line holds event text: errors and statuses are
// fixed text, refs, IDs and status codes.
package calsync

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/watch"
)

// Status states.
const (
	// StateReady means the last cycle synced every calendar.
	StateReady = "ready"
	// StateSyncing means no cycle has finished yet.
	StateSyncing = "syncing"
	// StateAuthRequired means the calendar server refused the account's
	// credentials. Mail is not affected.
	StateAuthRequired = "auth_required"
	// StateError means the last cycle failed for another reason.
	StateError = "error"
)

const (
	// DefaultPoll is the wait between cycles when Options.Poll is zero.
	DefaultPoll = 5 * time.Minute
	// maxBackoff caps the wait after repeated failures, unless Poll is
	// longer.
	maxBackoff = 30 * time.Minute
	// maxSleep is the longest single wait. The wall clock is read again
	// after each, so a suspend and resume is noticed.
	maxSleep = 5 * time.Minute
)

// Options configure a Syncer. Account, Store and Cache are required.
type Options struct {
	Account string
	Store   calstore.Store
	Cache   *calcache.Store
	// Log receives the watch events. Nil publishes none.
	Log *watch.Log
	// Self reports whether an address is one of James's. Nil knows none.
	Self func(addr string) bool
	// Include keeps only the calendars with these IDs or names; empty
	// keeps all. Exclude then drops the calendars with these IDs or names.
	// IDs match exactly and names without regard to case.
	Include []string
	Exclude []string
	// Default names the calendar for new events by ID or name. Empty or
	// "primary" means the primary calendar, else the first writable one.
	Default string
	// Poll is the wait between cycles. Zero means DefaultPoll.
	Poll time.Duration
	// WindowPast is how far back events are kept: singles and overrides
	// that ended before now - WindowPast are pruned after each cycle, and
	// changes to them emit no events.
	WindowPast time.Duration
	// Clock is the time source and timer. Nil means the system clock.
	Clock watch.Clock
	// Logger receives fixed-text log lines. Nil discards them.
	Logger *slog.Logger
}

// Status is a Syncer's state for status reports.
type Status struct {
	State    string
	LastSync time.Time
	// LastError is fixed text, "" when the last cycle succeeded.
	LastError string
	// LastErr is the error LastError describes, for callers that map it to
	// an API reason. Nil when LastError is "".
	LastErr error
}

// Syncer syncs one account's calendars. Its cycles never overlap.
type Syncer struct {
	o     Options
	clock watch.Clock
	wake  chan struct{}
	// series reads the stored rows of one object; nil means Cache.Series.
	// Tests replace it to make the read fail.
	series func(ctx context.Context, account, calID, providerID string) ([]calcache.EventRow, error)

	// cycleMu serializes cycles, Run's and SyncNow's.
	cycleMu sync.Mutex
	// marked is set once MarkReady has been called; guarded by cycleMu.
	marked bool
	// warnedDefault is set once an unmatched Default was logged; guarded
	// by cycleMu.
	warnedDefault bool

	mu       sync.Mutex
	status   Status
	failures int // consecutive failures other than auth
}

// New returns a Syncer. It does no IO.
func New(o Options) *Syncer {
	if o.Poll <= 0 {
		o.Poll = DefaultPoll
	}
	if o.Self == nil {
		o.Self = func(string) bool { return false }
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	clock := o.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Syncer{o: o, clock: clock, wake: make(chan struct{}, 1), status: Status{State: StateSyncing}}
}

// Run runs a cycle at once, then one every Poll, or sooner after a Poke,
// until ctx ends. After a failure other than an authorization one it waits
// Poll, doubling with each further failure up to 30 minutes.
func (s *Syncer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.SyncNow(ctx)
		if ctx.Err() != nil {
			return
		}
		if !s.sleep(ctx, wallNow(s.clock).Add(s.nextWait(err))) {
			return
		}
	}
}

// Poke asks Run for a cycle soon. It never blocks, and a nil Syncer, which
// an account without a calendar store has, ignores it.
func (s *Syncer) Poke() {
	if s == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SyncNow runs one cycle now, after any cycle under way, and returns its
// error.
func (s *Syncer) SyncNow(ctx context.Context) error {
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	err := s.cycle(ctx)
	if ctx.Err() != nil {
		// A shutdown or a caller giving up is not a sync failure.
		return cmp.Or(err, ctx.Err())
	}
	s.record(err)
	return err
}

// Status returns the syncer's state. A nil Syncer has the zero Status.
func (s *Syncer) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// record updates the status after a cycle.
func (s *Syncer) record(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.status = Status{State: StateReady, LastSync: wallNow(s.clock)}
		s.failures = 0
		return
	}
	s.status.LastError, s.status.LastErr = errorText(err), err
	if errors.Is(err, calstore.ErrAuth) {
		s.status.State = StateAuthRequired
		return
	}
	s.status.State = StateError
	s.failures++
}

// nextWait is the wait after a cycle that returned err.
func (s *Syncer) nextWait(err error) time.Duration {
	poll := s.o.Poll
	if err == nil || errors.Is(err, calstore.ErrAuth) {
		return poll
	}
	s.mu.Lock()
	n := s.failures
	s.mu.Unlock()
	limit := max(maxBackoff, poll)
	d := poll
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// sleep waits until the wall clock reaches until, at most maxSleep at a
// time, or until a Poke. It returns false when ctx ends first.
func (s *Syncer) sleep(ctx context.Context, until time.Time) bool {
	for {
		d := until.Sub(wallNow(s.clock))
		if d <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-s.wake:
			return true
		case <-s.clock.After(min(d, maxSleep)):
		}
	}
}

// cycle syncs every kept calendar. A calendar that fails is recorded on its
// row and the others still sync; an authorization failure ends the cycle,
// since it applies to every calendar. The account is marked ready after
// the first cycle in which every calendar synced.
func (s *Syncer) cycle(ctx context.Context) error {
	acct := s.o.Account
	cals, err := s.o.Store.ListCalendars(ctx)
	if err != nil {
		s.logFailure(ctx, "listing calendars failed", "", err)
		return err
	}
	kept := s.filter(cals)
	if err := s.o.Cache.SetCalendars(ctx, acct, kept, s.pickDefault(ctx, kept)); err != nil {
		s.logFailure(ctx, "storing calendars failed", "", err)
		return err
	}
	var failed error
	for _, c := range kept {
		err := s.syncCalendar(ctx, c.ID)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return err
		}
		s.logFailure(ctx, "syncing a calendar failed", c.ID, err)
		if serr := s.o.Cache.SetError(ctx, acct, c.ID, errorText(err)); serr != nil && ctx.Err() == nil {
			s.o.Logger.WarnContext(ctx, "recording a calendar error failed", "account", acct, "calendar", c.ID)
		}
		if errors.Is(err, calstore.ErrAuth) {
			return err
		}
		failed = cmp.Or(failed, err)
	}
	if _, err := s.o.Cache.Prune(ctx, wallNow(s.clock).Add(-s.o.WindowPast)); err != nil {
		s.logFailure(ctx, "pruning old events failed", "", err)
		failed = cmp.Or(failed, err)
	}
	if failed != nil {
		return failed
	}
	if !s.marked {
		if err := s.o.Cache.MarkReady(ctx, acct, wallNow(s.clock)); err != nil {
			s.logFailure(ctx, "marking the calendar cache ready failed", "", err)
			return err
		}
		s.marked = true
	}
	return nil
}

// syncCalendar syncs one calendar from its stored token, publishes the
// watch events of what changed, then writes the changes with the new
// token. A calendar's first sync publishes nothing. An invalid token is
// dropped and the calendar synced in full. Objects too large to take
// (Changes.Skipped, calcache.Admit) keep their stored rows, are counted in
// the log and recorded as the calendar's last error.
func (s *Syncer) syncCalendar(ctx context.Context, calID string) error {
	acct := s.o.Account
	row, err := s.o.Cache.Calendar(ctx, acct, calID)
	if err != nil {
		return err
	}
	ch, err := s.o.Store.Sync(ctx, calID, row.SyncToken)
	if errors.Is(err, calstore.ErrSyncTokenInvalid) && row.SyncToken != "" {
		s.o.Logger.InfoContext(ctx, "calendar sync token expired; syncing the calendar in full", "account", acct, "calendar", calID)
		ch, err = s.o.Store.Sync(ctx, calID, "")
	}
	if err != nil {
		return err
	}
	// Admit now, as Diff and Apply would, to learn what they skip.
	ch = calcache.Admit(ch)
	if !row.LastSync.IsZero() && s.o.Log != nil {
		changes, err := s.o.Cache.Diff(ctx, acct, calID, ch, s.o.Self)
		if err != nil {
			return err
		}
		evs, err := calendarEvents(eventInput{
			account: acct, calID: calID, changes: changes, self: s.o.Self,
			now: wallNow(s.clock), windowPast: s.o.WindowPast,
			master: func(providerID string) (*calcache.EventRow, error) {
				return s.storedMaster(ctx, calID, providerID)
			},
		})
		if err != nil {
			// Nothing is published or applied, so the next cycle diffs the
			// same changes again.
			return err
		}
		if len(evs) > 0 {
			if err := s.o.Log.Publish(ctx, evs...); err != nil {
				return err
			}
		}
	}
	if err := s.o.Cache.Apply(ctx, acct, calID, ch, s.o.Self, wallNow(s.clock)); err != nil {
		return err
	}
	if n := len(ch.Skipped); n > 0 {
		// Apply cleared the calendar's last error; the skip is recorded
		// after it, until the next sync that skips nothing clears it.
		s.o.Logger.WarnContext(ctx, skippedText, "account", acct, "calendar", calID, "skipped", n)
		if err := s.o.Cache.SetError(ctx, acct, calID, skippedText); err != nil && ctx.Err() == nil {
			s.o.Logger.WarnContext(ctx, "recording a calendar error failed", "account", acct, "calendar", calID)
		}
	}
	return nil
}

// skippedText is the fixed text recorded on a calendar when a sync skipped
// objects over calcache's limits (calcache.Admit) or the backend's.
const skippedText = "calendar objects too large to sync were skipped"

// storedMaster returns the stored row of providerID with no recurrence ID
// (the master or the single event), or nil when there is none. A failed
// read is an error, never taken for a missing master.
func (s *Syncer) storedMaster(ctx context.Context, calID, providerID string) (*calcache.EventRow, error) {
	read := s.series
	if read == nil {
		read = s.o.Cache.Series
	}
	rows, err := read(ctx, s.o.Account, calID, providerID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Event.RecurrenceID.IsZero() {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// filter applies Include and Exclude.
func (s *Syncer) filter(cals []calendar.Calendar) []calendar.Calendar {
	out := make([]calendar.Calendar, 0, len(cals))
	for _, c := range cals {
		if len(s.o.Include) > 0 && !names(c, s.o.Include) {
			continue
		}
		if names(c, s.o.Exclude) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// names reports whether c's ID or name is in list.
func names(c calendar.Calendar, list []string) bool {
	for _, n := range list {
		if c.ID == n || (c.Name != "" && strings.EqualFold(c.Name, n)) {
			return true
		}
	}
	return false
}

// pickDefault chooses the default calendar among cals: the one Default
// names by ID, else by name, or for an empty or "primary" Default the
// primary calendar, else the first writable one. "" when none matches.
func (s *Syncer) pickDefault(ctx context.Context, cals []calendar.Calendar) string {
	def := s.o.Default
	if def == "" || strings.EqualFold(def, "primary") {
		for _, c := range cals {
			if c.Primary {
				return c.ID
			}
		}
		for _, c := range cals {
			if c.Access == calendar.AccessWrite {
				return c.ID
			}
		}
		return ""
	}
	for _, c := range cals {
		if c.ID == def {
			return c.ID
		}
	}
	for _, c := range cals {
		if strings.EqualFold(c.Name, def) {
			return c.ID
		}
	}
	if !s.warnedDefault {
		s.warnedDefault = true
		s.o.Logger.WarnContext(ctx, "the configured default calendar matches no calendar", "account", s.o.Account)
	}
	return ""
}

// logFailure logs a failed step by its fixed text, never the error's.
func (s *Syncer) logFailure(ctx context.Context, msg, calID string, err error) {
	if ctx.Err() != nil {
		return
	}
	args := []any{"account", s.o.Account, "error", errorText(err)}
	if calID != "" {
		args = append(args, "calendar", calID)
	}
	s.o.Logger.WarnContext(ctx, msg, args...)
}

// errorText is the fixed text recorded for err.
func errorText(err error) string {
	switch {
	case errors.Is(err, calstore.ErrAuth):
		return "calendar access not authorized"
	case errors.Is(err, calstore.ErrUnavailable):
		return "calendar server unavailable"
	case errors.Is(err, calstore.ErrSyncTokenInvalid):
		return "calendar sync token refused"
	case errors.Is(err, calstore.ErrNotFound):
		return "calendar not found on the server"
	case errors.Is(err, context.DeadlineExceeded):
		return "calendar sync timed out"
	default:
		return "calendar sync failed"
	}
}

// wallNow reads the clock with the monotonic reading stripped, so a wait
// measured against it notices a suspend.
func wallNow(c watch.Clock) time.Time { return c.Now().Round(0) }

// systemClock is watch.Clock over package time.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
