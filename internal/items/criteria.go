package items

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// criterionColumns are the item_criteria columns scanCriterion reads, in its order.
const criterionColumns = "id, position, text, ticked, evidence, ticked_at, ticked_by"

// criteriaOf reads the criteria of itemID in position order. It closes its
// rows before returning.
func criteriaOf(ctx context.Context, q queryer, itemID string) ([]Criterion, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT "+criterionColumns+" FROM item_criteria WHERE item_id = ? ORDER BY position, id", itemID)
	if err != nil {
		return nil, fmt.Errorf("items: reading criteria: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []Criterion
	for rows.Next() {
		c, err := scanCriterion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("items: reading criteria: %w", err)
	}
	return out, nil
}

// scanCriterion reads one row of criterionColumns.
func scanCriterion(sc scanner) (Criterion, error) {
	var (
		c      Criterion
		ticked int
		at, by sql.NullString
	)
	if err := sc.Scan(&c.ID, &c.Position, &c.Text, &ticked, &c.Evidence, &at, &by); err != nil {
		return Criterion{}, fmt.Errorf("items: reading a criterion: %w", err)
	}
	c.Ticked, c.TickedBy = ticked != 0, by.String
	if at.Valid {
		t, err := parseTime(at.String)
		if err != nil {
			return Criterion{}, fmt.Errorf("items: reading a criterion: %w", err)
		}
		c.TickedAt = t
	}
	return c, nil
}

// newCriteria gives each text a new criterion ID, positioned from `from` on.
func newCriteria(texts []string, from int, now time.Time, rnd io.Reader) []Criterion {
	out := make([]Criterion, len(texts))
	for i, text := range texts {
		out[i] = Criterion{ID: NewID("cr_", now, rnd), Position: from + i, Text: text}
	}
	return out
}

// errCriterionTaken is putCriteria's error for a criterion ID another item
// already holds. New IDs are random, so it means a collision or a bug; it is
// fixed text and never names the ID.
var errCriterionTaken = errors.New("items: a criterion ID belongs to another item")

// putCriteria writes list as the item's criteria in tx: new IDs are
// inserted, unticked, and known ones get the position and text in list. An
// ID that another item holds is errCriterionTaken, and that item's
// criterion is left as it was.
func putCriteria(ctx context.Context, tx *sql.Tx, itemID string, list []Criterion) error {
	if len(list) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO item_criteria (id, item_id, position, text) VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET position = excluded.position, text = excluded.text
		 WHERE item_criteria.item_id = excluded.item_id`)
	if err != nil {
		return fmt.Errorf("items: preparing a criteria write: %w", err)
	}
	defer func() { _ = stmt.Close() }() // closing a prepared statement only frees it
	for _, c := range list {
		res, err := stmt.ExecContext(ctx, c.ID, itemID, c.Position, c.Text)
		if err != nil {
			return fmt.Errorf("items: writing a criterion: %w", err)
		}
		// The guarded upsert changes no row only when another item holds the ID.
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("items: writing a criterion: %w", err)
		}
		if n == 0 {
			return errCriterionTaken
		}
	}
	return nil
}

// Refusals of criteria edits and ticks. Their text never holds request values.
var (
	errCriteriaOrder  = &InvalidError{Msg: "order must name every remaining criterion exactly once", Hint: "list the criterion IDs from chainmail item show, leaving out the ones removed"}
	errUntickEvidence = &InvalidError{Msg: "evidence is given only when ticking", Hint: "leave evidence empty to untick"}
)

// CriterionText is a new text for one criterion.
type CriterionText struct{ ID, Text string }

// CriteriaEdit changes an item's criteria in one call, applied in field
// order: Remove first, then Edit, then Order (when set, every remaining ID
// exactly once, in the new order), then Add, appended in its order.
type CriteriaEdit struct {
	Remove []string
	Edit   []CriterionText
	Order  []string
	Add    []string
}

// EditCriteria applies e to the criteria of a live item and returns them,
// with the revision. Edited criteria keep their ticks. An edit that leaves
// the criteria as they were writes nothing and returns the current
// revision; otherwise the item gets one revision and one criteria event.
func (s *Store) EditCriteria(ctx context.Context, a Actor, itemID string, e CriteriaEdit) ([]Criterion, int64, error) {
	clean := CriteriaEdit{Remove: e.Remove, Order: e.Order}
	for _, ed := range e.Edit {
		text, err := CleanCriterion(ed.Text)
		if err != nil {
			return nil, 0, err
		}
		clean.Edit = append(clean.Edit, CriterionText{ID: ed.ID, Text: text})
	}
	for _, add := range e.Add {
		text, err := CleanCriterion(add)
		if err != nil {
			return nil, 0, err
		}
		clean.Add = append(clean.Add, text)
	}
	now := s.now().UTC()
	var (
		out      []Criterion
		revision int64
	)
	err := s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		h, err := itemHead(ctx, tx, itemID, "item")
		if err != nil {
			return nil, err
		}
		if h.deleted {
			return nil, errItemDeleted
		}
		cur, err := criteriaOf(ctx, tx, itemID)
		if err != nil {
			return nil, err
		}
		next, err := planCriteria(cur, clean, func() string { return NewID("cr_", now, s.rnd) })
		if err != nil {
			return nil, err
		}
		if sameCriteria(cur, next) {
			out = cur
			revision, err = currentRevision(ctx, tx)
			return nil, err
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		if err := dropCriteria(ctx, tx, cur, next); err != nil {
			return nil, err
		}
		if err := putCriteria(ctx, tx, itemID, next); err != nil {
			return nil, err
		}
		if err := touch(ctx, tx, itemID, revision, now, a); err != nil {
			return nil, err
		}
		if out, err = criteriaOf(ctx, tx, itemID); err != nil {
			return nil, err
		}
		return []watch.Event{itemEvent(now, itemID, h.typ, revision, OpCriteria, nil)}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, revision, nil
}

// planCriteria applies e, whose texts are clean, to cur and returns the
// resulting list with positions renumbered from 0. New criteria get IDs
// from newID.
func planCriteria(cur []Criterion, e CriteriaEdit, newID func() string) ([]Criterion, error) {
	removed := make(map[string]bool, len(e.Remove))
	for _, id := range e.Remove {
		if !slices.ContainsFunc(cur, func(c Criterion) bool { return c.ID == id }) {
			return nil, errMissing("criterion")
		}
		removed[id] = true
	}
	var kept []Criterion
	for _, c := range cur {
		if !removed[c.ID] {
			kept = append(kept, c)
		}
	}
	for _, ed := range e.Edit {
		i := slices.IndexFunc(kept, func(c Criterion) bool { return c.ID == ed.ID })
		if i < 0 {
			return nil, errMissing("criterion")
		}
		kept[i].Text = ed.Text
	}
	if len(e.Order) > 0 {
		if len(e.Order) != len(kept) {
			return nil, errCriteriaOrder
		}
		reordered := make([]Criterion, 0, len(kept))
		for _, id := range e.Order {
			i := slices.IndexFunc(kept, func(c Criterion) bool { return c.ID == id })
			if i < 0 || slices.ContainsFunc(reordered, func(c Criterion) bool { return c.ID == id }) {
				return nil, errCriteriaOrder
			}
			reordered = append(reordered, kept[i])
		}
		kept = reordered
	}
	for _, text := range e.Add {
		kept = append(kept, Criterion{ID: newID(), Text: text})
	}
	for i := range kept {
		kept[i].Position = i
	}
	return kept, nil
}

// sameCriteria reports whether two lists hold the same IDs, positions and texts.
func sameCriteria(a, b []Criterion) bool {
	return slices.EqualFunc(a, b, func(x, y Criterion) bool {
		return x.ID == y.ID && x.Position == y.Position && x.Text == y.Text
	})
}

// dropCriteria deletes in tx the criteria of cur that next no longer holds.
func dropCriteria(ctx context.Context, tx *sql.Tx, cur, next []Criterion) error {
	for _, c := range cur {
		if slices.ContainsFunc(next, func(n Criterion) bool { return n.ID == c.ID }) {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM item_criteria WHERE id = ?", c.ID); err != nil {
			return fmt.Errorf("items: removing a criterion: %w", err)
		}
	}
	return nil
}

// TickCriterion ticks a criterion of a live item with evidence, which is
// required, or unticks it, which clears evidence, time and actor and takes
// no evidence. It returns the criterion and the revision. A tick that
// matches the stored state writes nothing and returns the current revision.
func (s *Store) TickCriterion(ctx context.Context, a Actor, itemID, criterionID string, ticked bool, evidence string) (Criterion, int64, error) {
	switch {
	case ticked:
		clean, err := CleanEvidence(evidence)
		if err != nil {
			return Criterion{}, 0, err
		}
		evidence = clean
	case evidence != "":
		return Criterion{}, 0, errUntickEvidence
	}
	now := s.now().UTC()
	var (
		out      Criterion
		revision int64
	)
	err := s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		h, err := itemHead(ctx, tx, itemID, "item")
		if err != nil {
			return nil, err
		}
		if h.deleted {
			return nil, errItemDeleted
		}
		c, err := criterionOf(ctx, tx, itemID, criterionID)
		if err != nil {
			return nil, err
		}
		if c.Ticked == ticked && c.Evidence == evidence {
			out = c
			revision, err = currentRevision(ctx, tx)
			return nil, err
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		if ticked {
			_, err = tx.ExecContext(ctx,
				"UPDATE item_criteria SET ticked = 1, evidence = ?, ticked_at = ?, ticked_by = ? WHERE id = ?",
				evidence, formatTime(now), a.String(), criterionID)
		} else {
			_, err = tx.ExecContext(ctx,
				"UPDATE item_criteria SET ticked = 0, evidence = '', ticked_at = NULL, ticked_by = NULL WHERE id = ?",
				criterionID)
		}
		if err != nil {
			return nil, fmt.Errorf("items: ticking a criterion: %w", err)
		}
		if err := touch(ctx, tx, itemID, revision, now, a); err != nil {
			return nil, err
		}
		if out, err = criterionOf(ctx, tx, itemID, criterionID); err != nil {
			return nil, err
		}
		return []watch.Event{itemEvent(now, itemID, h.typ, revision, OpCriteria, nil)}, nil
	})
	if err != nil {
		return Criterion{}, 0, err
	}
	return out, revision, nil
}

// criterionOf reads criterion id of itemID, or ErrNotFound naming criterion.
func criterionOf(ctx context.Context, q queryer, itemID, id string) (Criterion, error) {
	c, err := scanCriterion(q.QueryRowContext(ctx,
		"SELECT "+criterionColumns+" FROM item_criteria WHERE id = ? AND item_id = ?", id, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return Criterion{}, errMissing("criterion")
	}
	return c, err
}
