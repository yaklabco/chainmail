package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/watch"
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

// CompleteWindow sets the folder's window_done in one guarded update, only
// if no row of the folder with internal_date at or after since lacks a
// body, unless its failure count has reached maxAttempts (final). An
// eviction that clears window_done can never be overwritten by a check
// made before it. It reports whether it set the flag; a folder with no
// progress row is left without one.
func (s *Store) CompleteWindow(ctx context.Context, account, folder string, since time.Time, maxAttempts int) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE folders SET window_done = 1
		 WHERE account = ? AND folder = ? AND NOT EXISTS (
		   SELECT 1 FROM messages m
		   LEFT JOIN bodies b ON b.msg = m.id
		   LEFT JOIN body_failures f ON f.msg = m.id
		   WHERE m.account = ? AND m.folder = ? AND m.internal_date >= ? AND b.msg IS NULL
		     AND (f.msg IS NULL OR f.attempts < ?))`,
		account, folder, account, folder, since.Unix(), maxAttempts)
	if err != nil {
		return false, s.fail("completing the prefetch window", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, s.fail("completing the prefetch window", err)
	}
	return n > 0, nil
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
	// evictInterval is how often the evictor sweeps without a kick.
	evictInterval = time.Hour
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

// evictVictim is a body a sweep removes: its row id, and the immutable
// identity of the message that row held when the victim query ran, which
// dropBodies checks again inside its transaction, since SQLite reuses the
// id of a deleted row. winStart is the account's window start in Unix
// seconds. inWindow is the victim query's view of the window, which orders
// the cap's victims; dropBodies derives the window effect from the row as
// it is when it deletes.
type evictVictim struct {
	msg         int64
	size        int64
	account     string
	folder      string
	uidValidity uint32
	uid         uint32
	winStart    int64
	inWindow    bool
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
		`SELECT b.msg, b.size_bytes, m.account, m.folder, m.uidvalidity, m.uid, ?, 0
		 FROM bodies b JOIN messages m ON m.id = b.msg
		 WHERE m.account = ? AND m.internal_date < ? AND (b.last_read_at IS NULL OR b.last_read_at < ?)
		 ORDER BY b.msg LIMIT ?`,
		windowStart, account, windowStart, readCut, evictChunk)
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
		if err := rows.Scan(&v.msg, &v.size, &v.account, &v.folder, &v.uidValidity, &v.uid, &v.winStart, &v.inWindow); err != nil {
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
// many it removed and their size_bytes. A victim whose row id no longer
// holds the message the victim query saw (deleted, its id perhaps reused
// by another message) is skipped. A folder that lost a window body, as the
// row reads inside the transaction, has window_done cleared, and the cap
// hold is set.
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
			dropped, inWindow, err := dropBody(ctx, tx, v)
			if err != nil {
				return err
			}
			if !dropped {
				continue
			}
			n++
			freed += v.size
			if inWindow {
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

// dropBody deletes the victim's body and rewrites its FTS row with the
// header terms alone, only while the victim's row id still holds the same
// message: account, folder, UIDVALIDITY and UID, which never change for a
// row. It reports false when the row is gone or holds another message, or
// the body was already gone, and then changes nothing: a message deleted
// since the victim query must not get an orphan FTS row that a reused id
// would inherit, and a reused id's body is not the victim's. It also
// reports whether the row is inside the window, from its internal_date.
func dropBody(ctx context.Context, tx *sql.Tx, v evictVictim) (dropped, inWindow bool, err error) {
	var (
		row               Row
		internalDate      int64
		from, to, cc, bcc string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT internal_date, subject, from_json, to_json, cc_json, bcc_json FROM messages
		 WHERE id = ? AND account = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
		v.msg, v.account, v.folder, int64(v.uidValidity), int64(v.uid)).
		Scan(&internalDate, &row.Subject, &from, &to, &cc, &bcc)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("reading the header of message %d: %w", v.msg, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM bodies WHERE msg = ?`, v.msg)
	if err != nil {
		return false, false, fmt.Errorf("deleting the body of message %d: %w", v.msg, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, false, err
	}
	for _, l := range []struct {
		dst  *[]mailstore.Address
		json string
	}{{&row.From, from}, {&row.To, to}, {&row.Cc, cc}, {&row.Bcc, bcc}} {
		if *l.dst, err = decodeList[mailstore.Address](l.json); err != nil {
			return false, false, fmt.Errorf("decoding the addresses of message %d: %w", v.msg, err)
		}
	}
	return true, internalDate >= v.winStart, writeFTS(ctx, tx, v.msg, row, "")
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
		 SELECT b.msg, b.size_bytes, m.account, m.folder, m.uidvalidity, m.uid, w.win_start, m.internal_date >= w.win_start
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

// EvictorOptions configures the daemon-wide Evictor.
type EvictorOptions struct {
	Store *Store
	// Window and Retention return an account's PrefetchBodies and
	// ReadRetention, read at every sweep.
	Window    func(account string) time.Duration
	Retention func(account string) time.Duration
	// MaxSize returns [cache] max_size in bytes, read at every sweep.
	MaxSize func() int64
	// Clock nil means the system clock.
	Clock watch.Clock
	// Logger nil discards.
	Logger *slog.Logger
	// OnError, when set, receives each failed sweep's error, unless the
	// sweep's context ended. The daemon watches it for a full disk.
	OnError func(err error)
}

// Evictor runs body eviction for the whole cache file: the cap is
// file-wide, so one Evictor serves every account.
type Evictor struct {
	o  EvictorOptions
	mu sync.Mutex // one sweep at a time
}

// NewEvictor returns an Evictor. Run starts its loop; Sweep runs one sweep.
func NewEvictor(o EvictorOptions) *Evictor {
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Evictor{o: o}
}

// Run sweeps every evictInterval and whenever a write leaves the file over
// max_size, until ctx ends.
func (e *Evictor) Run(ctx context.Context) {
	kick := e.o.Store.evict.kickChan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-e.o.Clock.After(evictInterval):
		}
		if _, err := e.Sweep(ctx); err != nil && ctx.Err() != nil {
			return
		}
	}
}

// Sweep runs one eviction sweep now with the current settings. It logs what
// it removed, warns when headers alone exceed max_size, and reports a
// failure to OnError.
func (e *Evictor) Sweep(ctx context.Context) (EvictResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res, err := e.o.Store.Evict(ctx, EvictPolicy{
		Now: e.o.Clock.Now(), Window: e.o.Window, Retention: e.o.Retention, MaxSize: e.o.MaxSize(),
	})
	if err != nil {
		if ctx.Err() == nil {
			e.o.Logger.Warn("cache eviction failed", "err", err)
			if e.o.OnError != nil {
				e.o.OnError(err)
			}
		}
		return res, err
	}
	if res.Bodies > 0 {
		e.o.Logger.Info("cache bodies evicted", "bodies", res.Bodies, "freed_bytes", res.FreedBytes)
	}
	if res.OverCap {
		e.o.Logger.Warn("cache.db is over max_size with message headers alone; raise [cache] max_size")
	}
	return res, nil
}
