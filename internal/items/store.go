package items

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// Item is one work item as stored, with its effective area and priority:
// its own values, or those of the nearest ancestor that sets them, and
// DefaultPriority when nothing on the way to the root sets a priority.
// Title and Description are untrusted text.
type Item struct {
	ID                   string
	Type                 Type
	Title, Description   string
	State                State
	Area                 Area
	Priority             Priority
	EffectiveArea        Area
	EffectivePriority    Priority
	StartDate, DueDate   string
	Estimate             *float64
	ParentID             string
	Ongoing              bool
	DeletedAt            time.Time // zero while live
	Revision             int64
	CreatedAt, UpdatedAt time.Time
	CreatedBy, UpdatedBy string
}

// Criterion is one acceptance criterion of an item. Text and Evidence are
// untrusted text.
type Criterion struct {
	ID       string
	Position int
	Text     string
	Ticked   bool
	Evidence string
	TickedAt time.Time
	TickedBy string
}

// Row is an item as lists show it, with the counts beside it. Rows leave
// Description empty.
type Row struct {
	Item
	Depth          int // levels below the top of the listed scope
	Children       int // live direct children
	CriteriaTotal  int
	CriteriaTicked int
	Links          int
}

// Store keeps work items in state.db. Every write runs in one transaction
// that also appends the item events it causes to the watch log, so the log
// never shows a change that did not commit, nor misses one that did.
//
// state.db has one connection, so a Store method does all its work through
// one transaction and never calls another public method while it holds it.
// A Store is safe for concurrent use: the connection serializes callers.
type Store struct {
	db  *sql.DB
	log *watch.Log
	now func() time.Time
	// rnd feeds NewID. Nil uses crypto/rand.
	rnd io.Reader
}

// NewStore returns a Store over db, which must hold the state schema, that
// appends item events to log, the daemon's one shared Log. now stamps
// changes; nil uses time.Now.
func NewStore(db *sql.DB, log *watch.Log, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, log: log, now: now}
}

// queryer is what *sql.DB and *sql.Tx share for reads.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanner is a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// write runs fn in one transaction. fn calls rev when it changes something:
// the first call increments the global revision and returns it, and later
// calls return the same value, so a transaction stamps every item it touches
// with one revision. When fn returns without having called rev, nothing is
// written and the transaction rolls back. Otherwise fn's events, which carry
// that revision, are appended, the transaction commits, and the log is
// notified. fn may use only tx.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("items: beginning a write: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	var revision int64
	rev := func() (int64, error) {
		if revision != 0 {
			return revision, nil
		}
		var v int64
		if err := tx.QueryRowContext(ctx,
			"UPDATE item_revision SET value = value + 1 WHERE id = 1 RETURNING value").Scan(&v); err != nil {
			return 0, fmt.Errorf("items: bumping the revision: %w", err)
		}
		revision = v
		return v, nil
	}
	events, err := fn(tx, rev)
	if err != nil {
		return err
	}
	if revision == 0 {
		if len(events) > 0 {
			return errors.New("items: events from a write that changed nothing")
		}
		return nil
	}
	if len(events) > 0 { // EnsureAdmin bumps the revision and appends nothing
		if err := s.log.Append(ctx, tx, events); err != nil {
			return fmt.Errorf("items: appending item events: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("items: committing a write: %w", err)
	}
	if len(events) > 0 {
		s.log.Notify()
	}
	return nil
}

// read runs fn in one transaction, so everything fn reads is one view.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("items: beginning a read: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only; nothing to keep
	return fn(tx)
}

// Revision returns the global revision: the revision of the latest change.
func (s *Store) Revision(ctx context.Context) (int64, error) {
	return currentRevision(ctx, s.db)
}

// currentRevision reads the global revision through q.
func currentRevision(ctx context.Context, q queryer) (int64, error) {
	var v int64
	if err := q.QueryRowContext(ctx, "SELECT value FROM item_revision WHERE id = 1").Scan(&v); err != nil {
		return 0, fmt.Errorf("items: reading the revision: %w", err)
	}
	return v, nil
}

