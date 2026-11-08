package rules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// Refusals of run queries. Their text never holds request values.
var (
	errRunLimit   = &InvalidError{Msg: "limit must be 1 to 500", Hint: "pass 0 for the default of 50"}
	errRunBefore  = &InvalidError{Msg: "before_id must be zero or more", Hint: "pass next_before_id from the previous page"}
	errRunTrigger = &InvalidError{Msg: "unknown trigger", Hint: "triggers are new, schedule and manual"}
	errRunOutcome = &InvalidError{Msg: "unknown outcome", Hint: "outcomes are ok, capped, failed, cache_not_ready and interrupted"}
)

// errUnknownRun reports a FinishRun for a run ID BeginRun never returned.
var errUnknownRun = errors.New("rules: unknown run")

// Run limits and the auto-disable threshold.
const (
	defaultRunLimit  = 50
	maxRunLimit      = 500
	autoDisableAfter = 5
)

// cacheRetryAfter is how soon a scheduled run the cache could not answer
// runs again (contract amendment 7), so a start-up catch-up that meets a
// stale cache before the first sync pass does not wait a whole period.
const cacheRetryAfter = 5 * time.Minute

// runReasonCacheDisabled is the cache_not_ready reason of an account with
// no cache. Retrying soon cannot help it, so its schedule keeps its period.
const runReasonCacheDisabled = "cache_disabled"

// runnerClient is the client kind rule actions run as (spec amendment 7).
const runnerClient = "rule"

// Run is one rule run on one account.
type Run struct {
	ID                                      int64
	RuleID, Account                         string
	Trigger                                 Trigger
	StartedAt, FinishedAt                   time.Time
	Matched, Applied, Held, Skipped, Failed int
	Outcome                                 Outcome
	Reason, Hint                            string
	// Unattempted is how many of Failed failed before any action was
	// attempted, such as a text rule's message whose body the cache did not
	// hold in time (body_not_cached). They count in Matched and Failed but
	// never toward the all_failed auto-disable. It is not stored: a run read
	// back has 0.
	Unattempted int
}

// RunQuery selects runs, newest first. Empty fields match everything.
type RunQuery struct {
	RuleID, Account string
	Trigger         Trigger
	Outcome         Outcome
	Limit           int   // 1 to 500, 0 means 50
	BeforeID        int64 // 0: newest
}

// Counts are totals over a rule's retained runs.
type Counts struct{ Runs, Matched, Applied, Held, Failed int64 }

// runColumns are the rule_runs columns, in the order scanRun reads them.
const runColumns = "id, rule_id, account, trigger_kind, started_at, finished_at, matched, applied, held, " +
	"skipped, failed, outcome, reason, hint"

