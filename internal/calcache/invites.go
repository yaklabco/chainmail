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
	Raw        []byte
	FoundAt    time.Time
}

var (
	errInviteKey = errors.New("mail invite without an account, message ref or UID")
	errInviteRef = errors.New("mail invite ref does not match its account and UID")
)

const inviteColumns = "ref, account, message_ref, uid, method, sequence, dtstamp, raw, found_at"

// latestFirst orders one UID's invites newest first: by sequence, then
// DTSTAMP, then when chainmail found them, then message ref so the order
// is total.
const latestFirst = "sequence DESC, dtstamp DESC, found_at DESC, message_ref DESC"

// PutMailInvite stores r, replacing the row of the same account, message
// ref and UID. An empty r.Ref is filled in as MailInviteRef(account, uid);
// any other ref than that one is refused.
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
		"INSERT OR REPLACE INTO mail_invites ("+inviteColumns+") VALUES ("+placeholders(9)+")",
		ref, r.Account, r.MessageRef, r.UID, r.Method, r.Sequence, formatTime(r.DTStamp), raw, formatTime(r.FoundAt)); err != nil {
		return s.fail("storing a mail invite", err)
	}
	return nil
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
		if err := rows.Scan(&r.Ref, &r.Account, &r.MessageRef, &r.UID, &r.Method, &r.Sequence, &stamp, &r.Raw, &foundAt); err != nil {
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
