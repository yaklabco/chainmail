package items

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"
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

// putCriteria writes list as the item's criteria in tx: new IDs are
// inserted, unticked, and known ones get the position and text in list.
func putCriteria(ctx context.Context, tx *sql.Tx, itemID string, list []Criterion) error {
	if len(list) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO item_criteria (id, item_id, position, text) VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET position = excluded.position, text = excluded.text`)
	if err != nil {
		return fmt.Errorf("items: preparing a criteria write: %w", err)
	}
	defer func() { _ = stmt.Close() }() // closing a prepared statement only frees it
	for _, c := range list {
		if _, err := stmt.ExecContext(ctx, c.ID, itemID, c.Position, c.Text); err != nil {
			return fmt.Errorf("items: writing a criterion: %w", err)
		}
	}
	return nil
}
