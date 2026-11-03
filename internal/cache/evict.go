package cache

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// evictState is the size cap state the prefetchers, the read path and the
// evictor share. Its zero value is ready to use.
type evictState struct {
	once sync.Once
	// kick wakes the evictor. It holds one value, so wakes coalesce.
	kick chan struct{}
	// hold is set when an eviction under the cap removed window bodies. The
	// prefetchers then fetch nothing until the file is under 80% of
	// max_size, so they do not refill what the cap just took (spec section 5).
	hold atomic.Bool
	// over is the last sweep's OverCap.
	over atomic.Bool
}

// kickChan returns the channel the evictor waits on, made on first use.
func (e *evictState) kickChan() chan struct{} {
	e.once.Do(func() { e.kick = make(chan struct{}, 1) })
	return e.kick
}

// noteWrite checks the file size after a write that stored a body and
// wakes the evictor when the file is over maxSize. A maxSize of zero or
// less means no cap. Every caller of PutBody in this package calls it.
func (s *Store) noteWrite(ctx context.Context, maxSize int64) error {
	size, err := s.SizeBytes(ctx)
	if err != nil {
		return err
	}
	if maxSize > 0 && size > maxSize {
		select {
		case s.evict.kickChan() <- struct{}{}:
		default:
		}
	}
	return nil
}

// OverCap reports whether the last eviction sweep left the file over
// max_size with no body left to evict: headers alone exceed the cap.
func (s *Store) OverCap() bool { return s.evict.over.Load() }

// SetWindowDone records whether the folder's prefetch window is complete
// and changes nothing else in its progress row, so it never undoes a
// concurrent builder update or a rebuild. A folder with no progress row is
// left without one.
func (s *Store) SetWindowDone(ctx context.Context, account, folder string, done bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE folders SET window_done = ? WHERE account = ? AND folder = ?`, boolInt(done), account, folder)
	if err != nil {
		return s.fail("setting window_done", err)
	}
	return nil
}

const (
	// evictChunk bounds the bodies one eviction query returns and one
	// transaction removes.
	evictChunk = 500
	// evictTargetPercent is the share of max_size an eviction under the cap
	// stops at (spec section 5, step 2).
	evictTargetPercent = 90
	// refetchPercent is the share of max_size the file must be under before
	// the prefetchers refill what the cap took (spec section 5).
	refetchPercent = 80
)

// EvictPolicy is one sweep's settings. Window and Retention are per
// account and must be set.
type EvictPolicy struct {
	Now       time.Time
	Window    func(account string) time.Duration // PrefetchBodies
	Retention func(account string) time.Duration // ReadRetention
	MaxSize   int64
}

// EvictResult is what a sweep removed.
type EvictResult struct {
	Bodies uint64
	// FreedBytes is the sum of the evicted bodies' size_bytes.
	FreedBytes int64
	// OverCap is set when no body is left to evict and the file is still
	// over MaxSize: headers alone exceed the cap.
	OverCap bool
}

func (r *EvictResult) add(bodies uint64, freed int64) {
	r.Bodies += bodies
	r.FreedBytes += freed
}

// evictVictim is a body a sweep removes.
type evictVictim struct {
	msg      int64
	size     int64
	account  string
	folder   string
	inWindow bool
}

// Evict applies body retention and the size cap to every account's bodies,
// in the order of spec section 5:
//
//  1. Bodies outside the account's window whose last_read_at is null or
//     older than its retention.
//  2. While the file is over MaxSize: bodies outside the window, oldest
//     last_read_at first with never read first, then window bodies, oldest
//     internal_date first, until the file is at or under 90% of MaxSize.
//
// Each removed body's FTS row is rewritten with its header terms alone, so
// headers stay searchable, and free pages go back to the file system after
// each batch, since the cap counts page_count * page_size. A MaxSize of zero
// or less means no cap. A failure returns what was removed before it.
func (s *Store) Evict(ctx context.Context, p EvictPolicy) (EvictResult, error) {
	var res EvictResult
	accounts, err := s.bodyAccounts(ctx)
	if err != nil {
		return res, err
	}
	starts := make(map[string]int64, len(accounts))
	for _, a := range accounts {
		windowStart := p.Now.Add(-p.Window(a)).Unix()
		starts[a] = windowStart
		readCut := p.Now.Add(-p.Retention(a)).Unix()
		for {
			victims, err := s.retentionVictims(ctx, a, windowStart, readCut)
			if err != nil {
				return res, err
			}
			n, freed, err := s.dropBodies(ctx, victims)
			res.add(n, freed)
			if err != nil {
				return res, err
			}
			if len(victims) < evictChunk {
				break
			}
		}
	}
	size, err := s.vacuum(ctx)
	if err != nil {
		return res, err
	}
	if p.MaxSize > 0 && size > p.MaxSize {
		target := p.MaxSize * evictTargetPercent / 100
		for size > target {
			victims, err := s.capVictims(ctx, starts, size-target)
			if err != nil {
				return res, err
			}
			if len(victims) == 0 {
				break
			}
			n, freed, err := s.dropBodies(ctx, victims)
			res.add(n, freed)
			if err != nil {
				return res, err
			}
			if size, err = s.vacuum(ctx); err != nil {
				return res, err
			}
		}
		res.OverCap = size > p.MaxSize
	}
	s.evict.over.Store(res.OverCap)
	return res, nil
}

// bodyAccounts lists the accounts with at least one cached body.
func (s *Store) bodyAccounts(ctx context.Context) ([]string, error) {
	const op = "listing accounts with bodies"
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT m.account FROM bodies b JOIN messages m ON m.id = b.msg ORDER BY m.account`)
	if err != nil {
		return nil, s.fail(op, err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, s.fail(op, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(op, err)
	}
	return out, nil
}

// retentionVictims returns up to evictChunk of the account's bodies outside
// its window (internal_date before windowStart) that were never read or
// were last read before readCut.
func (s *Store) retentionVictims(ctx context.Context, account string, windowStart, readCut int64) ([]evictVictim, error) {
	const op = "finding bodies past retention"
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.msg, b.size_bytes, m.account, m.folder, 0
		 FROM bodies b JOIN messages m ON m.id = b.msg
		 WHERE m.account = ? AND m.internal_date < ? AND (b.last_read_at IS NULL OR b.last_read_at < ?)
		 ORDER BY b.msg LIMIT ?`,
		account, windowStart, readCut, evictChunk)
	if err != nil {
		return nil, s.fail(op, err)
	}
	victims, err := scanVictims(rows, 0)
	if err != nil {
		return nil, s.fail(op, err)
	}
	return victims, nil
}

// scanVictims reads victims from rows and closes them. With need above
// zero it stops once the victims' sizes add up to need.
func scanVictims(rows *sql.Rows, need int64) ([]evictVictim, error) {
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []evictVictim
	var sum int64
	for rows.Next() {
		var v evictVictim
		if err := rows.Scan(&v.msg, &v.size, &v.account, &v.folder, &v.inWindow); err != nil {
			return nil, err
		}
		out = append(out, v)
		sum += v.size
		if need > 0 && sum >= need {
			break
		}
	}
	return out, rows.Err()
}

// dropBodies removes the victims' bodies in one transaction and returns how
// many it removed and their size_bytes. A folder that lost a window body
// has window_done cleared, and the cap hold is set.
func (s *Store) dropBodies(ctx context.Context, victims []evictVictim) (uint64, int64, error) {
	if len(victims) == 0 {
		return 0, 0, nil
	}
	var (
		n       uint64
		freed   int64
		cleared = map[[2]string]bool{}
	)
	err := s.inTx(ctx, "evicting bodies", func(tx *sql.Tx) error {
		for _, v := range victims {
			dropped, err := dropBody(ctx, tx, v.msg)
			if err != nil {
				return err
			}
			if !dropped {
				continue
			}
			n++
			freed += v.size
			if v.inWindow {
				cleared[[2]string{v.account, v.folder}] = true
			}
		}
		for k := range cleared {
			if _, err := tx.ExecContext(ctx, `UPDATE folders SET window_done = 0 WHERE account = ? AND folder = ?`, k[0], k[1]); err != nil {
				return fmt.Errorf("clearing window_done: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if len(cleared) > 0 {
		s.evict.hold.Store(true)
	}
	return n, freed, nil
}

// dropBody deletes one message's body and rewrites its FTS row with the
// header terms alone. It reports false when the body was already gone, and
// then leaves the FTS row alone: a message deleted since the victim query
// must not get an orphan FTS row that a reused id would inherit.
func dropBody(ctx context.Context, tx *sql.Tx, msg int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM bodies WHERE msg = ?`, msg)
	if err != nil {
		return false, fmt.Errorf("deleting the body of message %d: %w", msg, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	var (
		row               Row
		from, to, cc, bcc string
	)
	err = tx.QueryRowContext(ctx, `SELECT subject, from_json, to_json, cc_json, bcc_json FROM messages WHERE id = ?`, msg).
		Scan(&row.Subject, &from, &to, &cc, &bcc)
	if err != nil {
		return false, fmt.Errorf("reading the header of message %d: %w", msg, err)
	}
	for _, l := range []struct {
		dst  *[]mailstore.Address
		json string
	}{{&row.From, from}, {&row.To, to}, {&row.Cc, cc}, {&row.Bcc, bcc}} {
		if *l.dst, err = decodeList[mailstore.Address](l.json); err != nil {
			return false, fmt.Errorf("decoding the addresses of message %d: %w", msg, err)
		}
	}
	return true, writeFTS(ctx, tx, msg, row, "")
}

// vacuum returns free pages to the file system and reports the file size.
// modernc's Exec steps a statement to completion, so one call frees every
// free page.
func (s *Store) vacuum(ctx context.Context) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return 0, s.fail("returning free pages", err)
	}
	return s.SizeBytes(ctx)
}

