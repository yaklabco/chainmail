// Package idempotency records send-type requests by (account, key) so a
// repeated request returns the first outcome instead of sending again. Rows
// live in state.db's idempotency table and are kept for seven days.
package idempotency

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State is the stored outcome of a keyed request.
type State string

// The states a row moves through. InProgress is held while a send runs; the
// other four are written by Finish or Recover.
const (
	InProgress State = "in_progress"
	Done       State = "done"
	Failed     State = "failed"
	Retryable  State = "retryable"
	Unknown    State = "unknown"
)

// ErrKeyMismatch reports a key that was first used with a different request hash.
var ErrKeyMismatch = errors.New("idempotency key already used for a different request")

const (
	maxKeyLen     = 128
	retention     = 7 * 24 * time.Hour
	pruneInterval = time.Hour
)

// timeLayout stores times as fixed-width UTC with nine fractional digits, so
// the created_at column sorts and range-compares correctly as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// ValidKey reports whether k is 1 to 128 bytes of [A-Za-z0-9._:-].
func ValidKey(k string) bool {
	if k == "" || len(k) > maxKeyLen {
		return false
	}
	for i := range len(k) {
		c := k[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

type rowKey struct{ account, key string }

// attempt is one in-process send attempt on a key, from the claim that
// started it to its Finish. Callers that wait on it receive its own outcome,
// so a retry claimed later cannot change what they see.
type attempt struct {
	done chan struct{} // closed by Finish after state and result are set
	// state and result are the outcome Finish recorded. They are written
	// before done is closed and only read after it is.
	state  State
	result []byte
	// waiting counts callers blocked on done. It is guarded by Store.mu.
	waiting int
}

// Store claims and settles idempotency keys. It is safe for concurrent use.
type Store struct {
	// Logger receives failures of the hourly prune in Run. Records never hold
	// keys, accounts or results. Set it before calling Run. A nil Logger
	// uses slog.Default.
	Logger *slog.Logger

	db  *sql.DB
	now func() time.Time

	// mu serializes claims, settlements and prunes, so a claim and its
	// attempt appear together and a prune never removes a row this process
	// is still sending on. attempts holds the attempt for each key this
	// process has claimed and not yet finished.
	mu       sync.Mutex
	attempts map[rowKey]*attempt
}

// New returns a Store over db, which must hold the state schema. now supplies
// the time written to created_at and updated_at.
func New(db *sql.DB, now func() time.Time) *Store {
	return &Store{db: db, now: now, attempts: make(map[rowKey]*attempt)}
}

// Claim is the result of Begin.
type Claim struct {
	// Proceed is true when the caller holds the key and must perform the send,
	// then call Finish.
	Proceed bool
	// State is InProgress when Proceed is true. Otherwise it is the stored
	// state (Done, Failed or Unknown), the outcome of the in-process attempt
	// the caller waited for (any state Finish accepts, Retryable included),
	// or InProgress when another process holds the key.
	State State
	// Result is the stored or awaited result when Proceed is false.
	Result []byte
}

// Begin claims (account, key) for hash. Proceed is true when the caller
// must perform the send (a new key or a retryable one). Otherwise State and
// Result are the stored outcome. A key held in_progress by this process
// waits until Finish or ctx ends and returns that attempt's outcome with
// Proceed false, even when it is Retryable: a waiting duplicate never becomes
// the next attempt, because a send is never retried automatically. A key
// held in_progress with no holder in this process returns InProgress without
// proceeding.
func (s *Store) Begin(ctx context.Context, account, key string, hash [32]byte) (Claim, error) {
	rk := rowKey{account, key}
	claim, att, err := s.claim(ctx, rk, hash)
	if err != nil || att == nil {
		return claim, err
	}
	select {
	case <-att.done:
		return Claim{State: att.state, Result: bytes.Clone(att.result)}, nil
	case <-ctx.Done():
		s.mu.Lock()
		att.waiting--
		s.mu.Unlock()
		return Claim{}, fmt.Errorf("waiting for idempotency key: %w", ctx.Err())
	}
}

// claim runs the claim transaction. A non-nil attempt means the key is held
// by this process and the caller should wait for it to finish.
func (s *Store) claim(ctx context.Context, rk rowKey, hash [32]byte) (Claim, *attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, nil, fmt.Errorf("beginning idempotency claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report

	now := s.now().UTC().Format(timeLayout)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency (account, key, request_hash, state, result, created_at, updated_at)
		 VALUES (?, ?, ?, ?, x'', ?, ?) ON CONFLICT (account, key) DO NOTHING`,
		rk.account, rk.key, hash[:], string(InProgress), now, now)
	if err != nil {
		return Claim{}, nil, fmt.Errorf("inserting idempotency key: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return Claim{}, nil, fmt.Errorf("inserting idempotency key: %w", err)
	}
	if inserted == 1 {
		return s.commitClaim(tx, rk)
	}

	var (
		storedHash []byte
		state      string
		result     []byte
	)
	err = tx.QueryRowContext(ctx,
		"SELECT request_hash, state, result FROM idempotency WHERE account = ? AND key = ?",
		rk.account, rk.key).Scan(&storedHash, &state, &result)
	if err != nil {
		return Claim{}, nil, fmt.Errorf("reading idempotency key: %w", err)
	}
	if !bytes.Equal(storedHash, hash[:]) {
		return Claim{}, nil, ErrKeyMismatch
	}

	switch State(state) {
	case Retryable:
		_, err := tx.ExecContext(ctx,
			"UPDATE idempotency SET state = ?, result = x'', updated_at = ? WHERE account = ? AND key = ?",
			string(InProgress), now, rk.account, rk.key)
		if err != nil {
			return Claim{}, nil, fmt.Errorf("reclaiming idempotency key: %w", err)
		}
		return s.commitClaim(tx, rk)
	case InProgress:
		if att, ok := s.attempts[rk]; ok {
			att.waiting++
			return Claim{}, att, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, nil, fmt.Errorf("committing idempotency read: %w", err)
	}
	if len(result) == 0 {
		result = nil
	}
	return Claim{State: State(state), Result: result}, nil, nil
}

// commitClaim commits a claim that leaves the row in_progress and registers
// its attempt. s.mu must be held.
func (s *Store) commitClaim(tx *sql.Tx, rk rowKey) (Claim, *attempt, error) {
	if err := tx.Commit(); err != nil {
		return Claim{}, nil, fmt.Errorf("committing idempotency claim: %w", err)
	}
	s.attempts[rk] = &attempt{done: make(chan struct{})}
	return Claim{Proceed: true, State: InProgress}, nil, nil
}

// Finish settles a key claimed by Begin with state and result, and wakes any
// callers waiting on it with that outcome. state must not be InProgress.
// Waiters are woken even when the update fails, so they do not hang.
func (s *Store) Finish(ctx context.Context, account, key string, state State, result []byte) error {
	switch state {
	case Done, Failed, Retryable, Unknown:
	default:
		return fmt.Errorf("finishing idempotency key with state %q", state)
	}
	if result == nil {
		result = []byte{} // the column is NOT NULL
	}
	rk := rowKey{account, key}

	s.mu.Lock()
	defer s.mu.Unlock()
	if att, ok := s.attempts[rk]; ok {
		att.state = state
		if len(result) > 0 {
			att.result = bytes.Clone(result)
		}
		close(att.done)
		delete(s.attempts, rk)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE idempotency SET state = ?, result = ?, updated_at = ?
		 WHERE account = ? AND key = ? AND state = ?`,
		string(state), result, s.now().UTC().Format(timeLayout), account, key, string(InProgress))
	if err != nil {
		return fmt.Errorf("finishing idempotency key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finishing idempotency key: %w", err)
	}
	if n != 1 {
		return errors.New("finishing idempotency key: no claim in progress")
	}
	return nil
}

// Recover marks in_progress rows left by an earlier process as unknown. Call once at startup.
func (s *Store) Recover(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE idempotency SET state = ?, updated_at = ? WHERE state = ?",
		string(Unknown), s.now().UTC().Format(timeLayout), string(InProgress))
	if err != nil {
		return 0, fmt.Errorf("recovering idempotency keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recovering idempotency keys: %w", err)
	}
	return n, nil
}

// Prune deletes rows created before cutoff. A row this process holds
// in_progress is kept until its attempt finishes, so its waiters still get
// its outcome and a new Begin waits on it instead of starting another send.
// The next prune after Finish deletes it.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report

	before := cutoff.UTC().Format(timeLayout)
	res, err := tx.ExecContext(ctx,
		"DELETE FROM idempotency WHERE created_at < ? AND state != ?", before, string(InProgress))
	if err != nil {
		return 0, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pruning idempotency keys: %w", err)
	}

	stale, err := s.staleInProgress(ctx, tx, before)
	if err != nil {
		return 0, err
	}
	for _, rk := range stale {
		if _, held := s.attempts[rk]; held {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM idempotency WHERE account = ? AND key = ? AND state = ?",
			rk.account, rk.key, string(InProgress)); err != nil {
			return 0, fmt.Errorf("pruning idempotency keys: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	return n, nil
}

// staleInProgress lists in_progress rows created before the formatted cutoff.
func (s *Store) staleInProgress(ctx context.Context, tx *sql.Tx, before string) ([]rowKey, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT account, key FROM idempotency WHERE created_at < ? AND state = ?", before, string(InProgress))
	if err != nil {
		return nil, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var keys []rowKey
	for rows.Next() {
		var rk rowKey
		if err := rows.Scan(&rk.account, &rk.key); err != nil {
			return nil, fmt.Errorf("pruning idempotency keys: %w", err)
		}
		keys = append(keys, rk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	return keys, nil
}

// Run prunes rows older than 7 days every hour until ctx ends. It prunes once
// on entry too, so a daemon that restarts often still prunes. A failed prune
// is logged to Logger and retried at the next tick; one cut short because ctx
// ended is not logged.
func (s *Store) Run(ctx context.Context) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		if _, err := s.Prune(ctx, s.now().Add(-retention)); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "pruning idempotency keys failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
