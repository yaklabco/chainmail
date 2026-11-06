package items

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// Refusals of blockers. Their text never holds request values.
var (
	errSelfBlock  = &InvalidError{Msg: "an item cannot block itself", Hint: "name a different blocked_by_id"}
	errBlockCycle = &InvalidError{Msg: "the blocker would close a cycle", Hint: "the blocker already waits on this item, directly or through others"}
)

// SetBlocker records that blockedByID blocks itemID when linked, and
// removes that when not. A link needs both items live and must not close a
// cycle; removing works on deleted items too. A change stamps both items
// with one revision and emits one blockers event for each, itemID first.
// Setting what is already stored changes nothing and returns the current
// revision with changed false.
func (s *Store) SetBlocker(ctx context.Context, a Actor, itemID, blockedByID string, linked bool) (bool, int64, error) {
	if itemID == blockedByID {
		return false, 0, errSelfBlock
	}
	now := s.now().UTC()
	var (
		changed  bool
		revision int64
	)
	err := s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		item, err := itemHead(ctx, tx, itemID, "item")
		if err != nil {
			return nil, err
		}
		blocker, err := itemHead(ctx, tx, blockedByID, "blocker")
		if err != nil {
			return nil, err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM item_blockers WHERE item_id = ? AND blocked_by_id = ?)",
			itemID, blockedByID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("items: reading a blocker: %w", err)
		}
		if exists == linked {
			revision, err = currentRevision(ctx, tx)
			return nil, err
		}
		stmt := "DELETE FROM item_blockers WHERE item_id = ? AND blocked_by_id = ?"
		if linked {
			if item.deleted || blocker.deleted {
				return nil, errItemDeleted
			}
			cycle, err := waitsOn(ctx, tx, blockedByID, itemID)
			if err != nil {
				return nil, err
			}
			if cycle {
				return nil, errBlockCycle
			}
			stmt = "INSERT INTO item_blockers (item_id, blocked_by_id) VALUES (?, ?)"
		}
		if _, err := tx.ExecContext(ctx, stmt, itemID, blockedByID); err != nil {
			return nil, fmt.Errorf("items: setting a blocker: %w", err)
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		changed = true
		var events []watch.Event
		for _, end := range []struct {
			id  string
			typ Type
		}{{itemID, item.typ}, {blockedByID, blocker.typ}} {
			if err := touch(ctx, tx, end.id, revision, now, a); err != nil {
				return nil, err
			}
			events = append(events, itemEvent(now, end.id, end.typ, revision, OpBlockers, nil))
		}
		return events, nil
	})
	if err != nil {
		return false, 0, err
	}
	return changed, revision, nil
}

// waitsOn reports whether from waits on target, directly or through other
// blockers, deleted items included. UNION keeps the walk finite.
func waitsOn(ctx context.Context, q queryer, from, target string) (bool, error) {
	var found bool
	err := q.QueryRowContext(ctx, `
		WITH RECURSIVE waits(id) AS (
			SELECT blocked_by_id FROM item_blockers WHERE item_id = ?
			UNION
			SELECT b.blocked_by_id FROM item_blockers b JOIN waits w ON b.item_id = w.id
		)
		SELECT EXISTS (SELECT 1 FROM waits WHERE id = ?)`, from, target).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("items: walking blockers: %w", err)
	}
	return found, nil
}
