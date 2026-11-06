package items

import (
	"context"
	"errors"
	"fmt"
)

// node is one item in a forest.
type node struct {
	item     Item // every column but Description, with effective values set
	depth    int  // ancestors above it: 0 for a root
	parent   *node
	children []*node // live and deleted, in sibling order
}

// forest is every item in state.db, linked into trees. One person's work is
// a few thousand items at most, so reads and writes that need the tree
// around an item load it whole in one query and walk it in memory.
type forest struct {
	byID  map[string]*node
	roots []*node // in sibling order
}

// siblingOrder orders the children of one parent, and the roots: ongoing
// items first, then by creation.
const siblingOrder = "ongoing DESC, created_at, id"

// loadForest reads every item through q and links the trees.
func loadForest(ctx context.Context, q queryer) (*forest, error) {
	list, err := readItems(ctx, q)
	if err != nil {
		return nil, err
	}
	f := &forest{byID: make(map[string]*node, len(list))}
	nodes := make([]*node, len(list))
	for i, it := range list {
		nodes[i] = &node{item: it}
		f.byID[it.ID] = nodes[i]
	}
	// Rows arrive in sibling order, so appending in scan order keeps every
	// children list, and the roots, in sibling order.
	for _, n := range nodes {
		if n.item.ParentID == "" {
			f.roots = append(f.roots, n)
			continue
		}
		p, ok := f.byID[n.item.ParentID]
		if !ok {
			return nil, errors.New("items: an item's parent is missing")
		}
		n.parent = p
		p.children = append(p.children, n)
	}
	for _, n := range nodes {
		if err := f.settle(n); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// readItems reads every item in sibling order. It closes its rows before
// returning.
func readItems(ctx context.Context, q queryer) ([]Item, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+itemColumns+" FROM items ORDER BY "+siblingOrder)
	if err != nil {
		return nil, fmt.Errorf("items: reading items: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("items: reading items: %w", err)
	}
	return out, nil
}

// settle sets n's depth and effective area and priority by walking up to
// its root. The rules never let a cycle form; a walk longer than the forest
// means the stored tree is corrupt.
func (f *forest) settle(n *node) error {
	depth := 0
	area, priority := n.item.Area, n.item.Priority
	for p := n.parent; p != nil; p = p.parent {
		depth++
		if depth > len(f.byID) {
			return errors.New("items: the stored item tree has a cycle")
		}
		if area == "" {
			area = p.item.Area
		}
		if priority == "" {
			priority = p.item.Priority
		}
	}
	if priority == "" {
		priority = DefaultPriority
	}
	n.depth, n.item.EffectiveArea, n.item.EffectivePriority = depth, area, priority
	return nil
}

// find returns the node of id, or ErrNotFound naming what.
func (f *forest) find(id, what string) (*node, error) {
	if n, ok := f.byID[id]; ok {
		return n, nil
	}
	return nil, errMissing(what)
}

// getItem reads one item, deleted or not, with its description and its
// effective values.
func getItem(ctx context.Context, q queryer, id string) (Item, error) {
	f, err := loadForest(ctx, q)
	if err != nil {
		return Item{}, err
	}
	n, err := f.find(id, "item")
	if err != nil {
		return Item{}, err
	}
	return withDescription(ctx, q, n.item)
}

// withDescription returns it with its stored description, which forests
// leave out.
func withDescription(ctx context.Context, q queryer, it Item) (Item, error) {
	if err := q.QueryRowContext(ctx, "SELECT description FROM items WHERE id = ?", it.ID).Scan(&it.Description); err != nil {
		return Item{}, fmt.Errorf("items: reading a description: %w", err)
	}
	return it, nil
}

// live reports whether n's item is not deleted.
func (n *node) live() bool {
	return n.item.DeletedAt.IsZero()
}

// under reports whether n sits somewhere below a.
func (n *node) under(a *node) bool {
	for p := n.parent; p != nil; p = p.parent {
		if p == a {
			return true
		}
	}
	return false
}

// liveChildren returns n's live children in sibling order.
func (n *node) liveChildren() []*node {
	var out []*node
	for _, c := range n.children {
		if c.live() {
			out = append(out, c)
		}
	}
	return out
}

// liveSubtree returns n and its live descendants in tree order. Nothing
// live sits under a deleted item, so a deleted child is skipped whole.
func liveSubtree(n *node) []*node {
	out := []*node{n}
	for _, c := range n.children {
		if c.live() {
			out = append(out, liveSubtree(c)...)
		}
	}
	return out
}

// deletedWith returns n and the descendants deleted at the same instant as
// n, in tree order: the subtree that one cascade delete took.
func deletedWith(n *node) []*node {
	out := []*node{n}
	for _, c := range n.children {
		if !c.live() && c.item.DeletedAt.Equal(n.item.DeletedAt) {
			out = append(out, deletedWith(c)...)
		}
	}
	return out
}

// nodeIDs lists the item IDs of nodes, in order.
func nodeIDs(nodes []*node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.item.ID
	}
	return out
}
