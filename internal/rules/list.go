package rules

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ListQuery selects rules. Empty fields match everything.
type ListQuery struct {
	Account string // rules covering this account (explicitly, or all accounts)
	Enabled *bool
	Deleted bool // true: deleted rules only
}

// coversAccount matches a rules row that names the bound account or names
// none, meaning every account.
const coversAccount = "(json_array_length(accounts) = 0 OR EXISTS (SELECT 1 FROM json_each(rules.accounts) WHERE value = ?))"

// List returns the rules q selects, live ones unless q.Deleted, in
// priority then ID order.
func (s *Store) List(ctx context.Context, q ListQuery) ([]Rule, error) {
	var (
		b    strings.Builder
		args []any
	)
	b.WriteString("SELECT " + ruleColumns + " FROM rules WHERE ")
	if q.Deleted {
		b.WriteString("deleted_at IS NOT NULL")
	} else {
		b.WriteString("deleted_at IS NULL")
	}
	if q.Account != "" {
		b.WriteString(" AND " + coversAccount)
		args = append(args, q.Account)
	}
	if q.Enabled != nil {
		b.WriteString(" AND enabled = ?")
		args = append(args, sqlBool(*q.Enabled))
	}
	b.WriteString(" ORDER BY priority, id")
	var out []Rule
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = queryRules(ctx, tx, b.String(), args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EnabledOnNew returns the live, enabled on_new rules covering account, in
// priority then ID order: the rules a new-mail batch on account runs.
func (s *Store) EnabledOnNew(ctx context.Context, account string) ([]Rule, error) {
	var out []Rule
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = queryRules(ctx, tx,
			"SELECT "+ruleColumns+" FROM rules WHERE deleted_at IS NULL AND enabled = 1 AND on_new = 1 AND "+
				coversAccount+" ORDER BY priority, id", account)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Revisions returns the stored changes of the rule with ruleID, newest
// first, at most limit of them, or every one when limit <= 0. ruleID must
// be an ID, live or deleted. Not found is ErrNotFound.
func (s *Store) Revisions(ctx context.Context, ruleID string, limit int) ([]Revision, error) {
	if !ValidID(ruleID) {
		return nil, ErrNotFound
	}
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	var out []Revision
	err := s.read(ctx, func(tx *sql.Tx) error {
		if _, err := findRuleByID(ctx, tx, ruleID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT revision, change, rule, actor, at FROM rule_revisions WHERE rule_id = ? ORDER BY revision DESC LIMIT ?",
			ruleID, limit)
		if err != nil {
			return fmt.Errorf("rules: reading revisions: %w", err)
		}
		defer func() { _ = rows.Close() }() // rows.Err below reports any read failure
		for rows.Next() {
			var (
				rev      Revision
				body, at string
			)
			if err := rows.Scan(&rev.Revision, &rev.Change, &body, &rev.Actor, &at); err != nil {
				return fmt.Errorf("rules: reading a revision: %w", err)
			}
			if rev.Rule, err = decodeRevision(body); err != nil {
				return err
			}
			if rev.At, err = parseDBTime(at); err != nil {
				return err
			}
			out = append(out, rev)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rules: reading revisions: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// queryRules runs a SELECT of ruleColumns through q and reads every row.
func queryRules(ctx context.Context, q queryer, query string, args ...any) ([]Rule, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rules: listing rules: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any read failure
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rules: listing rules: %w", err)
	}
	return out, nil
}
