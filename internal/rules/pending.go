package rules

import (
	"context"
	"database/sql"
	"fmt"
)

// ClearPending drops the act-once row of (ruleID, account, key) while it
// is still pending. A runner calls it when a message's guarded action
// failed for good before it ran, so a later run does not take the action
// as done and skip it. An applied or held row, or none, is left as it is.
// It takes the state database rather than a Store, as it needs nothing
// else of one.
func ClearPending(ctx context.Context, db *sql.DB, ruleID, account, key string) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM rule_applied WHERE rule_id = ? AND account = ? AND message_key = ? AND outcome = ?`,
		ruleID, account, key, string(AppliedPending))
	if err != nil {
		return fmt.Errorf("rules: clearing a pending act-once row: %w", err)
	}
	return nil
}
