package items

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// Refusals of updates. Their text never holds request values.
var (
	errNoFields     = &InvalidError{Msg: "the update names no fields", Hint: fieldsHint}
	errClearType    = &InvalidError{Msg: "type cannot be cleared", Hint: "name a type to change it"}
	errClearState   = &InvalidError{Msg: "state cannot be cleared", Hint: "states are backlog, in-progress, in-review and done"}
	errMoveCycle    = &InvalidError{Msg: "the move would put the item under itself", Hint: "choose a parent outside the item's subtree"}
	errChildRule    = &InvalidError{Msg: "the new type cannot hold the item's children", Hint: parentRuleHint}
	errOngoingDone  = &InvalidError{Msg: "an ongoing item is never done", Hint: "file the chore as a task under it and finish the task"}
	errOngoingFixed = &InvalidError{Msg: "an ongoing item cannot be moved, retyped or deleted", Hint: "Admin items stay at the root as initiatives"}
	errItemDeleted  = &InvalidError{Msg: "the item is deleted", Hint: "restore it first: chainmail item restore"}
)

// Patch names the fields to change (UpdateItem mask paths) and carries their values.
type Patch struct {
	Fields []string // type, title, description, state, area, priority, start_date, due_date, estimate, parent_id
	Values NewItem  // Criteria and IdempotencyKey ignored
}

// Update changes the fields p names and returns the item as stored after.
// A named field with an empty value clears it; type, title and state cannot
// be cleared. Fields whose value equals the stored one are not changes: an
// update that changes nothing writes nothing and returns the stored item.
// Otherwise the item gets one revision and one event listing the changed
// fields in mask order, op moved when parent_id changed and updated else.
func (s *Store) Update(ctx context.Context, a Actor, id string, p Patch) (Item, error) {
	fields, err := maskFields(p.Fields)
	if err != nil {
		return Item{}, err
	}
	for _, field := range fields {
		switch {
		case field == fieldType && p.Values.Type == "":
			return Item{}, errClearType
		case field == fieldState && p.Values.State == "":
			return Item{}, errClearState
		}
	}
	want, err := cleanFields(p.Values, fields)
	if err != nil {
		return Item{}, err
	}
	now := s.now().UTC()
	var out Item
	err = s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		f, err := loadForest(ctx, tx)
		if err != nil {
			return nil, err
		}
		n, err := f.find(id, "item")
		if err != nil {
			return nil, err
		}
		cur, err := withDescription(ctx, tx, n.item)
		if err != nil {
			return nil, err
		}
		next := applyPatch(cur, fields, want)
		changed := changedFields(cur, next, fields)
		if len(changed) == 0 {
			out = cur
			return nil, nil
		}
		if !n.live() {
			return nil, errItemDeleted
		}
		if err := checkUpdate(ctx, tx, f, n, next, changed); err != nil {
			return nil, err
		}
		r, err := rev()
		if err != nil {
			return nil, err
		}
		next.Revision, next.UpdatedAt, next.UpdatedBy = r, now, a.String()
		if err := updateItem(ctx, tx, next); err != nil {
			return nil, err
		}
		if out, err = f.settledItem(next); err != nil {
			return nil, err
		}
		op := OpUpdated
		if slices.Contains(changed, fieldParent) {
			op = OpMoved
		}
		return []watch.Event{itemEvent(now, id, next.Type, r, op, changed)}, nil
	})
	if err != nil {
		return Item{}, err
	}
	return out, nil
}

