package cache

import (
	"context"
	"slices"
	"strings"
	"time"
)

// FolderStats counts what the cache holds for one folder.
type FolderStats struct {
	Folder         string
	Headers        uint64
	Bodies         uint64
	BodyBytes      int64
	WindowMessages uint64 // rows with internal_date at or after the window start
	WindowBodies   uint64 // of those, the rows with a body
	BodyFailures   uint64 // final failures (attempts >= 3)
}

// Stats returns per-folder counts for the account, ordered by folder. A
// folder with progress but no rows is listed with zero counts.
func (s *Store) Stats(ctx context.Context, account string, windowStart time.Time) ([]FolderStats, error) {
	ws := windowStart.Unix()
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.folder, count(*), count(b.msg), coalesce(sum(b.size_bytes), 0),
		   coalesce(sum(m.internal_date >= ?), 0),
		   coalesce(sum(m.internal_date >= ? AND b.msg IS NOT NULL), 0),
		   coalesce(sum(f.attempts >= ?), 0)
		 FROM messages m
		 LEFT JOIN bodies b ON b.msg = m.id
		 LEFT JOIN body_failures f ON f.msg = m.id
		 WHERE m.account = ?
		 GROUP BY m.folder ORDER BY m.folder`,
		ws, ws, finalAttempts, account)
	if err != nil {
		return nil, fail("reading stats", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []FolderStats
	for rows.Next() {
		var (
			st                                            FolderStats
			headers, bodies, windowMsgs, windowBodies, fx int64
		)
		if err := rows.Scan(&st.Folder, &headers, &bodies, &st.BodyBytes, &windowMsgs, &windowBodies, &fx); err != nil {
			return nil, fail("reading stats", err)
		}
		st.Headers, st.Bodies = uint64(headers), uint64(bodies)
		st.WindowMessages, st.WindowBodies, st.BodyFailures = uint64(windowMsgs), uint64(windowBodies), uint64(fx)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("reading stats", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fail("reading stats", err)
	}
	progress, err := s.AllProgress(ctx, account)
	if err != nil {
		return nil, err
	}
	for _, p := range progress {
		if !slices.ContainsFunc(out, func(st FolderStats) bool { return st.Folder == p.Folder }) {
			out = append(out, FolderStats{Folder: p.Folder})
		}
	}
	slices.SortFunc(out, func(a, b FolderStats) int { return strings.Compare(a.Folder, b.Folder) })
	return out, nil
}
