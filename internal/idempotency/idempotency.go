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

// Store claims and settles idempotency keys. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// mu serializes claims and settlements so a claim and its waiter entry
	// appear together. waiters holds one channel per key this process has
	// claimed; Finish closes it.
	mu      sync.Mutex
	waiters map[rowKey]chan struct{}
}

// New returns a Store over db, which must hold the state schema. now supplies
// the time written to created_at and updated_at.
func New(db *sql.DB, now func() time.Time) *Store {
	return &Store{db: db, now: now, waiters: make(map[rowKey]chan struct{})}
}

// Claim is the result of Begin.
type Claim struct {
	// Proceed is true when the caller holds the key and must perform the send,
	// then call Finish.
	Proceed bool
	// State is InProgress when Proceed is true. Otherwise it is the stored
	// state: Done, Failed or Unknown, or InProgress when another process holds
	// the key.
	State State
	// Result is the stored result when Proceed is false.
	Result []byte
}

// Begin claims (account, key) for hash. Proceed is true when the caller
// must perform the send (a new key or a retryable one). Otherwise State and
// Result are the stored outcome. A key held in_progress by this process
// waits until Finish or ctx ends, then is read again, so a waiter may itself
// proceed when the holder finished it as Retryable. A key held in_progress
// with no holder in this process returns InProgress without proceeding.
func (s *Store) Begin(ctx context.Context, account, key string, hash [32]byte) (Claim, error) {
	rk := rowKey{account, key}
	for {
		claim, wait, err := s.claim(ctx, rk, hash)
		if err != nil || wait == nil {
			return claim, err
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return Claim{}, fmt.Errorf("waiting for idempotency key: %w", ctx.Err())
		}
	}
}

// claim runs one claim attempt. A non-nil wait channel means the key is held
// by this process and the caller should wait on it and try again.
func (s *Store) claim(ctx context.Context, rk rowKey, hash [32]byte) (Claim, <-chan struct{}, error) {
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
		if wait, ok := s.waiters[rk]; ok {
			return Claim{}, wait, nil
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
// its waiter channel. s.mu must be held.
func (s *Store) commitClaim(tx *sql.Tx, rk rowKey) (Claim, <-chan struct{}, error) {
	if err := tx.Commit(); err != nil {
		return Claim{}, nil, fmt.Errorf("committing idempotency claim: %w", err)
	}
	s.waiters[rk] = make(chan struct{})
	return Claim{Proceed: true, State: InProgress}, nil, nil
}

// Finish settles a key claimed by Begin with state and result, and wakes any
// callers waiting on it. state must not be InProgress. Waiters are woken even
// when the update fails, so they read the row again instead of hanging.
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
	if wait, ok := s.waiters[rk]; ok {
		close(wait)
		delete(s.waiters, rk)
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

// Prune deletes rows created before cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM idempotency WHERE created_at < ?", cutoff.UTC().Format(timeLayout))
	if err != nil {
		return 0, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pruning idempotency keys: %w", err)
	}
	return n, nil
}

// Run prunes rows older than 7 days every hour until ctx ends. It prunes once
// on entry too, so a daemon that restarts often still prunes. A failed prune
// is retried at the next tick.
func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		// A failure leaves old rows for the next tick; nothing else depends on it.
		_, _ = s.Prune(ctx, s.now().Add(-retention))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
