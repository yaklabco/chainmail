package cache

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
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

// HasUnreadable reports whether any of the account's folders holds a
// message recorded as unreadable.
func (s *Store) HasUnreadable(ctx context.Context, account string, folders []string) (bool, error) {
	if len(folders) == 0 {
		return false, nil
	}
	for chunk := range slices.Chunk(folders, rowsChunk) {
		args := make([]any, 0, 1+len(chunk))
		args = append(args, account)
		for _, f := range chunk {
			args = append(args, f)
		}
		var found bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM unreadable WHERE account = ? AND folder IN ("+
			placeholders(len(chunk))+"))", args...).Scan(&found); err != nil {
			return false, s.fail("reading unreadable messages", err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// UnreadableMessage is one recorded unreadable message.
type UnreadableMessage struct {
	Source mailstore.Source
	UID    uint32
}

// OldestUnreadable returns up to limit of the account's recorded
// unreadable messages, those attempted longest ago first.
func (s *Store) OldestUnreadable(ctx context.Context, account string, limit int) ([]UnreadableMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT folder, uidvalidity, uid FROM unreadable WHERE account = ?
		 ORDER BY last_at, folder, uidvalidity, uid LIMIT ?`, account, limit)
	if err != nil {
		return nil, s.fail("reading unreadable messages", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []UnreadableMessage
	for rows.Next() {
		var (
			m                UnreadableMessage
			uidValidity, uid int64
		)
		if err := rows.Scan(&m.Source.Folder, &uidValidity, &uid); err != nil {
			return nil, s.fail("reading unreadable messages", err)
		}
		m.Source.UIDValidity, m.UID = uint32(uidValidity), uint32(uid)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail("reading unreadable messages", err)
	}
	return out, nil
}

// incompleteQuery counts an account's unreadable records and degraded rows
// per folder. Both halves run from an index: the unreadable table's
// primary key, and messages_attachments_unknown, a partial index over the
// few degraded rows, so cache status never scans the account's messages.
const incompleteQuery = `SELECT folder, sum(unreadable), sum(unknown) FROM (
	   SELECT folder, 1 AS unreadable, 0 AS unknown FROM unreadable WHERE account = ?1
	   UNION ALL
	   SELECT folder, 0, 1 FROM messages WHERE account = ?1 AND attachments_unknown = 1
	 ) GROUP BY folder ORDER BY folder`

// Incomplete returns, for each of the account's folders with any, how many
// messages are recorded as unreadable and how many rows were read without
// their body structure, ordered by folder.
func (s *Store) Incomplete(ctx context.Context, account string) ([]FolderIncomplete, error) {
	rows, err := s.db.QueryContext(ctx, incompleteQuery, account)
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

// RepairAttachments gives the row of r, when it was read without its body
// structure, the attachments a full read of the message found, and marks
// them known. A message's attachments never change for a UID, so this is
// safe whenever the full read was of r. Flags and every other column keep
// their stored values, which may be newer than the read. It reports whether
// it repaired a row: false when there is none or its attachments are
// known.
func (s *Store) RepairAttachments(ctx context.Context, r ref.Ref, has bool, attachments []mailstore.Attachment) (bool, error) {
	aj, err := jsonList(attachments)
	if err != nil {
		return false, fmt.Errorf("repairing attachments: %w", err)
	}
	args := append([]any{boolInt(has), aj, normAttachments(attachments)}, refArgs(r)...)
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET has_attachment = ?, attachments_json = ?, attachments_norm = ?, attachments_unknown = 0
		 WHERE `+msgKey+` AND attachments_unknown = 1`, args...)
	if err != nil {
		return false, s.fail("repairing attachments", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, s.fail("repairing attachments", err)
	}
	return n > 0, nil
}