// BeginRun inserts a run of ruleID on account, started and finished at at
// with outcome interrupted, so a crash before FinishRun leaves an honest
// row, and returns its ID.
func (s *Store) BeginRun(ctx context.Context, ruleID, account string, t Trigger, at time.Time) (int64, error) {
	if !knownTrigger(t) {
		return 0, errRunTrigger
	}
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO rule_runs (rule_id, account, trigger_kind, started_at, finished_at, outcome)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			ruleID, account, string(t), dbTime(at), dbTime(at), string(OutcomeInterrupted))
		if err != nil {
			return nil, fmt.Errorf("rules: beginning a run: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return nil, fmt.Errorf("rules: beginning a run: %w", err)
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// FinishRun overwrites run r.ID with r's finish time, counts, outcome,
// reason and hint, and updates its rule in the same transaction. The rule
// ID and trigger come from the stored run. failed_runs goes up by one when
// r.Failed > 0, and back to 0 when r.Failed == 0 and the outcome is ok or
// capped. A live, enabled rule is disabled with ReasonAllFailed when every
// match not skipped failed, r.Unattempted aside, with at least one acted-on
// failure, or with ReasonRepeatedFailures when failed_runs
// reaches 5. The disable is a change: a new revision with its row (change
// disabled, actor "<daemon uid>/rule") and a rule event with the reason. A
// scheduled run sets last_run_at to its finish time and plans next_run_at
// as nextScheduledRun says. A zero FinishedAt means now. disabled reports
// an auto-disable.
func (s *Store) FinishRun(ctx context.Context, r Run) (bool, error) {
	if !knownOutcome(r.Outcome) {
		return false, errRunOutcome
	}
	now := s.now().UTC()
	if r.FinishedAt.IsZero() {
		r.FinishedAt = now
	}
	disabled := false
	err := s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		var ruleID, trigger string
		err := tx.QueryRowContext(ctx, "SELECT rule_id, trigger_kind FROM rule_runs WHERE id = ?", r.ID).
			Scan(&ruleID, &trigger)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUnknownRun
		}
		if err != nil {
			return nil, fmt.Errorf("rules: reading a run: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rule_runs SET finished_at = ?, matched = ?, applied = ?, held = ?, skipped = ?, failed = ?,
			   outcome = ?, reason = ?, hint = ? WHERE id = ?`,
			dbTime(r.FinishedAt), r.Matched, r.Applied, r.Held, r.Skipped, r.Failed,
			string(r.Outcome), r.Reason, r.Hint, r.ID); err != nil {
			return nil, fmt.Errorf("rules: finishing a run: %w", err)
		}
		cur, err := findRuleByID(ctx, tx, ruleID)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		next := cur
		switch {
		case r.Failed > 0:
			next.FailedRuns++
		case r.Outcome == OutcomeOK || r.Outcome == OutcomeCapped:
			next.FailedRuns = 0
		}
		if Trigger(trigger) == TriggerSchedule {
			next.LastRunAt = r.FinishedAt.UTC()
			next.NextRunAt = s.nextScheduledRun(cur, r)
		}
		reason := disableReason(cur, next.FailedRuns, r)
		if reason == "" {
			return nil, saveRule(ctx, tx, next)
		}
		actor := runnerActor().String()
		next.Enabled, next.DisabledReason, next.NextRunAt = false, reason, time.Time{}
		next.Revision++
		next.UpdatedAt, next.UpdatedBy = now, actor
		_, events, err := commitChange(ctx, tx, next, ChangeDisabled, actor, reason, now)
		if err != nil {
			return nil, err
		}
		disabled = true
		return events, nil
	})
	if err != nil {
		return false, err
	}
	return disabled, nil
}

// nextScheduledRun is the next_run_at a finished scheduled run r leaves on
// rule cur: none for a deleted rule, FinishedAt plus cacheRetryAfter when
// the cache could not answer for a reason other than cache_disabled, and
// otherwise the schedule's next instant after FinishedAt. A disabled or
// unscheduled rule gets none in every case.
func (s *Store) nextScheduledRun(cur Rule, r Run) time.Time {
	if !cur.DeletedAt.IsZero() {
		return time.Time{}
	}
	next := s.nextRun(cur.Definition, r.FinishedAt)
	if next.IsZero() || r.Outcome != OutcomeCacheNotReady || r.Reason == runReasonCacheDisabled {
		return next
	}
	return r.FinishedAt.Add(cacheRetryAfter).UTC()
}

// disableReason is why run r disables rule cur, which has failedRuns
// consecutive failing runs counting r, or "" when it does not. Only a live,
// enabled rule is disabled, and only by a run with a failure. all_failed
// needs every acted-on match to have failed: unattempted failures are left
// out of both sides, and a run with only those never disables for it.
func disableReason(cur Rule, failedRuns int, r Run) string {
	switch {
	case !cur.Enabled || !cur.DeletedAt.IsZero() || r.Failed == 0:
		return ""
	case r.Failed > r.Unattempted && r.Failed == r.Matched-r.Skipped:
		return ReasonAllFailed
	case failedRuns >= autoDisableAfter:
		return ReasonRepeatedFailures
	}
	return ""
}

// runnerActor is who auto-disables a rule: the daemon's own user with
// client kind rule, as rule actions run.
func runnerActor() items.Actor {
	return items.Actor{UID: uint32(os.Getuid()), Client: runnerClient}
}

// ListRuns returns the runs q selects, newest first, and the BeforeID of
// the next page, 0 after the last.
func (s *Store) ListRuns(ctx context.Context, q RunQuery) ([]Run, int64, error) {
	limit, err := checkRunQuery(q)
	if err != nil {
		return nil, 0, err
	}
	var (
		b    strings.Builder
		args []any
	)
	b.WriteString("SELECT " + runColumns + " FROM rule_runs WHERE 1 = 1")
	filters := []struct{ col, val string }{
		{"rule_id", q.RuleID},
		{"account", q.Account},
		{"trigger_kind", string(q.Trigger)},
		{"outcome", string(q.Outcome)},
	}
	for _, f := range filters {
		if f.val != "" {
			b.WriteString(" AND " + f.col + " = ?")
			args = append(args, f.val)
		}
	}
	if q.BeforeID > 0 {
		b.WriteString(" AND id < ?")
		args = append(args, q.BeforeID)
	}
	b.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)
	var runs []Run
	err = s.read(ctx, func(tx *sql.Tx) error {
		var err error
		runs, err = queryRuns(ctx, tx, b.String(), args...)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(runs) > limit {
		runs = runs[:limit]
		next = runs[limit-1].ID
	}
	return runs, next, nil
}

// checkRunQuery refuses a bad page size, position or filter and returns the
// page size.
func checkRunQuery(q RunQuery) (int, error) {
	switch {
	case q.Limit < 0 || q.Limit > maxRunLimit:
		return 0, errRunLimit
	case q.BeforeID < 0:
		return 0, errRunBefore
	case q.Trigger != "" && !knownTrigger(q.Trigger):
		return 0, errRunTrigger
	case q.Outcome != "" && !knownOutcome(q.Outcome):
		return 0, errRunOutcome
	}
	if q.Limit == 0 {
		return defaultRunLimit, nil
	}
	return q.Limit, nil
}

// RecentRuns returns the newest n runs of ruleID, newest first.
func (s *Store) RecentRuns(ctx context.Context, ruleID string, n int) ([]Run, error) {
	if n <= 0 {
		return nil, nil
	}
	var runs []Run
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		runs, err = queryRuns(ctx, tx,
			"SELECT "+runColumns+" FROM rule_runs WHERE rule_id = ? ORDER BY id DESC LIMIT ?", ruleID, n)
		return err
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

// Counts returns the totals over ruleID's retained runs.
func (s *Store) Counts(ctx context.Context, ruleID string) (Counts, error) {
	var c Counts
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(matched), 0), COALESCE(sum(applied), 0), COALESCE(sum(held), 0),
		   COALESCE(sum(failed), 0) FROM rule_runs WHERE rule_id = ?`, ruleID).
		Scan(&c.Runs, &c.Matched, &c.Applied, &c.Held, &c.Failed); err != nil {
		return Counts{}, fmt.Errorf("rules: counting runs: %w", err)
	}
	return c, nil
}

// TrimRuns deletes the runs started before before and returns how many.
// The daemon calls it from its trim loop with a 30-day cutoff.
func (s *Store) TrimRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM rule_runs WHERE started_at < ?", dbTime(before))
	if err != nil {
		return 0, fmt.Errorf("rules: trimming runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rules: trimming runs: %w", err)
	}
	return n, nil
}

// queryRuns runs a SELECT of runColumns through q and reads every row.
func queryRuns(ctx context.Context, q queryer, query string, args ...any) ([]Run, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rules: listing runs: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any read failure
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rules: listing runs: %w", err)
	}
	return out, nil
}

