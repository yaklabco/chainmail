package items

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// LinkTarget is a mail thread to link, as the daemon read it from one of
// its messages.
type LinkTarget struct {
	Account   string
	ThreadKey string
	RootMsgID string
	MsgID     string // the given message's Message-ID; "" when it has none
}

// Link is a mail thread linked to an item. Note is item text: untrusted on
// every read, and never in an error, an event or an audit row.
type Link struct {
	ItemID, Account, ThreadKey, RootMsgID, AnchorMsgID, Note string
	CreatedAt                                                time.Time
	CreatedBy                                                string
}

// Refusals of a link request. They are fixed text and repeat nothing from
// the request: no key, Message-ID or note.
var (
	errLinkNoAccount = &InvalidError{Msg: "the message names no account", Hint: "link a message by its ref, as chainmail search shows it"}
	errLinkNoThread  = &InvalidError{
		Msg:  "the message has no thread key, so it cannot be linked",
		Hint: "link a message that has a Message-ID, or a reply that names its thread",
	}
	errAnchorNoMsgID = &InvalidError{Msg: "the message has no Message-ID, so it cannot be the anchor", Hint: "link it without anchor"}
	errLinkDeleted   = &InvalidError{Msg: "the item is deleted", Hint: "restore it first, with: chainmail item restore"}
)

// linkColumns are the item_links columns, in the order scanLink reads them.
const linkColumns = "item_id, account, thread_key, root_msgid, anchor_msgid, note, created_at, created_by"

// LinkThread links t's thread to a live item. On an existing link it sets the
// anchor when anchor is true and the note when note is non-empty, and reports
// created false. An empty Account or ThreadKey, or anchor with an empty
// MsgID, is *InvalidError. A change bumps the revision, stamps the item and
// appends an item event with op linked. A call that changes nothing writes
// nothing and returns the current revision.
func (s *Store) LinkThread(ctx context.Context, a Actor, itemID string, t LinkTarget, anchor bool, note string) (Link, bool, int64, error) {
	switch {
	case t.Account == "":
		return Link{}, false, 0, errLinkNoAccount
	case t.ThreadKey == "":
		return Link{}, false, 0, errLinkNoThread
	case anchor && t.MsgID == "":
		return Link{}, false, 0, errAnchorNoMsgID
	}
	note, err := CleanNote(note)
	if err != nil {
		return Link{}, false, 0, err
	}
	var (
		link     Link
		created  bool
		changed  bool
		revision int64
	)
	err = s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		typ, err := linkItemType(ctx, tx, itemID)
		if err != nil {
			return nil, err
		}
		now := s.now().UTC()
		old, found, err := readLink(ctx, tx, itemID, t.Account, t.ThreadKey)
		if err != nil {
			return nil, err
		}
		link, created, changed = old, !found, !found
		if !found {
			link = Link{
				ItemID: itemID, Account: t.Account, ThreadKey: t.ThreadKey, RootMsgID: t.RootMsgID,
				Note: note, CreatedAt: now, CreatedBy: a.String(),
			}
		}
		if anchor && link.AnchorMsgID != t.MsgID {
			link.AnchorMsgID, changed = t.MsgID, true
		}
		if found && note != "" && link.Note != note {
			link.Note, changed = note, true
		}
		if !changed {
			return nil, nil
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		if err := saveLink(ctx, tx, link); err != nil {
			return nil, err
		}
		if err := touch(ctx, tx, itemID, revision, now, a); err != nil {
			return nil, err
		}
		return []watch.Event{itemEvent(now, itemID, typ, revision, OpLinked, nil)}, nil
	})
	if err != nil {
		return Link{}, false, 0, err
	}
	if !changed {
		if revision, err = s.Revision(ctx); err != nil {
			return Link{}, false, 0, err
		}
	}
	return link, created, revision, nil
}

