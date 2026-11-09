package calcache

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// InviteFetchRow is a calendar part of a new mail message that the cache
// builder found but did not read in its bounded invite step, queued for
// the daemon's invite retry loop to read later. Method and Charset come
// from the message and are untrusted.
type InviteFetchRow struct {
	Account    string
	MessageRef string
	// Part is the IMAP section number of the calendar part.
	Part    string
	Method  string
	Charset string
	// Size is the part's encoded size as the server reported it.
	Size int64
	// Attempts counts the reads that failed and were put off.
	Attempts int
	// NextAt is when the part may be read next.
	NextAt time.Time
	// FoundAt is when the builder queued the part.
	FoundAt time.Time
}

var errFetchKey = errors.New("invite fetch without an account, message ref or part")

const fetchColumns = "account, message_ref, part, method, charset, size, attempts, next_at, found_at"

// PutInviteFetches queues rows in one transaction. A row whose account,
// message ref and part are queued already is left as it is, attempts and
// next try included, so a segment the builder applies again does not
// reset a part's backoff.
func (s *Store) PutInviteFetches(ctx context.Context, rows []InviteFetchRow) error {
	for _, r := range rows {
		if r.Account == "" || r.MessageRef == "" || r.Part == "" {
			return errFetchKey
		}
	}
	return s.inTx(ctx, "queueing invite fetches", func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO invite_fetches ("+fetchColumns+") VALUES ("+placeholders(9)+
					") ON CONFLICT (account, message_ref, part) DO NOTHING",
				r.Account, r.MessageRef, r.Part, r.Method, r.Charset, r.Size, r.Attempts,
				formatTime(r.NextAt), formatTime(r.FoundAt)); err != nil {
				return err
			}
		}
		return nil
	})
}

// DueInviteFetches returns at most limit queued parts whose next try is at
// or before now, the ones queued longest ago first.
func (s *Store) DueInviteFetches(ctx context.Context, now time.Time, limit int) ([]InviteFetchRow, error) {
	const op = "reading due invite fetches"
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+fetchColumns+" FROM invite_fetches WHERE next_at <= ? ORDER BY found_at, account, message_ref, part LIMIT ?",
		formatTime(now), limit)
	if err != nil {
		return nil, s.fail(op, err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read errors
	var out []InviteFetchRow
	for rows.Next() {
		var (
			r               InviteFetchRow
			nextAt, foundAt string
		)
		if err := rows.Scan(&r.Account, &r.MessageRef, &r.Part, &r.Method, &r.Charset, &r.Size, &r.Attempts, &nextAt, &foundAt); err != nil {
			return nil, s.fail(op, err)
		}
		if r.NextAt, err = parseTime(nextAt); err != nil {
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

// DeferInviteFetch sets the attempts and next try of r's queued part. An
// unknown part is no error.
func (s *Store) DeferInviteFetch(ctx context.Context, r InviteFetchRow) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE invite_fetches SET attempts = ?, next_at = ? WHERE account = ? AND message_ref = ? AND part = ?",
		r.Attempts, formatTime(r.NextAt), r.Account, r.MessageRef, r.Part); err != nil {
		return s.fail("putting off an invite fetch", err)
	}
	return nil
}

// DeleteInviteFetch removes a queued part. An unknown part is no error.
func (s *Store) DeleteInviteFetch(ctx context.Context, account, messageRef, part string) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM invite_fetches WHERE account = ? AND message_ref = ? AND part = ?",
		account, messageRef, part); err != nil {
		return s.fail("removing an invite fetch", err)
	}
	return nil
}
