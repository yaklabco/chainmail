package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// dayLayout is the budget day: a UTC date.
const dayLayout = "2006-01-02"

// checkDay refuses a day that is not a dayLayout date.
func checkDay(day string) error {
	if _, err := time.Parse(dayLayout, day); err != nil {
		return fmt.Errorf("budget day %q is not YYYY-MM-DD", day)
	}
	return nil
}

// AddBudget adds bytes to the account's total for day and returns the new total.
func (s *Store) AddBudget(ctx context.Context, account, day string, bytes int64) (int64, error) {
	if err := checkDay(day); err != nil {
		return 0, err
	}
	if bytes < 0 {
		return 0, fmt.Errorf("budget bytes %d must not be negative", bytes)
	}
	var total int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO budget (account, day, bytes) VALUES (?, ?, ?)
		 ON CONFLICT (account, day) DO UPDATE SET bytes = budget.bytes + excluded.bytes
		 RETURNING bytes`,
		account, day, bytes).Scan(&total)
	if err != nil {
		return 0, fail("adding to the budget", err)
	}
	return total, nil
}

// Budget returns the bytes the account has spent on day, 0 when none.
func (s *Store) Budget(ctx context.Context, account, day string) (int64, error) {
	if err := checkDay(day); err != nil {
		return 0, err
	}
	var total int64
	err := s.db.QueryRowContext(ctx, "SELECT bytes FROM budget WHERE account = ? AND day = ?", account, day).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fail("reading the budget", err)
	}
	return total, nil
}
