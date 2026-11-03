package cache

import (
	"context"
	"strings"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// Where is a compiled search condition over messages aliased m. FTS is a
// MATCH expression for messages_fts, "" for none; when set, rows must also
// satisfy m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH FTS).
type Where struct {
	SQL  string // "" means true
	Args []any
	FTS  string
}

// SearchUIDs returns the UIDs in src matching w, highest first, or only the
// count when countOnly (UIDs then nil). Below, when not 0, keeps UIDs < below.
// Without countOnly the count is len(UIDs), as the server path reports it.
func (s *Store) SearchUIDs(ctx context.Context, account string, src mailstore.Source, w Where, below uint32, countOnly bool) ([]uint32, uint32, error) {
	var q strings.Builder
	args := []any{account, src.Folder, int64(src.UIDValidity)}
	q.WriteString(" FROM messages m WHERE m.account = ? AND m.folder = ? AND m.uidvalidity = ?")
	if below != 0 {
		q.WriteString(" AND m.uid < ?")
		args = append(args, int64(below))
	}
	if w.SQL != "" {
		q.WriteString(" AND (")
		q.WriteString(w.SQL)
		q.WriteString(")")
		args = append(args, w.Args...)
	}
	if w.FTS != "" {
		q.WriteString(" AND m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
		args = append(args, w.FTS)
	}
	if countOnly {
		var n int64
		if err := s.db.QueryRowContext(ctx, "SELECT count(*)"+q.String(), args...).Scan(&n); err != nil {
			return nil, 0, s.fail("counting matches", err)
		}
		return nil, uint32(n), nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT m.uid"+q.String()+" ORDER BY m.uid DESC", args...)
	if err != nil {
		return nil, 0, s.fail("searching", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var uids []uint32
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, 0, s.fail("searching", err)
		}
		uids = append(uids, uint32(uid))
	}
	if err := rows.Err(); err != nil {
		return nil, 0, s.fail("searching", err)
	}
	return uids, uint32(len(uids)), nil
}
