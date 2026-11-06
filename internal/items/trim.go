package items

import (
	"context"
	"fmt"
	"time"
)

// Trim deletes item_mail_seen rows older than seenBefore and item_requests
// rows older than now minus RequestRetention.
//
// Both deletes run in one transaction, and Trim returns how many rows they
// removed together. The daemon runs it hourly beside the event log's trim,
// with seenBefore the log's cutoff from [watch] retention. Once a seen row is
// trimmed, a copy of that message arriving later in another folder fires
// item_mail again, as the event log's own retention already allows.
func (s *Store) Trim(ctx context.Context, seenBefore time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("items: beginning the item trim: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	var total int64
	for _, del := range []struct {
		query  string
		before time.Time
	}{
		{"DELETE FROM item_mail_seen WHERE at < ?", seenBefore},
		{"DELETE FROM item_requests WHERE at < ?", s.now().Add(-RequestRetention)},
	} {
		res, err := tx.ExecContext(ctx, del.query, formatTime(del.before))
		if err != nil {
			return 0, fmt.Errorf("items: trimming items: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("items: trimming items: %w", err)
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("items: committing the item trim: %w", err)
	}
	return total, nil
}
