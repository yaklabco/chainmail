package calcache

import (
	"context"
	"errors"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// MailInviteRow is one calendar invite found in a mail message.
type MailInviteRow struct {
	Ref        string // MailInviteRef(account, uid)
	Account    string
	MessageRef string
	UID        string
	Method     string
	Sequence   int
	DTStamp    time.Time
	// Raw is the invite's iCalendar data, in Charset. Read it with
	// calendar.ParseInvite(Raw, Charset).
	Raw []byte
	// Charset is the MIME charset Raw is in, "" for UTF-8.
	Charset string
	FoundAt time.Time
	// Announced reports that the invite event of the row's UID was
	// published. MarkInviteAnnounced sets it on every row of the UID, and
	// PutMailInvite never clears it.
	Announced bool
}

var (
	errInviteKey = errors.New("mail invite without an account, message ref or UID")
	errInviteRef = errors.New("mail invite ref does not match its account and UID")
)

const inviteColumns = "ref, account, message_ref, uid, method, sequence, dtstamp, raw, charset, found_at, announced"

// latestFirst orders one UID's invites newest first: by sequence, then
// DTSTAMP, then when chainmail found them, then message ref so the order
// is total.
const latestFirst = "sequence DESC, dtstamp DESC, found_at DESC, message_ref DESC"

// PutMailInvite stores r, replacing the row of the same account, message
// ref and UID, except that a row once announced stays announced. An empty
// r.Ref is filled in as MailInviteRef(account, uid); any other ref than
// that one is refused.
func (s *Store) PutMailInvite(ctx context.Context, r MailInviteRow) error {
	if r.Account == "" || r.MessageRef == "" || r.UID == "" {
		return errInviteKey
	}
	ref := calendar.MailInviteRef(r.Account, r.UID)
	if r.Ref != "" && r.Ref != ref {
		return errInviteRef
	}
	raw := r.Raw
	if raw == nil {
		raw = []byte{}
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO mail_invites ("+inviteColumns+") VALUES ("+placeholders(11)+`)
		ON CONFLICT (account, message_ref, uid) DO UPDATE SET
			ref = excluded.ref, method = excluded.method, sequence = excluded.sequence, dtstamp = excluded.dtstamp,
			raw = excluded.raw, charset = excluded.charset, found_at = excluded.found_at,
			announced = max(announced, excluded.announced)`,
		ref, r.Account, r.MessageRef, r.UID, r.Method, r.Sequence, formatTime(r.DTStamp), raw, r.Charset,
		formatTime(r.FoundAt), r.Announced); err != nil {
		return s.fail("storing a mail invite", err)
	}
	return nil
}

// MarkInviteAnnounced records that the invite event of ref's UID was
// published, on every row stored for it. An unknown ref is no error.
func (s *Store) MarkInviteAnnounced(ctx context.Context, ref string) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE mail_invites SET announced = 1 WHERE ref = ?", ref); err != nil {
		return s.fail("marking a mail invite announced", err)
	}
	return nil
}

// PendingInvites returns the account's invites whose event may still be
// due, sorted by UID: for each UID first found at or after since, as a
// REQUEST, with no row announced, its latest row when that is a REQUEST.
// The caller decides from the invite itself whether the event is due.
func (s *Store) PendingInvites(ctx context.Context, account string, since time.Time) ([]MailInviteRow, error) {
	return s.queryInvites(ctx, "reading pending mail invites", `
		SELECT `+inviteColumns+` FROM (
			SELECT `+inviteColumns+`,
				row_number() OVER (PARTITION BY uid ORDER BY `+latestFirst+`) AS n,
				first_value(method) OVER (PARTITION BY uid ORDER BY found_at, message_ref) AS first_method,
				min(found_at) OVER (PARTITION BY uid) AS first_found,
				max(announced) OVER (PARTITION BY uid) AS any_announced
			FROM mail_invites WHERE account = ?
		) WHERE n = 1 AND method = ? AND first_method = ? AND any_announced = 0 AND first_found >= ?
		ORDER BY uid`, account, calendar.MethodRequest, calendar.MethodRequest, formatTime(since))
}

// MailInvites returns the account's latest invite per UID, by sequence and
// then DTSTAMP, sorted by UID. A UID whose latest invite is a CANCEL is
// returned with that row, so callers can tell the invite was withdrawn.
func (s *Store) MailInvites(ctx context.Context, account string) ([]MailInviteRow, error) {
	return s.queryInvites(ctx, "reading mail invites", `
		SELECT `+inviteColumns+` FROM (
			SELECT `+inviteColumns+`, row_number() OVER (PARTITION BY uid ORDER BY `+latestFirst+`) AS n
			FROM mail_invites WHERE account = ?
		) WHERE n = 1 ORDER BY uid`, account)
}

// MailInvite returns the latest invite with ref, or ErrNotFound.
func (s *Store) MailInvite(ctx context.Context, ref string) (MailInviteRow, error) {
	rows, err := s.queryInvites(ctx, "reading a mail invite",
		"SELECT "+inviteColumns+" FROM mail_invites WHERE ref = ? ORDER BY "+latestFirst+" LIMIT 1", ref)
	if err != nil {
		return MailInviteRow{}, err
	}
	if len(rows) == 0 {
		return MailInviteRow{}, ErrNotFound
	}
	return rows[0], nil
}

func (s *Store) queryInvites(ctx context.Context, op, query string, args ...any) ([]MailInviteRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, s.fail(op, err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read errors
	var out []MailInviteRow
	for rows.Next() {
		var (
			r              MailInviteRow
			stamp, foundAt string
		)
		if err := rows.Scan(&r.Ref, &r.Account, &r.MessageRef, &r.UID, &r.Method, &r.Sequence, &stamp, &r.Raw, &r.Charset, &foundAt, &r.Announced); err != nil {
			return nil, s.fail(op, err)
		}
		if r.DTStamp, err = parseTime(stamp); err != nil {
			return nil, err
		}
		if r.FoundAt, err = parseTime(foundAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(op, err)
	}
	return out, nil
}
