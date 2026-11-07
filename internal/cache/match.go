package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// MatchOrder is the order MatchRows returns rows in.
type MatchOrder int

const (
	// OldestFirst orders rows by arrival (internal_date), then folder,
	// then UID, ascending.
	OldestFirst MatchOrder = iota
	// NewestFirst is OldestFirst reversed.
	NewestFirst
)

// maxMatchLimit bounds MatchOptions.Limit.
const maxMatchLimit = 1000

// MatchCursor is a keyset position in MatchRows' order: the arrival time,
// folder and UID of the last row a page returned. The next page starts
// strictly after it.
type MatchCursor struct {
	InternalDate time.Time
	Folder       string
	UID          uint32
}

// IsZero reports whether c is the zero cursor, which starts at the
// beginning.
func (c MatchCursor) IsZero() bool {
	return c.InternalDate.IsZero() && c.Folder == "" && c.UID == 0
}

// CursorOf returns the cursor that continues after r.
func CursorOf(r Row) MatchCursor {
	return MatchCursor{InternalDate: r.InternalDate, Folder: r.Folder, UID: r.UID}
}

// MatchOptions scope, order and bound a MatchRows call.
type MatchOptions struct {
	// Sources are the folders to match in, each with the UIDVALIDITY of
	// its progress, so rows of a reset folder never match. Empty matches
	// nothing.
	Sources []mailstore.Source
	// UIDs, when not nil, keeps only the UIDs listed for each folder. A
	// folder of Sources absent from the map, or listed with no UIDs,
	// matches nothing.
	UIDs map[string][]uint32
	// After is the keyset position in Order to continue after. Zero starts
	// at the beginning.
	After MatchCursor
	// Order is OldestFirst or NewestFirst.
	Order MatchOrder
	// Limit is the most rows returned, 1 to 1000.
	Limit int
}

// MatchRows returns the full cached rows of account that satisfy w in the
// folders of o.Sources, ordered by (internal_date, folder, uid) in
// o.Order, strictly after o.After, at most o.Limit. Arguments bind in
// placeholder order: the account, the sources, w.Args, the UID
// restriction, then w.FTS, then the cursor and the limit. A limit outside
// 1 to 1000 or an unknown order is an error; empty sources, or a UID
// restriction that leaves no source a UID, match nothing.
func (s *Store) MatchRows(ctx context.Context, account string, w Where, o MatchOptions) ([]Row, error) {
	if o.Limit < 1 || o.Limit > maxMatchLimit {
		return nil, fmt.Errorf("matching rows: limit %d is outside 1 to %d", o.Limit, maxMatchLimit)
	}
	dir, after := "", ">"
	switch o.Order {
	case OldestFirst:
	case NewestFirst:
		dir, after = " DESC", "<"
	default:
		return nil, fmt.Errorf("matching rows: unknown order %d", o.Order)
	}
	if len(o.Sources) == 0 {
		return nil, nil
	}
	var q strings.Builder
	args := []any{account}
	q.WriteString("SELECT " + rowColumns + " FROM messages m WHERE m.account = ? AND (")
	for i, src := range o.Sources {
		if i > 0 {
			q.WriteString(" OR ")
		}
		q.WriteString("(m.folder = ? AND m.uidvalidity = ?)")
		args = append(args, src.Folder, int64(src.UIDValidity))
	}
	q.WriteString(")")
	if w.SQL != "" {
		q.WriteString(" AND (" + w.SQL + ")")
		args = append(args, w.Args...)
	}
	if o.UIDs != nil {
		cond, uidArgs, err := uidRestriction(o.Sources, o.UIDs)
		if err != nil {
			return nil, err
		}
		if cond == "" {
			return nil, nil
		}
		q.WriteString(" AND (" + cond + ")")
		args = append(args, uidArgs...)
	}
	if w.FTS != "" {
		q.WriteString(" AND m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
		args = append(args, w.FTS)
	}
	if !o.After.IsZero() {
		q.WriteString(" AND (m.internal_date, m.folder, m.uid) " + after + " (?, ?, ?)")
		args = append(args, o.After.InternalDate.Unix(), o.After.Folder, int64(o.After.UID))
	}
	q.WriteString(" ORDER BY m.internal_date" + dir + ", m.folder" + dir + ", m.uid" + dir + " LIMIT ?")
	args = append(args, o.Limit)
	rows, err := s.queryRows(ctx, q.String(), args...)
	if err != nil {
		return nil, s.fail("matching rows", err)
	}
	return rows, nil
}

// uidRestriction is the condition that keeps, per folder of sources, only
// the UIDs uids lists for it, with its arguments: each folder's UIDs go in
// as one JSON array. It is "" when no folder of sources has a UID listed.
func uidRestriction(sources []mailstore.Source, uids map[string][]uint32) (string, []any, error) {
	var (
		conds []string
		args  []any
	)
	for _, src := range sources {
		list := uids[src.Folder]
		if len(list) == 0 {
			continue
		}
		b, err := json.Marshal(list)
		if err != nil {
			return "", nil, fmt.Errorf("matching rows: encoding the UIDs of a folder: %w", err)
		}
		conds = append(conds, "(m.folder = ? AND m.uid IN (SELECT value FROM json_each(?)))")
		args = append(args, src.Folder, string(b))
	}
	return strings.Join(conds, " OR "), args, nil
}

// SourcesOf pairs each of folders with the UIDVALIDITY of its current
// progress, in the order given and once each, leaving out folders the
// cache holds no progress for. Those have no rows MatchRows could return.
func (s *Store) SourcesOf(ctx context.Context, account string, folders []string) ([]mailstore.Source, error) {
	all, err := s.AllProgress(ctx, account)
	if err != nil {
		return nil, err
	}
	held := make(map[string]uint32, len(all))
	for _, p := range all {
		held[p.Folder] = p.UIDValidity
	}
	var out []mailstore.Source
	seen := make(map[string]bool, len(folders))
	for _, f := range folders {
		v, ok := held[f]
		if !ok || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, mailstore.Source{Folder: f, UIDValidity: v})
	}
	return out, nil
}