// itemEvent is the event for one changed item. It carries the item's ID,
// type and changed field names, never item text, and no account.
func itemEvent(at time.Time, id string, t Type, revision int64, op string, fields []string) watch.Event {
	return watch.Event{
		At:   at,
		Kind: watch.KindItem,
		Item: &watch.ItemChange{ItemID: id, Type: string(t), Revision: revision, Op: op, Fields: fields},
	}
}

// errMissing is ErrNotFound naming what is missing: item, parent,
// criterion, blocker or link. It never names an ID.
func errMissing(what string) error {
	return fmt.Errorf("%w: %s", ErrNotFound, what)
}

// itemColumns are the items columns scanItem reads, in its order. The
// description is read only for single items, by withDescription.
const itemColumns = "id, type, title, state, area, priority, start_date, due_date, estimate, parent_id, " +
	"ongoing, deleted_at, revision, created_at, updated_at, created_by, updated_by"

// scanItem reads one row of itemColumns. Effective values are left unset.
func scanItem(sc scanner) (Item, error) {
	var (
		it                                 Item
		typ, state, created, updated       string
		area, priority, start, due, parent sql.NullString
		deleted                            sql.NullString
		estimate                           sql.NullFloat64
		ongoing                            int
	)
	if err := sc.Scan(&it.ID, &typ, &it.Title, &state, &area, &priority, &start, &due, &estimate, &parent,
		&ongoing, &deleted, &it.Revision, &created, &updated, &it.CreatedBy, &it.UpdatedBy); err != nil {
		return Item{}, fmt.Errorf("items: reading an item: %w", err)
	}
	it.Type, it.State = Type(typ), State(state)
	it.Area, it.Priority = Area(area.String), Priority(priority.String)
	it.StartDate, it.DueDate, it.ParentID = start.String, due.String, parent.String
	it.Ongoing = ongoing != 0
	if estimate.Valid {
		v := estimate.Float64
		it.Estimate = &v
	}
	var err error
	if deleted.Valid {
		if it.DeletedAt, err = parseTime(deleted.String); err != nil {
			return Item{}, fmt.Errorf("items: reading an item: %w", err)
		}
	}
	if it.CreatedAt, err = parseTime(created); err != nil {
		return Item{}, fmt.Errorf("items: reading an item: %w", err)
	}
	if it.UpdatedAt, err = parseTime(updated); err != nil {
		return Item{}, fmt.Errorf("items: reading an item: %w", err)
	}
	return it, nil
}

// insertItem inserts it, live, in tx.
func insertItem(ctx context.Context, tx *sql.Tx, it Item) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO items (id, type, title, description, state, area, priority, start_date, due_date,
		   estimate, parent_id, ongoing, revision, created_at, updated_at, created_by, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, string(it.Type), it.Title, it.Description, string(it.State),
		nullText(string(it.Area)), nullText(string(it.Priority)), nullText(it.StartDate), nullText(it.DueDate),
		nullFloat(it.Estimate), nullText(it.ParentID), sqlBool(it.Ongoing), it.Revision,
		formatTime(it.CreatedAt), formatTime(it.UpdatedAt), it.CreatedBy, it.UpdatedBy)
	if err != nil {
		return fmt.Errorf("items: inserting an item: %w", err)
	}
	return nil
}

// nullText stores "" as NULL, which the nullable text columns use for unset.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullFloat stores a nil estimate as NULL.
func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// sqlBool renders b as the 0 or 1 the integer flag columns hold.
func sqlBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// head is what a check needs to know about one item without loading the tree.
type head struct {
	typ     Type
	deleted bool
}

// itemHead reads the head of id through q, or ErrNotFound naming what.
func itemHead(ctx context.Context, q queryer, id, what string) (head, error) {
	var (
		typ     string
		deleted int
	)
	err := q.QueryRowContext(ctx, "SELECT type, deleted_at IS NOT NULL FROM items WHERE id = ?", id).Scan(&typ, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return head{}, errMissing(what)
	}
	if err != nil {
		return head{}, fmt.Errorf("items: reading an item: %w", err)
	}
	return head{typ: Type(typ), deleted: deleted != 0}, nil
}
