package rules

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// nextRun is when a rule with definition d next runs after after: the
// schedule's next instant in the store's location for an enabled rule with
// a schedule, else the zero time, which is stored as NULL.
func (s *Store) nextRun(d Definition, after time.Time) time.Time {
	if !d.Enabled || d.Schedule.Kind == ScheduleNone {
		return time.Time{}
	}
	return d.Schedule.Next(after, s.loc).UTC()
}

// DueScheduled returns the live, enabled rules whose next scheduled run is
// at or before now, in next_run_at, then priority, then ID order. A rule
// whose run time passed while the daemon was down is returned once: its
// finished run plans the next one from the finish time.
func (s *Store) DueScheduled(ctx context.Context, now time.Time) ([]Rule, error) {
	var out []Rule
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = queryRules(ctx, tx,
			"SELECT "+ruleColumns+" FROM rules WHERE deleted_at IS NULL AND enabled = 1 AND "+
				"next_run_at IS NOT NULL AND next_run_at <= ? ORDER BY next_run_at, priority, id",
			dbTime(now))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// NextScheduled returns the earliest next run of a live, enabled rule, and
// false when no such rule has a schedule.
func (s *Store) NextScheduled(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullString
	if err := s.db.QueryRowContext(ctx,
		"SELECT min(next_run_at) FROM rules WHERE deleted_at IS NULL AND enabled = 1 AND next_run_at IS NOT NULL").
		Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("rules: reading the next scheduled run: %w", err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	at, err := parseDBTime(next.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}
