package sqlitefile

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// errFull stands in for a caller's disk-full sentinel.
var errFull = errors.New("test.db is full")

// classifyFull wraps a SQLITE_FULL error in errFull, as internal/cache
// does with ErrDiskFull.
func classifyFull(err error) error {
	if se, ok := errors.AsType[*sqlite.Error](err); ok && se.Code()&0xff == sqlite3.SQLITE_FULL {
		return fmt.Errorf("%w: %w", errFull, err)
	}
	return err
}

// testSpec is a one-table cache file; reasons collects OnRecreate calls.
func testSpec(reasons *[]string) Spec {
	return Spec{
		What:     "test cache",
		Name:     "test.db",
		Version:  3,
		Schema:   "CREATE TABLE kept (x INTEGER);",
		Classify: classifyFull,
		OnRecreate: func(reason string) {
			*reasons = append(*reasons, reason)
		},
	}
}

func openTest(t *testing.T, path string, reasons *[]string) *sql.DB {
	t.Helper()
	conn, err := Open(t.Context(), path, testSpec(reasons))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	return conn
}

func scalar(t *testing.T, conn *sql.DB, query string, dest any) {
	t.Helper()
	if err := conn.QueryRowContext(t.Context(), query).Scan(dest); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(permissiveDir(t), "test.db")
	var reasons []string
	conn := openTest(t, path, &reasons)
	var (
		version, autoVacuum, foreignKeys, tables int
		journal                                  string
	)
	scalar(t, conn, "PRAGMA user_version", &version)
	scalar(t, conn, "PRAGMA auto_vacuum", &autoVacuum)
	scalar(t, conn, "PRAGMA foreign_keys", &foreignKeys)
	scalar(t, conn, "PRAGMA journal_mode", &journal)
	scalar(t, conn, "SELECT count(*) FROM sqlite_master WHERE name = 'kept'", &tables)
	if version != 3 || autoVacuum != 2 || foreignKeys != 1 || journal != "wal" || tables != 1 {
		t.Errorf("user_version %d, auto_vacuum %d, foreign_keys %d, journal %q, tables %d; want 3, 2, 1, wal, 1",
			version, autoVacuum, foreignKeys, journal, tables)
	}
	if conn.Stats().MaxOpenConnections != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", conn.Stats().MaxOpenConnections)
	}
	wantMode(t, path, PrivateMode, true)
	if len(reasons) != 0 {
		t.Errorf("OnRecreate calls for a new file = %v, want none", reasons)
	}
}

func TestOpenKeepsData(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.db")
	var reasons []string
	conn, err := Open(t.Context(), path, testSpec(&reasons))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "INSERT INTO kept VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn = openTest(t, path, &reasons)
	var n int
	scalar(t, conn, "SELECT count(*) FROM kept", &n)
	if n != 1 || len(reasons) != 0 {
		t.Errorf("after reopen: %d rows, OnRecreate calls %v; want 1 row and none", n, reasons)
	}
}

func TestOpenRecreates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason string
		spoil  func(t *testing.T, path string)
	}{
		{
			name:   "other user_version",
			reason: "version",
			spoil: func(t *testing.T, path string) {
				rawExec(t, path, "CREATE TABLE kept (x INTEGER)", "INSERT INTO kept VALUES (1)", "PRAGMA user_version = 99")
			},
		},
		{
			name:   "not a database",
			reason: "corrupt",
			spoil: func(t *testing.T, path string) {
				if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 300), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "corruption marker on an intact file",
			reason: "corrupt",
			spoil: func(t *testing.T, path string) {
				rawExec(t, path, "CREATE TABLE kept (x INTEGER)", "INSERT INTO kept VALUES (1)", "PRAGMA user_version = 3")
				if err := MarkCorrupt(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "foreign tables at version 0",
			reason: "unknown_tables",
			spoil: func(t *testing.T, path string) {
				rawExec(t, path, "CREATE TABLE other (x INTEGER)")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "test.db")
			tt.spoil(t, path)
			var reasons []string
			conn := openTest(t, path, &reasons)
			var version, rows, other int
			scalar(t, conn, "PRAGMA user_version", &version)
			scalar(t, conn, "SELECT count(*) FROM kept", &rows)
			scalar(t, conn, "SELECT count(*) FROM sqlite_master WHERE name = 'other'", &other)
			if version != 3 || rows != 0 || other != 0 {
				t.Errorf("after recreate: user_version %d, %d rows, %d foreign tables; want 3, 0, 0", version, rows, other)
			}
			if len(reasons) != 1 || reasons[0] != tt.reason {
				t.Errorf("OnRecreate calls = %v, want [%s]", reasons, tt.reason)
			}
			if _, err := os.Stat(MarkerPath(path)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("corruption marker after recreate: %v, want none", err)
			}
		})
	}
}