// capVictims returns the next bodies in cap order whose sizes add up to
// need, at most evictChunk of them. starts holds each account's window
// start in Unix seconds; bodies of accounts not in it are left alone.
func (s *Store) capVictims(ctx context.Context, starts map[string]int64, need int64) ([]evictVictim, error) {
	const op = "finding bodies over the cap"
	if len(starts) == 0 {
		return nil, nil
	}
	accounts := slices.Sorted(maps.Keys(starts))
	values := make([]string, len(accounts))
	args := make([]any, 0, 2*len(accounts)+1)
	for i, a := range accounts {
		values[i] = "(?, ?)"
		args = append(args, a, starts[a])
	}
	args = append(args, evictChunk)
	rows, err := s.db.QueryContext(ctx,
		`WITH w(account, win_start) AS (VALUES `+strings.Join(values, ", ")+`)
		 SELECT b.msg, b.size_bytes, m.account, m.folder, m.internal_date >= w.win_start
		 FROM bodies b JOIN messages m ON m.id = b.msg JOIN w ON w.account = m.account
		 ORDER BY m.internal_date >= w.win_start,
		          CASE WHEN m.internal_date >= w.win_start THEN m.internal_date ELSE coalesce(b.last_read_at, -1) END,
		          m.internal_date, b.msg
		 LIMIT ?`, args...)
	if err != nil {
		return nil, s.fail(op, err)
	}
	victims, err := scanVictims(rows, need)
	if err != nil {
		return nil, s.fail(op, err)
	}
	return victims, nil
}
