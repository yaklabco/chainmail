package cache

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/jamesainslie/chainmail/internal/ref"
)

// MessageIDOf returns the Message-ID cached for r, from messages or ref_recovery.
// The bool is false when neither holds a non-empty Message-ID for r.
func (s *Store) MessageIDOf(ctx context.Context, r ref.Ref) (string, bool, error) {
	for _, table := range []string{"messages", "ref_recovery"} {
		var id string
		err := s.db.QueryRowContext(ctx, "SELECT message_id FROM "+table+" WHERE "+msgKey, refArgs(r)...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, s.fail("reading a Message-ID", err)
		}
		if id != "" {
			return id, true, nil
		}
	}
	return "", false, nil
}

// FindByMessageID returns the refs of the account's current rows with messageID.
// They are ordered by folder, then UID. An empty messageID finds nothing.
func (s *Store) FindByMessageID(ctx context.Context, account, messageID string) ([]ref.Ref, error) {
	if messageID == "" {
		return nil, nil
	}
	refs, err := s.queryRefs(ctx,
		`SELECT account, folder, uidvalidity, uid, gmail_msgid FROM messages
		 WHERE account = ? AND message_id = ? ORDER BY folder, uid`,
		account, messageID)
	if err != nil {
		return nil, s.fail("finding a Message-ID", err)
	}
	return refs, nil
}

// RecoverRef finds where the message r named lives now, by Message-ID: r's
// Message-ID, from its row or from ref_recovery after a UIDVALIDITY
// rebuild, matched against account's cached rows other than r's own.
// Exactly one match is the answer; none or several are not. It is the
// recovery rule for accounts other than Google, which recover by
// X-GM-MSGID instead; the caller checks the provider and logs a failure.
func (s *Store) RecoverRef(ctx context.Context, account string, r ref.Ref) (ref.Ref, bool, error) {
	id, ok, err := s.MessageIDOf(ctx, r)
	if err != nil || !ok {
		return ref.Ref{}, false, err
	}
	found, err := s.FindByMessageID(ctx, account, id)
	if err != nil {
		return ref.Ref{}, false, err
	}
	found = slices.DeleteFunc(found, func(c ref.Ref) bool {
		return c.Folder == r.Folder && c.UIDValidity == r.UIDValidity && c.UID == r.UID
	})
	if len(found) != 1 {
		return ref.Ref{}, false, nil
	}
	return found[0], true, nil
}

// PruneRecovery deletes ref_recovery rows removed before before.
func (s *Store) PruneRecovery(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM ref_recovery WHERE removed_at < ?", before.Unix()); err != nil {
		return s.fail("pruning recovery rows", err)
	}
	return nil
}
