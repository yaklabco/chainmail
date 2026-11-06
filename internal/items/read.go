package items

import (
	"context"
	"database/sql"
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
