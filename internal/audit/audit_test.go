package audit

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/db"
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
	if at != "2026-10-27T12:00:00Z" || uid != 501 || client != "cli" || account != "work" ||
		action != "secret.set" || targets != `["accounts/work"]` || result != "ok" {
		t.Errorf("row = %s %d %s %s %s %s %s", at, uid, client, account, action, targets, result)
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
	if _, err := conn.ExecContext(t.Context(), "UPDATE audit SET action = 'y'"); err == nil {
		t.Error("UPDATE succeeded")
	}
	if _, err := conn.ExecContext(t.Context(), "DELETE FROM audit"); err == nil {
		t.Error("DELETE succeeded")
	}
}