// maskFields checks the mask: at least one known field. A field named twice
// counts once, at its first place.
func maskFields(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errNoFields
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !slices.Contains(itemFields, p) {
			return nil, errUnknownField
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// applyPatch returns it with the named fields set from v.
func applyPatch(it Item, fields []string, v NewItem) Item {
	for _, field := range fields {
		switch field {
		case fieldType:
			it.Type = v.Type
		case fieldTitle:
			it.Title = v.Title
		case fieldDescription:
			it.Description = v.Description
		case fieldState:
			it.State = v.State
		case fieldArea:
			it.Area = v.Area
		case fieldPriority:
			it.Priority = v.Priority
		case fieldStartDate:
			it.StartDate = v.StartDate
		case fieldDueDate:
			it.DueDate = v.DueDate
		case fieldEstimate:
			it.Estimate = v.Estimate
		case fieldParent:
			it.ParentID = v.ParentID
		}
	}
	return it
}

// changedFields lists, in mask order, the named fields whose value differs
// between cur and next.
func changedFields(cur, next Item, fields []string) []string {
	var out []string
	for _, field := range fields {
		var same bool
		switch field {
		case fieldType:
			same = cur.Type == next.Type
		case fieldTitle:
			same = cur.Title == next.Title
		case fieldDescription:
			same = cur.Description == next.Description
		case fieldState:
			same = cur.State == next.State
		case fieldArea:
			same = cur.Area == next.Area
		case fieldPriority:
			same = cur.Priority == next.Priority
		case fieldStartDate:
			same = cur.StartDate == next.StartDate
		case fieldDueDate:
			same = cur.DueDate == next.DueDate
		case fieldEstimate:
			same = sameEstimate(cur.Estimate, next.Estimate)
		case fieldParent:
			same = cur.ParentID == next.ParentID
		}
		if !same {
			out = append(out, field)
		}
	}
	return out
}

// sameEstimate reports whether two estimates are both unset or equal.
func sameEstimate(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// checkUpdate applies the item rules to n becoming next. The cycle check
// runs before the parent rule: the rank order alone would refuse a move
// into the item's own subtree, but the cycle is the clearer reason.
func checkUpdate(ctx context.Context, tx *sql.Tx, f *forest, n *node, next Item, changed []string) error {
	retyped, moved := slices.Contains(changed, fieldType), slices.Contains(changed, fieldParent)
	if n.item.Ongoing && (retyped || moved) {
		return errOngoingFixed
	}
	if moved && next.ParentID != "" {
		p, err := f.find(next.ParentID, "parent")
		if err != nil {
			return err
		}
		if p == n || p.under(n) {
			return errMoveCycle
		}
	}
	if retyped || moved || slices.Contains(changed, fieldArea) {
		if err := checkPlacement(f, next.Type, next.ParentID, next.Area); err != nil {
			return err
		}
	}
	if retyped {
		for _, c := range n.liveChildren() {
			if !CanParent(next.Type, c.item.Type) {
				return errChildRule
			}
		}
	}
	if slices.Contains(changed, fieldState) && next.State == StateDone {
		if n.item.Ongoing {
			return errOngoingDone
		}
		unticked, err := untickedCount(ctx, tx, n.item.ID)
		if err != nil {
			return err
		}
		if unticked > 0 {
			return errDoneUnticked
		}
	}
	return nil
}

// untickedCount counts the item's unticked criteria.
func untickedCount(ctx context.Context, q queryer, id string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM item_criteria WHERE item_id = ? AND ticked = 0", id).Scan(&n); err != nil {
		return 0, fmt.Errorf("items: counting unticked criteria: %w", err)
	}
	return n, nil
}

// updateItem writes every changeable column of it in tx.
func updateItem(ctx context.Context, tx *sql.Tx, it Item) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE items SET type = ?, title = ?, description = ?, state = ?, area = ?, priority = ?,
		   start_date = ?, due_date = ?, estimate = ?, parent_id = ?, revision = ?, updated_at = ?, updated_by = ?
		 WHERE id = ?`,
		string(it.Type), it.Title, it.Description, string(it.State),
		nullText(string(it.Area)), nullText(string(it.Priority)), nullText(it.StartDate), nullText(it.DueDate),
		nullFloat(it.Estimate), nullText(it.ParentID), it.Revision, formatTime(it.UpdatedAt), it.UpdatedBy, it.ID)
	if err != nil {
		return fmt.Errorf("items: updating an item: %w", err)
	}
	return nil
}