// scanRun reads one row of runColumns.
func scanRun(sc scanner) (Run, error) {
	var (
		r                                   Run
		trigger, outcome, started, finished string
	)
	if err := sc.Scan(&r.ID, &r.RuleID, &r.Account, &trigger, &started, &finished, &r.Matched, &r.Applied,
		&r.Held, &r.Skipped, &r.Failed, &outcome, &r.Reason, &r.Hint); err != nil {
		return Run{}, fmt.Errorf("rules: reading a run: %w", err)
	}
	r.Trigger, r.Outcome = Trigger(trigger), Outcome(outcome)
	var err error
	if r.StartedAt, err = parseDBTime(started); err != nil {
		return Run{}, err
	}
	if r.FinishedAt, err = parseDBTime(finished); err != nil {
		return Run{}, err
	}
	return r, nil
}

// knownTrigger reports whether t is one of the three triggers.
func knownTrigger(t Trigger) bool {
	return slices.Contains([]Trigger{TriggerNew, TriggerSchedule, TriggerManual}, t)
}

// knownOutcome reports whether o is one of the five run outcomes.
func knownOutcome(o Outcome) bool {
	return slices.Contains([]Outcome{OutcomeOK, OutcomeCapped, OutcomeFailed, OutcomeCacheNotReady, OutcomeInterrupted}, o)
}
