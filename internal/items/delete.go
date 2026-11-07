package items

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// Refusals of delete and restore. Their text never holds request values.
var (
	errHasChildren = &InvalidError{Msg: "the item has live children", Hint: "pass cascade to delete the whole subtree"}
	errNotDeleted  = &InvalidError{Msg: "the item is not deleted", Hint: "only a deleted item can be restored"}
)

// Delete soft-deletes the item and returns the IDs it deleted, in tree
// order, with the revision. An item with live children needs cascade, which
// deletes the whole live subtree at one instant and one revision, with one
// deleted event per item. Ongoing items are never deleted. Deleting an item
// that is already deleted changes nothing and returns no IDs.
func (s *Store) Delete(ctx context.Context, a Actor, id string, cascade bool) ([]string, int64, error) {
	now := s.now().UTC()
	var (
		ids      []string
		revision int64
	)
	err := s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		f, err := loadForest(ctx, tx)
		if err != nil {
			return nil, err
		}
		n, err := f.find(id, "item")
		if err != nil {
			return nil, err
		}
		if !n.live() {
			revision, err = currentRevision(ctx, tx)
			return nil, err
		}
		set := liveSubtree(n)
		for _, m := range set {
			if m.item.Ongoing {
				return nil, errOngoingFixed
			}
		}
		if len(set) > 1 && !cascade {
			return nil, errHasChildren
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		ids = nodeIDs(set)
		return stampDeleted(ctx, tx, set, sql.NullString{String: formatTime(now), Valid: true}, revision, now, a, OpDeleted)
	})
	if err != nil {
		return nil, 0, err
	}
	return ids, revision, nil
}

// Restore undeletes the item and returns the IDs it restored, in tree
// order, with the revision. With cascade it also restores the descendants
// deleted at the same instant as the item. The parent must be live and must
// still be able to hold the item, since it may have been retyped since.
// A due hook set with SetDueHook runs for the restored items inside the
// same transaction.
func (s *Store) Restore(ctx context.Context, a Actor, id string, cascade bool) ([]string, int64, error) {
	now := s.now().UTC()
	var (
		ids      []string
		revision int64
	)
	err := s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		f, err := loadForest(ctx, tx)
		if err != nil {
			return nil, err
		}
		n, err := f.find(id, "item")
		if err != nil {
			return nil, err
		}
		if n.live() {
			return nil, errNotDeleted
		}
		if p := n.parent; p != nil {
			if !p.live() {
				return nil, errParentDeleted
			}
			if !CanParent(p.item.Type, n.item.Type) {
				return nil, errParentRule
			}
		}
		set := []*node{n}
		if cascade {
			set = deletedWith(n)
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		ids = nodeIDs(set)
		events, err := stampDeleted(ctx, tx, set, sql.NullString{}, revision, now, a, OpRestored)
		if err != nil {
			return nil, err
		}
		return s.dueEvents(ctx, tx, ids, now, events)
	})
	if err != nil {
		return nil, 0, err
	}
	return ids, revision, nil
}

// stampDeleted sets deleted_at to deletedAt on every node in tx with one
// revision, and returns one event per node with op. deletedAt is a stored
// time to delete, or the zero sql.NullString, which stores NULL, to restore.
func stampDeleted(ctx context.Context, tx *sql.Tx, set []*node, deletedAt sql.NullString, revision int64, at time.Time, a Actor, op string) ([]watch.Event, error) {
	stmt, err := tx.PrepareContext(ctx,
		"UPDATE items SET deleted_at = ?, revision = ?, updated_at = ?, updated_by = ? WHERE id = ?")
	if err != nil {
		return nil, fmt.Errorf("items: preparing a delete: %w", err)
	}
	defer func() { _ = stmt.Close() }() // closing a prepared statement only frees it
	events := make([]watch.Event, 0, len(set))
	for _, n := range set {
		if _, err := stmt.ExecContext(ctx, deletedAt, revision, formatTime(at), a.String(), n.item.ID); err != nil {
			return nil, fmt.Errorf("items: marking an item %s: %w", op, err)
		}
		events = append(events, itemEvent(at, n.item.ID, n.item.Type, revision, op, nil))
	}
	return events, nil
}
