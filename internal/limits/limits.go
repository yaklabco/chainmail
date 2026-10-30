// Package limits probes how many IMAP connections an account accepts and
// stores the result, from which the connection pool is sized.
package limits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

// Probe defaults and pool bounds.
const (
	// DefaultCeiling is the most connections a probe opens.
	DefaultCeiling = 16
	// DefaultInterval is the wait between one probe connection and the next.
	DefaultInterval = 250 * time.Millisecond
	// StaleAfter is the age at which a stored limit is probed again.
	StaleAfter = 30 * 24 * time.Hour

	// reserved connections are left for the user's other mail clients.
	reserved = 2
	maxPool  = 12
)

// ProbeOptions tunes Probe.
type ProbeOptions struct {
	Ceiling  int           // 0 means 16
	Interval time.Duration // 0 means 250ms
}

// Probe opens connections with dial one at a time, Interval apart, until
// one fails or Ceiling succeed, closes them all and returns how many
// succeeded. A failure of the first is returned as an error.
//
// dial should return a connection that has logged in and answered NOOP, and
// its Close should log out. Any failure after the first ends the probe: a
// connection-limit refusal, a reset and a failed login all count. Cancelling
// ctx is an error rather than a cap, because the count so far is not one.
func Probe(ctx context.Context, dial func(context.Context) (io.Closer, error), opts ProbeOptions) (int, error) {
	ceiling := opts.Ceiling
	if ceiling <= 0 {
		ceiling = DefaultCeiling
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	var conns []io.Closer
	defer func() {
		for i := len(conns) - 1; i >= 0; i-- {
			// A failed logout does not change how many connections were accepted.
			_ = conns[i].Close()
		}
	}()

	for len(conns) < ceiling {
		if len(conns) > 0 {
			if err := wait(ctx, interval); err != nil {
				return 0, fmt.Errorf("limits: probe: %w", err)
			}
		}
		c, err := dial(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, fmt.Errorf("limits: probe: %w", ctxErr)
			}
			if len(conns) == 0 {
				return 0, fmt.Errorf("limits: first connection: %w", err)
			}
			break
		}
		conns = append(conns, c)
	}
	return len(conns), nil
}

// wait sleeps for d or until ctx is done.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Limit is the stored probe result for one account.
type Limit struct {
	Account  string
	IMAPCap  int
	ProbedAt time.Time
}

// timeLayout stores probed_at as fixed-width UTC with nine fractional digits,
// as the audit table does, so the column sorts and compares correctly as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Store keeps probe results in the account_limits table of state.db.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore returns a Store. A nil now uses time.Now.
func NewStore(db *sql.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Get returns the stored limit for account. The bool is false when none is
// stored.
func (s *Store) Get(ctx context.Context, account string) (Limit, bool, error) {
	var (
		imapCap  int
		probedAt string
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT imap_cap, probed_at FROM account_limits WHERE account = ?", account,
	).Scan(&imapCap, &probedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, fmt.Errorf("limits: reading %s: %w", account, err)
	}
	at, err := time.Parse(timeLayout, probedAt)
	if err != nil {
		return Limit{}, false, fmt.Errorf("limits: reading %s: probed_at: %w", account, err)
	}
	return Limit{Account: account, IMAPCap: imapCap, ProbedAt: at.UTC()}, true, nil
}

// Put stores l, replacing any earlier limit for the account. A zero ProbedAt
// uses the store's clock.
func (s *Store) Put(ctx context.Context, l Limit) error {
	if l.Account == "" {
		return errors.New("limits: account is required")
	}
	if l.IMAPCap < 1 {
		return fmt.Errorf("limits: imap cap %d for %s must be at least 1", l.IMAPCap, l.Account)
	}
	at := l.ProbedAt
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_limits (account, imap_cap, probed_at) VALUES (?, ?, ?)
		 ON CONFLICT (account) DO UPDATE SET imap_cap = excluded.imap_cap, probed_at = excluded.probed_at`,
		l.Account, l.IMAPCap, at.UTC().Format(timeLayout))
	if err != nil {
		return fmt.Errorf("limits: storing %s: %w", l.Account, err)
	}
	return nil
}

// PoolSize picks the pool: configPool when > 0, else clamp(cap-2, 1, 12)
// when a limit is known, else presetDefault.
func PoolSize(configPool int, l *Limit, presetDefault int) int {
	if configPool > 0 {
		return configPool
	}
	if l != nil {
		return min(max(l.IMAPCap-reserved, 1), maxPool)
	}
	return presetDefault
}

// Stale reports a limit older than 30 days. A limit exactly 30 days old is
// stale, as is one with no ProbedAt.
func Stale(l Limit, now time.Time) bool {
	return !now.Before(l.ProbedAt.Add(StaleAfter))
}
