package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jamesainslie/chainmail/internal/ref"
)

// Body is a cached message body: mailstore.Message.Raw (the original header
// and the text parts, no attachment bodies) and Message.TextClipped.
type Body struct {
	Raw         []byte
	TextClipped bool
}

// finalAttempts is the body failure count after which a message is final:
// at most 3 attempts over 24 hours. Stats counts final failures with it, and
// the prefetcher passes it to BodylessSince as maxAttempts.
const finalAttempts = 3

// PutBody stores a body and its FTS body terms; read sets last_read_at to now.
// It clears any body failure for the message. False when no row.
func (s *Store) PutBody(ctx context.Context, r ref.Ref, b Body, read bool) (bool, error) {
	text := bodyText(b.Raw) // rendered before the transaction opens
	raw := b.Raw
	if raw == nil {
		raw = []byte{} // the driver binds a nil slice as NULL
	}
	now := s.now().Unix()
	var readAt any
	if read {
		readAt = now
	}
	found := false
	err := s.inTx(ctx, "putting a body", func(tx *sql.Tx) error {
		id, row, ok, err := loadRow(ctx, tx, r)
		if err != nil || !ok {
			return err
		}
		found = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bodies (msg, raw, text_clipped, size_bytes, fetched_at, last_read_at) VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (msg) DO UPDATE SET
			   raw = excluded.raw, text_clipped = excluded.text_clipped, size_bytes = excluded.size_bytes,
			   fetched_at = excluded.fetched_at, last_read_at = coalesce(excluded.last_read_at, bodies.last_read_at)`,
			id, raw, boolInt(b.TextClipped), len(raw), now, readAt); err != nil {
			return fmt.Errorf("writing the body of message %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM body_failures WHERE msg = ?", id); err != nil {
			return fmt.Errorf("clearing the failures of message %d: %w", id, err)
		}
		return writeFTS(ctx, tx, id, row, text)
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// Body returns the body cached for r. The bool is false when there is none.
func (s *Store) Body(ctx context.Context, r ref.Ref) (Body, bool, error) {
	var (
		raw     []byte
		clipped int64
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT b.raw, b.text_clipped FROM bodies b JOIN messages m ON m.id = b.msg WHERE "+msgKeyM,
		refArgs(r)...).Scan(&raw, &clipped)
	if errors.Is(err, sql.ErrNoRows) {
		return Body{}, false, nil
	}
	if err != nil {
		return Body{}, false, s.fail("reading a body", err)
	}
	return Body{Raw: raw, TextClipped: clipped == 1}, true, nil
}

// MarkRead sets the last_read_at of r's body to now. A message without a
// cached body is left alone.
func (s *Store) MarkRead(ctx context.Context, r ref.Ref) error {
	args := append([]any{s.now().Unix()}, refArgs(r)...)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE bodies SET last_read_at = ? WHERE msg = (SELECT id FROM messages WHERE "+msgKey+")", args...); err != nil {
		return s.fail("marking a body read", err)
	}
	return nil
}

// RecordBodyFailure adds an attempt with a fixed reason code and returns the attempt count.
// A message with no cached row records nothing and returns 0.
func (s *Store) RecordBodyFailure(ctx context.Context, r ref.Ref, reason string) (int, error) {
	if reason == "" {
		return 0, errors.New("recording a body failure: empty reason")
	}
	now := s.now().Unix()
	attempts := 0
	err := s.inTx(ctx, "recording a body failure", func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM messages WHERE "+msgKey, refArgs(r)...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`INSERT INTO body_failures (msg, attempts, first_attempt_at, last_attempt_at, reason) VALUES (?, 1, ?, ?, ?)
			 ON CONFLICT (msg) DO UPDATE SET
			   attempts = body_failures.attempts + 1, last_attempt_at = excluded.last_attempt_at, reason = excluded.reason
			 RETURNING attempts`,
			id, now, now, reason).Scan(&attempts)
	})
	if err != nil {
		return 0, err
	}
	return attempts, nil
}

// BodylessSince returns refs of rows in the folder with internal_date at or
// after since that have no body and fewer than maxAttempts failures (whose
// last attempt is older than retryAfter), newest first, at most limit.
func (s *Store) BodylessSince(ctx context.Context, account, folder string, since time.Time, maxAttempts int, retryAfter time.Time, limit int) ([]ref.Ref, error) {
	if limit <= 0 {
		return nil, nil
	}
	refs, err := s.queryRefs(ctx,
		`SELECT m.account, m.folder, m.uidvalidity, m.uid, m.gmail_msgid
		 FROM messages m
		 LEFT JOIN bodies b ON b.msg = m.id
		 LEFT JOIN body_failures f ON f.msg = m.id
		 WHERE m.account = ? AND m.folder = ? AND m.internal_date >= ? AND b.msg IS NULL
		   AND (f.msg IS NULL OR (f.attempts < ? AND f.last_attempt_at < ?))
		 ORDER BY m.internal_date DESC, m.uid DESC
		 LIMIT ?`,
		account, folder, since.Unix(), maxAttempts, retryAfter.Unix(), limit)
	if err != nil {
		return nil, s.fail("listing bodyless messages", err)
	}
	return refs, nil
}

// queryRefs runs a SELECT of account, folder, uidvalidity, uid and
// gmail_msgid and returns the refs, in order.
func (s *Store) queryRefs(ctx context.Context, query string, args ...any) ([]ref.Ref, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []ref.Ref
	for rows.Next() {
		var (
			r                      ref.Ref
			uidValidity, uid, gmid int64
		)
		if err := rows.Scan(&r.Account, &r.Folder, &uidValidity, &uid, &gmid); err != nil {
			return nil, err
		}
		r.UIDValidity, r.UID, r.GmailMsgID = uint32(uidValidity), uint32(uid), uint64(gmid)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
