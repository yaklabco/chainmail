package items

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/watch"
)

// MailAccount is what MailArrived needs to know about the account.
type MailAccount struct {
	Name    string
	Address string // the account's own address, for from_self
}

// mailClockSkew is how far before a link's creation a message may have
// arrived and still count as news on the linked thread. IMAP INTERNALDATE
// has one-second precision, so a reply in the same second as the link reads
// as earlier than it, and the server's clock may lag the daemon's, which
// stamps the link. Only mail older than this is taken to have been on the
// server before the link.
const mailClockSkew = 5 * time.Minute

// MailArrived runs inside a sync batch transaction (the engine's OnNew hook).
// For each new message with a thread key, and each live item linked to that
// thread, it records (item, account, message key) in item_mail_seen, where the
// message key is Summary.MessageID or, when empty, the ref; and for each row
// it inserts it returns one item_mail event: Account, Folder, Ref and Message
// copied from the new event, ItemID set, FromSelf true when any From address
// equals Address case-insensitively, At = at. Events come in the order of
// news, then by item ID. A zero at is replaced by the current time, the clock
// a Store uses by default, so the seen rows always carry a time Trim can age.
//
// A message whose InternalDate is before the link's creation less
// mailClockSkew, truncated to the second, is skipped for that link: it was
// on the server already, and has only been filed or copied. A zero
// InternalDate is never skipped.
//
// It uses only tx: the state.db connection is the batch's until it commits.
// A row already recorded, as for the same message arriving in a second
// folder, gives no event. The seen rows commit or roll back with the batch,
// so a batch the next pass repeats gets its events then.
func MailArrived(ctx context.Context, tx *sql.Tx, acct MailAccount, news []watch.NewMail, at time.Time) ([]watch.Event, error) {
	if len(news) == 0 {
		return nil, nil
	}
	var one int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM item_links WHERE account = ? LIMIT 1", acct.Name).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil // nothing on the account is linked
	case err != nil:
		return nil, fmt.Errorf("items: reading links: %w", err)
	}
	insert, err := tx.PrepareContext(ctx, "INSERT INTO item_mail_seen (item_id, account, message_key, at) VALUES (?, ?, ?, ?)"+
		" ON CONFLICT (item_id, account, message_key) DO NOTHING")
	if err != nil {
		return nil, fmt.Errorf("items: preparing item mail: %w", err)
	}
	defer func() { _ = insert.Close() }() // closing a prepared statement only frees it
	if at.IsZero() {
		at = time.Now().UTC()
	}
	stamp := formatTime(at)
	var out []watch.Event
	for _, n := range news {
		if n.Summary.ThreadID == "" {
			continue
		}
		ids, err := linkedItems(ctx, tx, acct.Name, n.Summary.ThreadID, n.Summary.InternalDate)
		if err != nil {
			return nil, err
		}
		key := cmp.Or(n.Summary.MessageID, n.Summary.Ref.String())
		self := fromSelf(n.Summary.From, acct.Address)
		for _, id := range ids {
			res, err := insert.ExecContext(ctx, id, acct.Name, key, stamp)
			if err != nil {
				return nil, fmt.Errorf("items: recording item mail: %w", err)
			}
			added, err := res.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("items: recording item mail: %w", err)
			}
			if added == 0 {
				continue
			}
			out = append(out, watch.Event{
				At: at, Kind: watch.KindItemMail,
				Account: n.Event.Account, Folder: n.Event.Folder, Ref: n.Event.Ref,
				Message: copyMessage(n.Event.Message), ItemID: id, FromSelf: self,
			})
		}
	}
	return out, nil
}

// linkedItems returns the live items linked to threadKey on account, by ID,
// leaving out links made after the message arrived, as mailBeforeLink
// decides: a message the server had before the link, now filed or copied
// into another synced folder, is not news. A zero arrival time leaves
// nothing out. It closes its Rows before returning.
func linkedItems(ctx context.Context, tx *sql.Tx, account, threadKey string, arrived time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT item_links.item_id, item_links.created_at
FROM item_links JOIN items ON items.id = item_links.item_id
WHERE item_links.account = ? AND item_links.thread_key = ? AND items.deleted_at IS NULL
ORDER BY item_links.item_id`, account, threadKey)
	if err != nil {
		return nil, fmt.Errorf("items: reading links: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var ids []string
	for rows.Next() {
		var id, created string
		if err := rows.Scan(&id, &created); err != nil {
			return nil, fmt.Errorf("items: reading links: %w", err)
		}
		createdAt, err := parseTime(created)
		if err != nil {
			return nil, fmt.Errorf("items: reading links: %w", err)
		}
		if mailBeforeLink(arrived, createdAt) {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("items: reading links: %w", err)
	}
	return ids, nil
}

// mailBeforeLink reports whether a message that arrived on the server at
// arrived was there before a link made at linked: earlier than linked less
// mailClockSkew, truncated to the second as INTERNALDATE is. A zero arrival
// time is never before.
func mailBeforeLink(arrived, linked time.Time) bool {
	return !arrived.IsZero() && arrived.Before(linked.Add(-mailClockSkew).Truncate(time.Second))
}

// fromSelf reports whether any of from is the account's own address. No
// configured address is never self.
func fromSelf(from []mailstore.Address, address string) bool {
	return address != "" && slices.ContainsFunc(from, func(a mailstore.Address) bool { return strings.EqualFold(a.Email, address) })
}

// copyMessage gives an item_mail event its own copy of the new event's
// summary, so the two events share no pointer. The slices inside are only
// read.
func copyMessage(m *watch.Message) *watch.Message {
	if m == nil {
		return nil
	}
	c := *m
	return &c
}
