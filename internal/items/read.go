package items

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// Get returns the item with id, deleted or not.
func (s *Store) Get(ctx context.Context, id string) (Item, error) {
	var it Item
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		it, err = getItem(ctx, tx, id)
		return err
	})
	if err != nil {
		return Item{}, err
	}
	return it, nil
}

// Criteria returns the item's criteria in position order, deleted item or not.
func (s *Store) Criteria(ctx context.Context, id string) ([]Criterion, error) {
	var out []Criterion
	err := s.read(ctx, func(tx *sql.Tx) error {
		if _, err := itemHead(ctx, tx, id, "item"); err != nil {
			return err
		}
		var err error
		out, err = criteriaOf(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// treeRead loads the forest and the row counts in one read and finds id.
func (s *Store) treeRead(ctx context.Context, id string, fn func(tx *sql.Tx, f *forest, n *node, c counts) error) error {
	return s.read(ctx, func(tx *sql.Tx) error {
		f, err := loadForest(ctx, tx)
		if err != nil {
			return err
		}
		n, err := f.find(id, "item")
		if err != nil {
			return err
		}
		c, err := loadCounts(ctx, tx)
		if err != nil {
			return err
		}
		return fn(tx, f, n, c)
	})
}

// Ancestors returns the items above id, root first, not including id. The
// root's row has Depth 0, the next 1, and so on.
func (s *Store) Ancestors(ctx context.Context, id string) ([]Row, error) {
	var out []Row
	err := s.treeRead(ctx, id, func(_ *sql.Tx, _ *forest, n *node, c counts) error {
		var chain []*node
		for p := n.parent; p != nil; p = p.parent {
			chain = append(chain, p)
		}
		slices.Reverse(chain)
		for i, p := range chain {
			out = append(out, row(p, i, c))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Children returns id's live children in sibling order, at most limit of
// them (all when limit <= 0), and how many live children it has. Rows have
// Depth 0.
func (s *Store) Children(ctx context.Context, id string, limit int) ([]Row, int, error) {
	var (
		out   []Row
		total int
	)
	err := s.treeRead(ctx, id, func(_ *sql.Tx, _ *forest, n *node, c counts) error {
		live := n.liveChildren()
		total = len(live)
		if limit > 0 && len(live) > limit {
			live = live[:limit]
		}
		for _, k := range live {
			out = append(out, row(k, 0, c))
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// Subtree returns id and the IDs of its live descendants, in tree order.
// An unknown or deleted id is ErrNotFound.
func (s *Store) Subtree(ctx context.Context, id string) ([]string, error) {
	var ids []string
	err := s.treeRead(ctx, id, func(_ *sql.Tx, _ *forest, n *node, _ counts) error {
		if !n.live() {
			return errMissing("item")
		}
		ids = nodeIDs(liveSubtree(n))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// Blockers returns the live items that block id, then the live items id
// blocks, each in creation order. Rows have Depth 0.
func (s *Store) Blockers(ctx context.Context, id string) ([]Row, []Row, error) {
	var blockedBy, blocks []Row
	err := s.treeRead(ctx, id, func(tx *sql.Tx, f *forest, _ *node, c counts) error {
		by, err := relatedIDs(ctx, tx,
			`SELECT b.blocked_by_id FROM item_blockers b JOIN items i ON i.id = b.blocked_by_id
			 WHERE b.item_id = ? ORDER BY i.created_at, i.id`, id)
		if err != nil {
			return err
		}
		bl, err := relatedIDs(ctx, tx,
			`SELECT b.item_id FROM item_blockers b JOIN items i ON i.id = b.item_id
			 WHERE b.blocked_by_id = ? ORDER BY i.created_at, i.id`, id)
		if err != nil {
			return err
		}
		blockedBy, blocks = liveRows(f, by, c), liveRows(f, bl, c)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return blockedBy, blocks, nil
}

// relatedIDs runs a query for item IDs with one argument. It closes its
// rows before returning.
func relatedIDs(ctx context.Context, q queryer, query, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("items: reading blockers: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("items: reading blockers: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("items: reading blockers: %w", err)
	}
	return out, nil
}

// liveRows returns rows, at Depth 0, for the live items among ids, in order.
func liveRows(f *forest, ids []string, c counts) []Row {
	var out []Row
	for _, id := range ids {
		if n, ok := f.byID[id]; ok && n.live() {
			out = append(out, row(n, 0, c))
		}
	}
	return out
}
