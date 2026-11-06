package audit

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestRecord(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	fixed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	w := NewWriter(conn, func() time.Time { return fixed })
	err := w.Record(t.Context(), Entry{
		CallerUID: 501,
		Client:    "cli",
		Account:   "work",
		Action:    "secret.set",
		Targets:   []string{"accounts/work"},
		Result:    ResultOK,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var at, client, account, action, targets, result string
	var uid uint32
	row := conn.QueryRowContext(t.Context(), "SELECT at, caller_uid, client, account, action, targets, result FROM audit")
	if err := row.Scan(&at, &uid, &client, &account, &action, &targets, &result); err != nil {
		t.Fatal(err)
	}
	if at != "2026-10-27T12:00:00.000000000Z" || uid != 501 || client != "cli" || account != "work" ||
		action != "secret.set" || targets != `["accounts/work"]` || result != "ok" {
		t.Errorf("row = %s %d %s %s %s %s %s", at, uid, client, account, action, targets, result)
	}
}

func TestRecordTimestampsSortChronologically(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	w := NewWriter(conn, nil)
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// Recorded out of order. Variable-width fractions ("00Z", "00.5Z", "00.15Z")
	// sort wrongly as text, so these catch a layout that trims trailing zeros.
	offsets := []time.Duration{
		500 * time.Millisecond,
		0,
		150 * time.Millisecond,
		100 * time.Millisecond,
		time.Second,
	}
	for _, d := range offsets {
		if err := w.Record(t.Context(), Entry{At: base.Add(d), Action: "x", Result: ResultOK}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := conn.QueryContext(t.Context(), "SELECT at FROM audit ORDER BY at")
	if err != nil {
		t.Fatal(err)
	}
	var got []time.Time
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		got = append(got, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(offsets) {
		t.Fatalf("got %d rows, want %d", len(got), len(offsets))
	}
	if !slices.IsSortedFunc(got, func(a, b time.Time) int { return a.Compare(b) }) {
		t.Errorf("ORDER BY at is not chronological: %v", got)
	}
}

func TestRecordExplicitAtOverridesClock(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	w := NewWriter(conn, func() time.Time { return clock })
	explicit := time.Date(2026, 10, 27, 7, 30, 0, 250_000_000, time.FixedZone("x", -5*60*60))
	if err := w.Record(t.Context(), Entry{At: explicit, Action: "x", Result: ResultOK}); err != nil {
		t.Fatal(err)
	}
	var at string
	if err := conn.QueryRowContext(t.Context(), "SELECT at FROM audit").Scan(&at); err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-27T12:30:00.250000000Z"; at != want {
		t.Errorf("at = %q, want %q", at, want)
	}
}

func TestRecordStoresOptionalFields(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	w := NewWriter(conn, nil)
	err := w.Record(t.Context(), Entry{
		Action:     "mail.send",
		Decision:   "held",
		ApprovalID: "ap-123",
		Result:     ResultError,
		Reason:     "denied",
	})
	if err != nil {
		t.Fatal(err)
	}
	var targets, decision, approvalID, result, reason string
	row := conn.QueryRowContext(t.Context(), "SELECT targets, decision, approval_id, result, reason FROM audit")
	if err := row.Scan(&targets, &decision, &approvalID, &result, &reason); err != nil {
		t.Fatal(err)
	}
	if targets != "[]" || decision != "held" || approvalID != "ap-123" || result != "error" || reason != "denied" {
		t.Errorf("row = %s %s %s %s %s", targets, decision, approvalID, result, reason)
	}
}

func TestRecordValidates(t *testing.T) {
	t.Parallel()
	w := NewWriter(openDB(t), nil)
	if err := w.Record(t.Context(), Entry{Result: ResultOK}); err == nil {
		t.Error("missing action accepted")
	}
	if err := w.Record(t.Context(), Entry{Action: "x", Result: "maybe"}); err == nil {
		t.Error("invalid result accepted")
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	w := NewWriter(conn, nil)
	if err := w.Record(t.Context(), Entry{Action: "x", Result: ResultOK}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"UPDATE audit SET action = 'y'", "DELETE FROM audit"} {
		_, err := conn.ExecContext(t.Context(), stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want an append-only error", stmt, err)
		}
	}
}
