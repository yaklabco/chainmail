package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/caldav"
	"github.com/yaklabco/chainmail/internal/calstore/google"
	"github.com/yaklabco/chainmail/internal/calsync"
	"github.com/yaklabco/chainmail/internal/config"
)

// calAccount is one account's calendar runtime.
type calAccount struct {
	name string
	// store is the account's calendar backend, nil when its store is none
	// or calendars are disabled.
	store calstore.Store
	// syncer keeps calendar.db up to date from store. Nil when store is
	// nil or calendar.db is not open. Poke and Status accept a nil syncer.
	syncer *calsync.Syncer
}

const (
	// calendarNotReadyHint answers a calendar read before the account's
	// first complete calendar sync.
	calendarNotReadyHint = "the calendar has not finished its first sync; run: chainmail cache status"
	// calendarAuthHint answers a calendar refusal of credentials when the
	// account is not known.
	calendarAuthHint = "run: chainmail auth login <account>"
	// eventRefHint answers an event ref that does not parse.
	eventRefHint = "event refs come from agenda, invites or event results; pass them unchanged"
)

// errCalendarNotReady refuses a calendar read on an account whose first
// calendar sync has not finished, or whose calendar.db is not open.
var errCalendarNotReady = &fixedTextError{
	msg: "the calendar cache has not finished its first sync for this account",
	err: &notReadyCause{hint: calendarNotReadyHint},
}

// buildCalendars fills s.calendars with every configured account and the
// calendar store of each whose store is not none. It does no IO.
func (s *Server) buildCalendars() {
	s.calSettings = s.opts.Config.CalendarSettings()
	if s.opts.CalendarPoll > 0 {
		s.calSettings.Poll = s.opts.CalendarPoll
	}
	cfg := s.opts.Config
	s.calendars = make(map[string]*calAccount, len(cfg.AccountNames()))
	for _, name := range cfg.AccountNames() {
		ca := &calAccount{name: name}
		s.calendars[name] = ca
		acct, err := cfg.Account(name)
		if err != nil || s.opts.DisableCalendar || acct.Calendar.Store == config.CalendarStoreNone || acct.Calendar.Store == "" {
			continue
		}
		ca.store = s.newCalendarStore(acct)
	}
}

// newCalendarStore builds acct's calendar backend, nil when it cannot be
// built (logged).
func (s *Server) newCalendarStore(acct config.ResolvedAccount) calstore.Store {
	if s.opts.NewCalendarStore != nil {
		return s.opts.NewCalendarStore(acct)
	}
	hc, err := s.auth.HTTPClient(acct.Name)
	if err != nil {
		s.opts.Logger.Error("building the calendar client failed", "account", acct.Name, "reason", apiReason(err))
		return nil
	}
	switch acct.Calendar.Store {
	case config.CalendarStoreGoogle:
		return google.New(hc)
	case config.CalendarStoreCalDAV:
		st, err := caldav.New(hc, acct.Calendar.URL, caldav.WithClock(s.opts.Now))
		if err != nil {
			// Config validation rules this out; the URL is not logged.
			s.opts.Logger.Error("the CalDAV URL is not usable", "account", acct.Name)
			return nil
		}
		return st
	default:
		s.opts.Logger.Error("unknown calendar store", "account", acct.Name)
		return nil
	}
}

// openCalendar opens calendar.db in the cache directory and gives each
// account with a store its syncer. It opens nothing with DisableCalendar or
// without a CacheDir; a failure is logged and calendar reads then answer
// CACHE_NOT_READY. It opens even when every account's store is none, since
// the invite hook keeps the invites found in mail there (Amendment 6), and
// such accounts get no syncer. calcache.Open replaces a
// file that is corrupt or of another schema itself. It does no network IO:
// the syncers start in startBackground.
func (s *Server) openCalendar(ctx context.Context) {
	if s.opts.DisableCalendar || s.opts.CacheDir == "" {
		return
	}
	if err := os.MkdirAll(s.opts.CacheDir, 0o700); err != nil {
		s.opts.Logger.Error("calendar.db could not be opened; calendars do not sync", "err", err)
		return
	}
	cc, err := calcache.Open(ctx, filepath.Join(s.opts.CacheDir, calcache.FileName))
	if err != nil {
		s.opts.Logger.Error("calendar.db could not be opened; calendars do not sync", "err", err)
		return
	}
	if why := cc.Recreated(); why != "" {
		s.opts.Logger.Info("recreated calendar.db; calendars sync in full", "reason", why)
	}
	s.calcache = cc
	cs := s.calSettings
	for _, name := range s.opts.Config.AccountNames() {
		ca := s.calendars[name]
		if ca == nil || ca.store == nil {
			continue
		}
		acct, err := s.opts.Config.Account(name)
		if err != nil {
			continue
		}
		ca.syncer = calsync.New(calsync.Options{
			Account:    name,
			Store:      ca.store,
			Cache:      cc,
			Log:        s.watchLog,
			Self:       s.policy.IsOwn,
			Include:    acct.Calendar.Include,
			Exclude:    acct.Calendar.Exclude,
			Default:    acct.Calendar.Default,
			Poll:       cs.Poll,
			WindowPast: cs.WindowPast,
			Clock:      s.calClock,
			Logger:     s.opts.Logger,
		})
	}
}

