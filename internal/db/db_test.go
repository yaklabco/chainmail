package db

import (
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestOpenStateCreatesSchema(t *testing.T) {
	t.Parallel()
	conn, err := OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	var version int
	if err := conn.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
	}
	var name string
	if err := conn.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'audit'").Scan(&name); err != nil {
		t.Errorf("audit table missing: %v", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.db")
	for range 2 {
		conn, err := OpenState(t.Context(), path)
		if err != nil {
			t.Fatalf("OpenState: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationNumbering(t *testing.T) {
	t.Parallel()
	gap := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")},
		"0003_c.sql": {Data: []byte("CREATE TABLE c (x INTEGER);")},
	}
	_, err := Open(t.Context(), filepath.Join(t.TempDir(), "x.db"), gap)
	if err == nil || !strings.Contains(err.Error(), "want number 0002") {
		t.Errorf("err = %v, want a numbering error", err)
	}
}

func TestNewerSchemaIsRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.db")
	two := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")},
		"0002_b.sql": {Data: []byte("CREATE TABLE b (x INTEGER);")},
	}
	conn, err := Open(t.Context(), path, two)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	one := fstest.MapFS{"0001_a.sql": two["0001_a.sql"]}
	if _, err := Open(t.Context(), path, one); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Errorf("err = %v, want a newer-schema error", err)
	}
}