// rawExec runs queries against the file at path on its own connection.
func rawExec(t *testing.T, path string, queries ...string) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // a close error changes nothing here
	for _, q := range queries {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestOpenRefusesURICharacters(t *testing.T) {
	t.Parallel()
	var reasons []string
	for _, name := range []string{"a?b.db", "a#b.db", "a%b.db"} {
		_, err := Open(t.Context(), filepath.Join(t.TempDir(), name), testSpec(&reasons))
		if err == nil || !strings.Contains(err.Error(), "test cache path") || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("Open(%q) = %v, want the path refused", name, err)
		}
	}
}

func TestRemoveDB(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cache.db")
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveDB(path); err != nil {
		t.Fatalf("RemoveDB: %v", err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still there: %v", filepath.Base(p), err)
		}
	}
	if err := RemoveDB(path); err != nil {
		t.Errorf("RemoveDB of missing files: %v, want nil", err)
	}
}

// notADBError returns the error SQLite gives when a file is not a database
// (SQLITE_NOTADB), which IsCorrupt reports.
func notADBError(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 300), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // a close error changes nothing here
	var version int
	err = conn.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version)
	if !IsCorrupt(err) {
		t.Fatalf("reading a junk file: %v, want a corrupt error", err)
	}
	return err
}

// diskFullError returns a real SQLITE_FULL error, made by capping
// max_page_count below what an insert needs.
func diskFullError(t *testing.T) error {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // a close error changes nothing here
	conn.SetMaxOpenConns(1)             // max_page_count is per connection
	ctx := t.Context()
	for _, q := range []string{"CREATE TABLE t (x BLOB)", "PRAGMA max_page_count = 2"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO t VALUES (zeroblob(100000))")
	if !errors.Is(classifyFull(err), errFull) {
		t.Fatalf("insert past max_page_count: %v, want a full-classified error", err)
	}
	return err
}

func TestIsCorrupt(t *testing.T) {
	t.Parallel()
	if !IsCorrupt(notADBError(t)) || !IsCorrupt(fmt.Errorf("wrapped: %w", notADBError(t))) {
		t.Error("IsCorrupt(not a database) = false, want true")
	}
	if IsCorrupt(diskFullError(t)) || IsCorrupt(errors.New("plain")) || IsCorrupt(nil) {
		t.Error("IsCorrupt of a full disk, a plain error or nil = true, want false")
	}
}

func TestOpenErrKeepsClassification(t *testing.T) {
	t.Parallel()
	var reasons []string
	spec := testSpec(&reasons)
	err := spec.openErr("reading user_version", diskFullError(t))
	if !errors.Is(err, errFull) {
		t.Errorf("openErr(full) = %v, want the caller's classification", err)
	}
	if _, ok := errors.AsType[*recreateError](err); ok {
		t.Errorf("openErr(full) = %v, want no recreate", err)
	}
	if !strings.Contains(err.Error(), "reading user_version") {
		t.Errorf("openErr(full) = %q, want the operation named", err)
	}
	re, ok := errors.AsType[*recreateError](spec.openErr("reading the schema", notADBError(t)))
	if !ok || re.reason != "corrupt" {
		t.Errorf("openErr(not a database) = %v, want a corrupt recreate", re)
	}
	if ok && re.Error() != "test.db must be recreated: corrupt" {
		t.Errorf("recreate error = %q, want the file named", re.Error())
	}
}

func TestMarkCorrupt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(permissiveDir(t), "test.db")
	if err := MarkCorrupt(path); err != nil {
		t.Fatalf("MarkCorrupt: %v", err)
	}
	wantMode(t, MarkerPath(path), PrivateMode, true)
	if MarkerPath(path) != path+".corrupt" {
		t.Errorf("MarkerPath = %q, want %q", MarkerPath(path), path+".corrupt")
	}
	blocked := filepath.Join(filepath.Dir(path), "blocked.db")
	if err := os.Mkdir(MarkerPath(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MarkCorrupt(blocked); err == nil {
		t.Error("MarkCorrupt where a directory holds the marker's name succeeded, want an error")
	}
}
