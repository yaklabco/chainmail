package rules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// appliedChunk is how many keys one Applied query binds.
const appliedChunk = 500

// errAppliedOutcome reports a MarkApplied with an outcome other than
// applied or held. Pending rows are written by MarkPending only.
var errAppliedOutcome = errors.New("rules: a message is marked applied or held, never pending")

// AppliedRow is a rule's act-once row for one message.
type AppliedRow struct {
	Outcome AppliedOutcome
	Note    string
	RunID   int64
	At      time.Time
}

// Applied returns the act-once rows of ruleID on account for the given
// message keys, by key. Keys without a row are absent. It queries 500 keys
// at a time, in one read transaction, and always returns a non-nil map.
func (s *Store) Applied(ctx context.Context, ruleID, account string, keys []string) (map[string]AppliedRow, error) {
	out := make(map[string]AppliedRow)
	if len(keys) == 0 {
		return out, nil
	}
	err := s.read(ctx, func(tx *sql.Tx) error {
		for chunk := range slices.Chunk(keys, appliedChunk) {
			args := make([]any, 0, len(chunk)+2)
			args = append(args, ruleID, account)
			for _, k := range chunk {
				args = append(args, k)
			}
			query := "SELECT message_key, outcome, note, run_id, at FROM rule_applied " +
				"WHERE rule_id = ? AND account = ? AND message_key IN (" + placeholders(len(chunk)) + ")"
			if err := scanApplied(ctx, tx, query, args, out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanApplied runs one act-once query through q and adds its rows to into.
func scanApplied(ctx context.Context, q queryer, query string, args []any, into map[string]AppliedRow) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("rules: reading act-once rows: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any read failure
	for rows.Next() {
		var (
			row          AppliedRow
			key, outcome string
			at           string
		)
		if err := rows.Scan(&key, &outcome, &row.Note, &row.RunID, &at); err != nil {
			return fmt.Errorf("rules: reading an act-once row: %w", err)
		}
		row.Outcome = AppliedOutcome(outcome)
		if row.At, err = parseDBTime(at); err != nil {
			return err
		}
		into[key] = row
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rules: reading act-once rows: %w", err)
	}
	return nil
}

// MarkPending writes a pending row for ruleID's guarded action (forward or
// copy) on one message before the action runs, so a crash replay knows the
// action may have happened. An existing row of any outcome is left as it
// is, so a pending row keeps the run and time of the first attempt.
func (s *Store) MarkPending(ctx context.Context, ruleID, account, key string, runID int64) error {
	now := s.now().UTC()
	return s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rule_applied (rule_id, account, message_key, run_id, outcome, note, at)
			 VALUES (?, ?, ?, ?, ?, '', ?)
			 ON CONFLICT (rule_id, account, message_key) DO NOTHING`,
			ruleID, account, key, runID, string(AppliedPending), dbTime(now)); err != nil {
			return nil, fmt.Errorf("rules: marking a message pending: %w", err)
		}
		return nil, nil
	})
}

// MarkApplied upserts ruleID's row for one message with outcome applied or
// held, the note, the run and now. For each alias that is set and differs
// from key (the message's key after a move, archive, trash or copy), it
// then upserts ruleID's row at the alias with the same values, which a
// runner may have written right after a copy, and copies every other
// rule's row for (account, key) to (account, alias) with INSERT OR IGNORE,
// so another rule's row already at the alias is never overwritten and the
// copies keep their run, outcome, note and time. Every row commits in one
// transaction, or none does (spec amendment 6).
func (s *Store) MarkApplied(ctx context.Context, ruleID, account, key string, runID int64, o AppliedOutcome, note string, aliases ...string) error {
	if o != AppliedDone && o != AppliedHeld {
		return errAppliedOutcome
	}
	now := s.now().UTC()
	return s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		if err := upsertApplied(ctx, tx, ruleID, account, key, runID, o, note, now); err != nil {
			return nil, err
		}
		for _, alias := range aliases {
			if alias == "" || alias == key {
				continue
			}
			if err := upsertApplied(ctx, tx, ruleID, account, alias, runID, o, note, now); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO rule_applied (rule_id, account, message_key, run_id, outcome, note, at)
				 SELECT rule_id, account, ?, run_id, outcome, note, at FROM rule_applied
				 WHERE account = ? AND message_key = ?`,
				alias, account, key); err != nil {
				return nil, fmt.Errorf("rules: copying act-once rows to a new key: %w", err)
			}
		}
		return nil, nil
	})
}

// upsertApplied writes ruleID's row for key with outcome o, the note, the
// run and now, replacing a row already there.
func upsertApplied(ctx context.Context, tx *sql.Tx, ruleID, account, key string, runID int64, o AppliedOutcome, note string, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO rule_applied (rule_id, account, message_key, run_id, outcome, note, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (rule_id, account, message_key) DO UPDATE SET
		   run_id = excluded.run_id, outcome = excluded.outcome, note = excluded.note, at = excluded.at`,
		ruleID, account, key, runID, string(o), note, dbTime(now)); err != nil {
		return fmt.Errorf("rules: marking a message applied: %w", err)
	}
	return nil
}