// UnlinkThread removes the link of item itemID to threadKey on account. It
// bumps the revision, stamps the item and appends an item event with op
// unlinked. An unknown item is ErrNotFound "item", a deleted one
// *InvalidError, and a link the item does not have ErrNotFound "link".
func (s *Store) UnlinkThread(ctx context.Context, a Actor, itemID, account, threadKey string) (int64, error) {
	var revision int64
	err := s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		typ, err := linkItemType(ctx, tx, itemID)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx,
			"DELETE FROM item_links WHERE item_id = ? AND account = ? AND thread_key = ?", itemID, account, threadKey)
		if err != nil {
			return nil, fmt.Errorf("items: removing a link: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("items: removing a link: %w", err)
		}
		if n == 0 {
			return nil, errMissing("link")
		}
		if revision, err = rev(); err != nil {
			return nil, err
		}
		now := s.now().UTC()
		if err := touch(ctx, tx, itemID, revision, now, a); err != nil {
			return nil, err
		}
		return []watch.Event{itemEvent(now, itemID, typ, revision, OpUnlinked, nil)}, nil
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

// Links returns the links of item itemID, live or deleted, newest created
// first, then by account and thread key. An unknown item is ErrNotFound
// "item"; an item with no links gives an empty result.
func (s *Store) Links(ctx context.Context, itemID string) ([]Link, error) {
	var out []Link
	err := s.read(ctx, func(tx *sql.Tx) error {
		if _, err := itemHead(ctx, tx, itemID, "item"); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT "+linkColumns+" FROM item_links WHERE item_id = ? ORDER BY created_at DESC, account, thread_key", itemID)
		if err != nil {
			return fmt.Errorf("items: reading links: %w", err)
		}
		defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				return fmt.Errorf("items: reading links: %w", err)
			}
			out = append(out, l)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("items: reading links: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// linkItemType returns the type of the live item id, read in tx. An unknown
// item is ErrNotFound "item" and a deleted one errLinkDeleted.
func linkItemType(ctx context.Context, tx *sql.Tx, id string) (Type, error) {
	h, err := itemHead(ctx, tx, id, "item")
	if err != nil {
		return "", err
	}
	if h.deleted {
		return "", errLinkDeleted
	}
	return h.typ, nil
}

// readLink reads the link of itemID to threadKey on account in tx, and
// reports whether there is one.
func readLink(ctx context.Context, tx *sql.Tx, itemID, account, threadKey string) (Link, bool, error) {
	row := tx.QueryRowContext(ctx,
		"SELECT "+linkColumns+" FROM item_links WHERE item_id = ? AND account = ? AND thread_key = ?",
		itemID, account, threadKey)
	l, err := scanLink(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Link{}, false, nil
	case err != nil:
		return Link{}, false, fmt.Errorf("items: reading a link: %w", err)
	}
	return l, true, nil
}

// scanLink reads one row of linkColumns.
func scanLink(sc scanner) (Link, error) {
	var l Link
	var created string
	if err := sc.Scan(&l.ItemID, &l.Account, &l.ThreadKey, &l.RootMsgID, &l.AnchorMsgID, &l.Note, &created, &l.CreatedBy); err != nil {
		return Link{}, err
	}
	at, err := parseTime(created)
	if err != nil {
		return Link{}, err
	}
	l.CreatedAt = at
	return l, nil
}

// saveLink writes l, replacing the anchor and note of the link already
// stored for the same item, account and thread, whose root and creation
// stay as they were.
func saveLink(ctx context.Context, tx *sql.Tx, l Link) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO item_links ("+linkColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)"+
		" ON CONFLICT (item_id, account, thread_key) DO UPDATE SET anchor_msgid = excluded.anchor_msgid, note = excluded.note",
		l.ItemID, l.Account, l.ThreadKey, l.RootMsgID, l.AnchorMsgID, l.Note, formatTime(l.CreatedAt), l.CreatedBy)
	if err != nil {
		return fmt.Errorf("items: saving a link: %w", err)
	}
	return nil
}

// subtreeLinksSQL lists the distinct (account, thread key) pairs linked to
// an item or any live item below it, by account then key. The walk stops at
// deleted items: a delete with live children needs cascade, which takes the
// subtree with it.
const subtreeLinksSQL = `WITH RECURSIVE subtree(id) AS (
    SELECT ?
    UNION
    SELECT items.id FROM items JOIN subtree ON items.parent_id = subtree.id
    WHERE items.deleted_at IS NULL
)
SELECT DISTINCT item_links.account, item_links.thread_key
FROM item_links JOIN subtree ON item_links.item_id = subtree.id
ORDER BY item_links.account, item_links.thread_key`

// ThreadKeys returns, per account, the distinct thread keys linked to id or
// any live item below it, each account's keys sorted. The map is never nil.
// ErrNotFound "item" for an unknown or deleted id.
func (s *Store) ThreadKeys(ctx context.Context, id string) (map[string][]string, error) {
	out := map[string][]string{}
	// One read transaction, so the item cannot be deleted between the check
	// and the walk.
	err := s.read(ctx, func(tx *sql.Tx) error {
		h, err := itemHead(ctx, tx, id, "item")
		if err != nil {
			return err
		}
		if h.deleted {
			return errMissing("item")
		}
		rows, err := tx.QueryContext(ctx, subtreeLinksSQL, id)
		if err != nil {
			return fmt.Errorf("items: reading links: %w", err)
		}
		defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
		for rows.Next() {
			var account, key string
			if err := rows.Scan(&account, &key); err != nil {
				return fmt.Errorf("items: reading links: %w", err)
			}
			out[account] = append(out[account], key)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("items: reading links: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LinkedThreadKeys returns the distinct thread keys of account linked to any
// live item, sorted. An account with none gives an empty, non-nil slice.
func (s *Store) LinkedThreadKeys(ctx context.Context, account string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT item_links.thread_key
FROM item_links JOIN items ON items.id = item_links.item_id
WHERE item_links.account = ? AND items.deleted_at IS NULL
ORDER BY item_links.thread_key`, account)
	if err != nil {
		return nil, fmt.Errorf("items: reading links: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	out := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("items: reading links: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("items: reading links: %w", err)
	}
	return out, nil
}
