package cache

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Progress is a folder's cache progress, the folders row. Freshness lives
// in state.db's sync_folders; this records only what the cache has done.
type Progress struct {
	Account, Folder string
	UIDValidity     uint32
	BaselineUIDNext uint32
	BackfillLowUID  uint32 // 0 before the first batch
	BackfillDone    bool
	WindowDone      bool
	LastAppliedAt   time.Time // zero until the builder applies an event
}

const progressColumns = `account, folder, uidvalidity, baseline_uidnext, backfill_low_uid,
	backfill_done, window_done, last_applied_at`

// progressScan holds one folders row as stored.
type progressScan struct {
	account, folder            string
	uidValidity, baseline, low int64
	backfillDone, windowDone   int64
	lastApplied                sql.NullInt64
}

func (p *progressScan) dest() []any {
	return []any{&p.account, &p.folder, &p.uidValidity, &p.baseline, &p.low, &p.backfillDone, &p.windowDone, &p.lastApplied}
}

func (p *progressScan) progress() Progress {
	return Progress{
		Account:         p.account,
		Folder:          p.folder,
		UIDValidity:     uint32(p.uidValidity),
		BaselineUIDNext: uint32(p.baseline),
		BackfillLowUID:  uint32(p.low),
		BackfillDone:    p.backfillDone == 1,
		WindowDone:      p.windowDone == 1,
		LastAppliedAt:   fromNullUnix(p.lastApplied),
	}
}

// Progress returns the folder's progress. The bool is false when it has none.
func (s *Store) Progress(ctx context.Context, account, folder string) (Progress, bool, error) {
	var p progressScan
	err := s.db.QueryRowContext(ctx,
		"SELECT "+progressColumns+" FROM folders WHERE account = ? AND folder = ?", account, folder).Scan(p.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, false, nil
	}
	if err != nil {
		return Progress{}, false, fail("reading progress", err)
	}
	return p.progress(), true, nil
}

// AllProgress returns the progress of every folder of the account, by folder name.
func (s *Store) AllProgress(ctx context.Context, account string) ([]Progress, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+progressColumns+" FROM folders WHERE account = ? ORDER BY folder", account)
	if err != nil {
		return nil, fail("listing progress", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []Progress
	for rows.Next() {
		var p progressScan
		if err := rows.Scan(p.dest()...); err != nil {
			return nil, fail("listing progress", err)
		}
		out = append(out, p.progress())
	}
	if err := rows.Err(); err != nil {
		return nil, fail("listing progress", err)
	}
	return out, nil
}

// PutProgress writes every column of p, inserting or replacing the folder's row.
func (s *Store) PutProgress(ctx context.Context, p Progress) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (`+progressColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (account, folder) DO UPDATE SET
		   uidvalidity = excluded.uidvalidity, baseline_uidnext = excluded.baseline_uidnext,
		   backfill_low_uid = excluded.backfill_low_uid, backfill_done = excluded.backfill_done,
		   window_done = excluded.window_done, last_applied_at = excluded.last_applied_at`,
		p.Account, p.Folder, int64(p.UIDValidity), int64(p.BaselineUIDNext), int64(p.BackfillLowUID),
		boolInt(p.BackfillDone), boolInt(p.WindowDone), nullUnix(p.LastAppliedAt))
	if err != nil {
		return fail("writing progress", err)
	}
	return nil
}
