package rules

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// dueOpenItems selects the open items that have a due date: not done and
// not deleted. A live item never sits under a deleted one, so deleted_at
// alone decides whether an item is live.
const dueOpenItems = "SELECT id, type, state, due_date, parent_id FROM items " +
	"WHERE due_date IS NOT NULL AND state <> 'done' AND deleted_at IS NULL"

// DueChecker writes the item_due_soon, item_due and item_overdue events.
// For each open item it fires the most advanced stage the item's due date
// has reached, once per (item, due date, stage), as item_due_sent records.
// Firing a stage also records every earlier stage for that due date, so an
// item that first qualifies late gets one event and never a stale one after.
// Due events carry the item's ID, type, due date, state and parent, never
// its text, and no account.
//
// state.db has one connection, so a DueChecker method never calls another
// Log or Store method while it holds a transaction.
type DueChecker struct {
	db       *sql.DB
	log      *watch.Log
	now      func() time.Time
	settings DueSettings
}

// NewDueChecker returns a DueChecker over db, which must hold the state
// schema, that appends due events to log, the daemon's one shared Log. now
// is the time Scan checks at; nil uses time.Now. A nil s.Location means
// time.Local.
func NewDueChecker(db *sql.DB, log *watch.Log, now func() time.Time, s DueSettings) *DueChecker {
	if now == nil {
		now = time.Now
	}
	if s.Location == nil {
		s.Location = time.Local
	}
	return &DueChecker{db: db, log: log, now: now, settings: s}
}

// Scan checks every open item with a due date in one transaction, writes
// the stage events not yet in item_due_sent, and returns how many it wrote.
// A scan that fires nothing writes nothing.
func (d *DueChecker) Scan(ctx context.Context) (int, error) {
	return d.scan(ctx, d.now())
}

// scan is Scan at now.
func (d *DueChecker) scan(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rules: beginning a due scan: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	open, err := dueItems(ctx, tx, dueOpenItems+" ORDER BY due_date, id")
	if err != nil {
		return 0, err
	}
	events, err := d.fire(ctx, tx, open, now)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil // nothing recorded; the rollback ends the read
	}
	if err := d.log.Append(ctx, tx, events); err != nil {
		return 0, fmt.Errorf("rules: appending due events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rules: committing a due scan: %w", err)
	}
	d.log.Notify()
	return len(events), nil
}

// dueItem is what a due event needs to know about one open item.
type dueItem struct {
	id, typ, state, dueDate, parentID string
}

// dueItems reads the items query selects, in dueOpenItems' columns, and
// closes the rows before it returns, so the caller may query again.
func dueItems(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]dueItem, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rules: reading items with due dates: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []dueItem
	for rows.Next() {
		var (
			it     dueItem
			parent sql.NullString
		)
		if err := rows.Scan(&it.id, &it.typ, &it.state, &it.dueDate, &parent); err != nil {
			return nil, fmt.Errorf("rules: reading an item with a due date: %w", err)
		}
		it.parentID = parent.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rules: reading items with due dates: %w", err)
	}
	return out, nil
}

// fire records in tx the stage each of open has reached at now, with every
// earlier stage, and returns one event for each stage that was not yet
// recorded.
func (d *DueChecker) fire(ctx context.Context, tx *sql.Tx, open []dueItem, now time.Time) ([]watch.Event, error) {
	if len(open) == 0 {
		return nil, nil
	}
	stmt, err := tx.PrepareContext(ctx,
		"INSERT OR IGNORE INTO item_due_sent (item_id, due_date, stage, at) VALUES (?, ?, ?, ?)")
	if err != nil {
		return nil, fmt.Errorf("rules: preparing a due record: %w", err)
	}
	defer func() { _ = stmt.Close() }() // closing a prepared statement only frees it
	at := watch.FormatTime(now)
	var events []watch.Event
	for _, it := range open {
		stage, ok := DueStage(it.dueDate, now, d.settings)
		if !ok {
			continue
		}
		res, err := stmt.ExecContext(ctx, it.id, it.dueDate, string(stage), at)
		if err != nil {
			return nil, fmt.Errorf("rules: recording a due stage: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("rules: recording a due stage: %w", err)
		}
		if n == 0 {
			continue // fired before, or passed when a later stage fired
		}
		for _, earlier := range dueStagesBefore(stage) {
			if _, err := stmt.ExecContext(ctx, it.id, it.dueDate, string(earlier), at); err != nil {
				return nil, fmt.Errorf("rules: recording a due stage: %w", err)
			}
		}
		events = append(events, watch.Event{
			At:   now,
			Kind: stage,
			Due: &watch.ItemDue{
				ItemID: it.id, Type: it.typ, DueDate: it.dueDate, State: it.state, ParentID: it.parentID,
			},
		})
	}
	return events, nil
}

// dueStagesBefore lists the stages that come before k.
func dueStagesBefore(k watch.Kind) []watch.Kind {
	switch k {
	case watch.KindItemOverdue:
		return []watch.Kind{watch.KindItemDueSoon, watch.KindItemDue}
	case watch.KindItemDue:
		return []watch.Kind{watch.KindItemDueSoon}
	default:
		return nil
	}
}