// closeCalendar closes calendar.db, once nothing uses it any more.
func (s *Server) closeCalendar() {
	if s.calcache == nil {
		return
	}
	if err := s.calcache.Close(); err != nil {
		s.opts.Logger.Warn("closing calendar.db", "err", err)
	}
}

// startCalendarSyncers runs every syncer on wg until ctx ends.
func (s *Server) startCalendarSyncers(ctx context.Context, wg *sync.WaitGroup) {
	var names []string
	for _, name := range s.opts.Config.AccountNames() {
		ca := s.calendars[name]
		if ca == nil || ca.syncer == nil {
			continue
		}
		names = append(names, name)
		wg.Go(func() { ca.syncer.Run(ctx) })
	}
	if len(names) > 0 {
		s.opts.Logger.Info("syncing calendars", "accounts", names)
	}
}

// calendarAccount returns the calendar runtime of the named account. An
// invalid name is INVALID_ARGUMENT and an unknown one NOT_FOUND, as the
// other account lookups answer.
func (s *Server) calendarAccount(name string) (*calAccount, error) {
	if !config.ValidName(name) {
		return nil, errInvalidAccountName
	}
	if _, err := s.opts.Config.Account(name); err != nil {
		return nil, err
	}
	ca, ok := s.calendars[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", config.ErrUnknownAccount, name)
	}
	return ca, nil
}

// calendarReady returns nil when the calendar cache can answer reads of
// account, else an error toConnectError maps to CACHE_NOT_READY.
func (s *Server) calendarReady(ctx context.Context, account string) error {
	if s.calcache == nil {
		return errCalendarNotReady
	}
	ready, err := s.calcache.Ready(ctx, account)
	if err != nil {
		return err
	}
	if !ready {
		return errCalendarNotReady
	}
	return nil
}

// calendarStatuses reports the calendar cache of each of accts that has a
// calendar store, in their order. It never fails: what cannot be read is
// state error with an INTERNAL reason, and the cause goes to the log.
func (s *Server) calendarStatuses(ctx context.Context, accts []config.ResolvedAccount) []*chainmailv1.AccountCalendarStatus {
	var out []*chainmailv1.AccountCalendarStatus
	for _, acct := range accts {
		ca := s.calendars[acct.Name]
		if ca == nil || ca.store == nil {
			continue
		}
		out = append(out, s.calendarStatus(ctx, ca))
	}
	return out
}

func (s *Server) calendarStatus(ctx context.Context, ca *calAccount) *chainmailv1.AccountCalendarStatus {
	out := &chainmailv1.AccountCalendarStatus{Account: ca.name}
	if ca.syncer == nil || s.calcache == nil {
		out.State, out.LastError = calsync.StateError, api.ReasonInternal
		return out
	}
	st := ca.syncer.Status()
	out.State, out.LastSyncAt = st.State, timestamp(st.LastSync)
	if st.LastErr != nil {
		out.LastError = apiReason(calendarError(ca.name, st.LastErr))
	}
	cals, events, err := s.calcache.Stats(ctx, ca.name)
	if err != nil {
		if ctx.Err() == nil {
			s.opts.Logger.Warn("reading an account's calendar cache counts", "account", ca.name, "err", err)
		}
		out.State, out.LastError = calsync.StateError, api.ReasonInternal
		return out
	}
	out.Calendars, out.Events = cals, events
	return out
}

// calendarAuthError is a calendar server's refusal of an account's
// credentials, with the account for the hint. It never means the mail
// credentials are bad, so it is never reported to auth.Manager.
type calendarAuthError struct {
	account string
	err     error
}

func (e *calendarAuthError) Error() string {
	return "the calendar server refused the account's credentials"
}

func (e *calendarAuthError) Unwrap() error { return e.err }

// calendarError adds the account to a calendar authorization failure, so
// its hint names the login to run. Other errors pass through.
func calendarError(account string, err error) error {
	if errors.Is(err, calstore.ErrAuth) {
		if _, ok := errors.AsType[*calendarAuthError](err); !ok {
			return &calendarAuthError{account: account, err: err}
		}
	}
	return err
}

// pokeCalendar asks account's calendar syncer for a cycle soon. An account
// with no syncer, or no such account, is nothing to do. The calendars map is
// built before any cache worker runs and never changes after.
func (s *Server) pokeCalendar(account string) {
	if ca := s.calendars[account]; ca != nil && ca.syncer != nil {
		ca.syncer.Poke()
	}
}
