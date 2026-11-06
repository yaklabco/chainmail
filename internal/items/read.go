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
