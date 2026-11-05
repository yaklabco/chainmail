package cache

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// FolderIncomplete counts one folder's messages the cache could not read
// whole.
type FolderIncomplete struct {
	Folder string
	// Unreadable counts the messages recorded as unreadable: the server's
	// answer for them could not be parsed, so the cache has no row.
	Unreadable uint64
	// AttachmentsUnknown counts the rows read without their body
	// structure, whose attachments are not known.
	AttachmentsUnknown uint64
}

// MarkUnreadable records uids of src as unreadable, or counts one more
// attempt, stamped now, for a UID already recorded. It records nothing for
// a folder without progress at src's UIDVALIDITY, so no record outlives
// its folder, nor for a UID whose row the cache holds.
func (s *Store) MarkUnreadable(ctx context.Context, account string, src mailstore.Source, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	now := s.now().Unix()
	return s.inTx(ctx, "recording unreadable messages", func(tx *sql.Tx) error {
		mark, err := tx.PrepareContext(ctx, `INSERT INTO unreadable (account, folder, uidvalidity, uid, first_at, last_at, attempts)
			SELECT ?1, ?2, ?3, ?4, ?5, ?5, 1
			WHERE EXISTS (SELECT 1 FROM folders WHERE account = ?1 AND folder = ?2 AND uidvalidity = ?3)
			AND NOT EXISTS (SELECT 1 FROM messages WHERE account = ?1 AND folder = ?2 AND uidvalidity = ?3 AND uid = ?4)
			ON CONFLICT (account, folder, uidvalidity, uid) DO UPDATE SET last_at = excluded.last_at, attempts = attempts + 1`)
		if err != nil {
			return err
		}
		defer func() { _ = mark.Close() }() // closing a prepared statement only frees it
		for _, uid := range uids {
			if _, err := mark.ExecContext(ctx, account, src.Folder, int64(src.UIDValidity), int64(uid), now); err != nil {
				return fmt.Errorf("recording %s UID %d: %w", src.Folder, uid, err)
			}
		}
		return nil
	})
}

// ForgetUnreadable deletes the records of uids of src, for messages the
// folder no longer holds. A UID with no record is not an error.
func (s *Store) ForgetUnreadable(ctx context.Context, account string, src mailstore.Source, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	return s.inTx(ctx, "forgetting unreadable messages", func(tx *sql.Tx) error {
		for chunk := range slices.Chunk(uids, rowsChunk) {
			args := make([]any, 0, 3+len(chunk))
			args = append(args, account, src.Folder, int64(src.UIDValidity))
			for _, u := range chunk {
				args = append(args, int64(u))
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM unreadable WHERE account = ? AND folder = ? AND uidvalidity = ? AND uid IN ("+
				placeholders(len(chunk))+")", args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnreadableUIDs returns the account's recorded unreadable UIDs by folder
// and UIDVALIDITY, ascending.
func (s *Store) UnreadableUIDs(ctx context.Context, account string) (map[mailstore.Source][]uint32, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT folder, uidvalidity, uid FROM unreadable WHERE account = ? ORDER BY folder, uidvalidity, uid", account)
	if err != nil {
		return nil, s.fail("reading unreadable messages", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	out := map[mailstore.Source][]uint32{}
	for rows.Next() {
		var (
			folder           string
			uidValidity, uid int64
		)
		if err := rows.Scan(&folder, &uidValidity, &uid); err != nil {
			return nil, s.fail("reading unreadable messages", err)
		}
		src := mailstore.Source{Folder: folder, UIDValidity: uint32(uidValidity)}
		out[src] = append(out[src], uint32(uid))
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail("reading unreadable messages", err)
	}
	return out, nil
}

// Incomplete returns, for each of the account's folders with any, how many
// messages are recorded as unreadable and how many rows were read without
// their body structure, ordered by folder.
func (s *Store) Incomplete(ctx context.Context, account string) ([]FolderIncomplete, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT folder, sum(unreadable), sum(unknown) FROM (
		   SELECT folder, 1 AS unreadable, 0 AS unknown FROM unreadable WHERE account = ?1
		   UNION ALL
		   SELECT folder, 0, 1 FROM messages WHERE account = ?1 AND attachments_unknown = 1
		 ) GROUP BY folder ORDER BY folder`, account)
	if err != nil {
		return nil, s.fail("counting incomplete messages", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []FolderIncomplete
	for rows.Next() {
		var (
			f                   FolderIncomplete
			unreadable, unknown int64
		)
		if err := rows.Scan(&f.Folder, &unreadable, &unknown); err != nil {
			return nil, s.fail("counting incomplete messages", err)
		}
		f.Unreadable, f.AttachmentsUnknown = uint64(unreadable), uint64(unknown)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail("counting incomplete messages", err)
	}
	return out, nil
}
