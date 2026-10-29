// Package audit records what chainmail did, who asked, and how it turned out.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Result is the outcome of an audited action.
type Result string

// Results.
const (
	ResultOK    Result = "ok"
	ResultError Result = "error"
)

// Entry is one audited action. It must never hold a secret value or message content.
type Entry struct {
	// At defaults to the writer's clock.
	At         time.Time
	CallerUID  uint32
	Client     string
	Account    string
	Action     string
	Targets    []string
	Decision   string
	ApprovalID string
	Result     Result
	// Reason is the error reason when Result is ResultError.
	Reason string
}

// Writer appends entries to the audit table in state.db.
type Writer struct {
	db  *sql.DB
	now func() time.Time
}

// NewWriter returns a Writer. A nil now uses time.Now.
func NewWriter(db *sql.DB, now func() time.Time) *Writer {
	if now == nil {
		now = time.Now
	}
	return &Writer{db: db, now: now}
}

// Record appends e to the audit log.
func (w *Writer) Record(ctx context.Context, e Entry) error {
	if e.Action == "" {
		return errors.New("audit: action is required")
	}
	if e.Result != ResultOK && e.Result != ResultError {
		return fmt.Errorf("audit: invalid result %q", e.Result)
	}
	at := e.At
	if at.IsZero() {
		at = w.now()
	}
	targets := e.Targets
	if targets == nil {
		targets = []string{}
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return fmt.Errorf("audit: encoding targets: %w", err)
	}
	_, err = w.db.ExecContext(ctx,
		`INSERT INTO audit (at, caller_uid, client, account, action, targets, decision, approval_id, result, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano), e.CallerUID, e.Client, e.Account, e.Action,
		string(targetsJSON), e.Decision, e.ApprovalID, string(e.Result), e.Reason)
	if err != nil {
		return fmt.Errorf("audit: writing %s: %w", e.Action, err)
	}
	return nil
}
