package items

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// requestHash identifies a create request for its idempotency key: the
// SHA-256, in hex, of the canonical JSON of the cleaned request with the
// key cleared. encoding/json writes struct fields in declaration order, so
// the encoding is stable for a given NewItem.
func requestHash(n NewItem) (string, error) {
	n.IdempotencyKey = ""
	b, err := json.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("items: hashing a create request: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// lookupRequest returns the item an earlier create with key made. found is
// false when the key is unused. A key used for a different request is
// ErrKeyMismatch.
func lookupRequest(ctx context.Context, q queryer, key, hash string) (itemID string, found bool, err error) {
	var stored string
	err = q.QueryRowContext(ctx, "SELECT request_hash, item_id FROM item_requests WHERE key = ?", key).Scan(&stored, &itemID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("items: reading an idempotency key: %w", err)
	case stored != hash:
		return "", false, ErrKeyMismatch
	}
	return itemID, true, nil
}

// putRequest records in tx that the create with key and hash made itemID.
func putRequest(ctx context.Context, tx *sql.Tx, key, hash, itemID string, at time.Time) error {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO item_requests (key, request_hash, item_id, at) VALUES (?, ?, ?, ?)",
		key, hash, itemID, formatTime(at)); err != nil {
		return fmt.Errorf("items: recording an idempotency key: %w", err)
	}
	return nil
}
