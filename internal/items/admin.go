package items

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yaklabco/chainmail/internal/watch"
)

// adminTitle is the title of both Admin items.
const adminTitle = "Admin"

// adminItems are the standing Admin initiatives, one per area, in the order
// EnsureAdmin creates them.
var adminItems = []struct {
	id   string
	area Area
}{
	{AdminWorkID, AreaWork},
	{AdminPersonalID, AreaPersonal},
}

// EnsureAdmin creates the Admin items that are missing: ongoing initiatives
// titled Admin at the root, one per area. Items it creates together share
// one revision. They are bootstrap rows, so it writes no item events (the
// daemon runs it before anything reads the log, and a changed_since reader
// still finds them by revision). Running it again changes nothing.
func (s *Store) EnsureAdmin(ctx context.Context, a Actor) error {
	now := s.now().UTC()
	return s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		for _, adm := range adminItems {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM items WHERE id = ?", adm.id).Scan(&n); err != nil {
				return nil, fmt.Errorf("items: looking for an Admin item: %w", err)
			}
			if n > 0 {
				continue
			}
			r, err := rev()
			if err != nil {
				return nil, err
			}
			it := Item{
				ID: adm.id, Type: TypeInitiative, Title: adminTitle, State: StateBacklog, Area: adm.area,
				Ongoing: true, Revision: r, CreatedAt: now, UpdatedAt: now,
				CreatedBy: a.String(), UpdatedBy: a.String(),
			}
			if err := insertItem(ctx, tx, it); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
}
