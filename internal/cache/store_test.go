package cache

import (
	"bytes"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// putMarker writes a budget row through raw SQL, so these tests need
// nothing but Open.
func putMarker(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(),
		"INSERT INTO budget (account, day, bytes) VALUES ('acme', '2026-11-03', 1)"); err != nil {
		t.Fatalf("writing the marker row: %v", err)
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	ctx := t.Context()
	var (
		version, autoVacuum, foreignKeys int
		journal                          string
	)
	for _, p := range []struct {
		pragma string
		dest   any
	}{
		{"user_version", &version},
		{"auto_vacuum", &autoVacuum},
		{"foreign_keys", &foreignKeys},
		{"journal_mode", &journal},
	} {
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+p.pragma).Scan(p.dest); err != nil {
			t.Fatalf("PRAGMA %s: %v", p.pragma, err)
		}
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	if autoVacuum != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (incremental)", autoVacuum)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	for _, table := range []string{"messages", "bodies", "body_failures", "messages_fts", "folders", "ref_recovery", "budget"} {
		if countRows(t, s, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table) != 1 {
			t.Errorf("table %s missing", table)
		}
	}
}

func TestOpenKeepsData(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(t.Context(), path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	putMarker(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, OpenOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	if n := countRows(t, s, "SELECT count(*) FROM budget"); n != 1 {
		t.Errorf("budget has %d rows after reopen, want the marker kept", n)
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
				s, err := Open(t.Context(), path, OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				putMarker(t, s)
				if _, err := s.db.ExecContext(t.Context(), "PRAGMA user_version = 99"); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
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
			name:   "foreign tables at version 0",
			reason: "unknown_tables",
			spoil: func(t *testing.T, path string) {
				conn, err := sql.Open("sqlite", "file:"+path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.ExecContext(t.Context(), "CREATE TABLE other (x INTEGER)"); err != nil {
					t.Fatal(err)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cache.db")
			tt.spoil(t, path)
			var logs bytes.Buffer
			s, err := Open(t.Context(), path, OpenOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
			var version int
			if err := s.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != SchemaVersion {
				t.Errorf("user_version = %d, want %d", version, SchemaVersion)
			}
			if n := countRows(t, s, "SELECT count(*) FROM budget"); n != 0 {
				t.Errorf("budget has %d rows after recreate, want 0", n)
			}
			if countRows(t, s, "SELECT count(*) FROM sqlite_master WHERE name = 'other'") != 0 {
				t.Error("the foreign table survived the recreate")
			}
			if !strings.Contains(logs.String(), "recreating cache.db") || !strings.Contains(logs.String(), "reason="+tt.reason) {
				t.Errorf("log = %q, want a recreate warning with reason=%s", logs.String(), tt.reason)
			}
			putMarker(t, s)
		})
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
	if err := removeDB(path); err != nil {
		t.Fatalf("removeDB: %v", err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still there: %v", filepath.Base(p), err)
		}
	}
	if err := removeDB(path); err != nil {
		t.Errorf("removeDB of missing files: %v, want nil", err)
	}
}

func TestOpenRefusesURICharacters(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a?b.db", "a#b.db", "a%b.db"} {
		if _, err := Open(t.Context(), filepath.Join(t.TempDir(), name), OpenOptions{}); err == nil {
			t.Errorf("Open(%q) succeeded, want an error", name)
		}
	}
}

func TestSizeBytesGrows(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	before, err := s.SizeBytes(t.Context())
	if err != nil || before <= 0 {
		t.Fatalf("SizeBytes = %d, %v; want a positive size", before, err)
	}
	if _, err := s.db.ExecContext(t.Context(),
		"INSERT INTO ref_recovery VALUES ('acme', 'INBOX', 7, 1, ?, 0)", strings.Repeat("x", 600_000)); err != nil {
		t.Fatal(err)
	}
	after, err := s.SizeBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after < before+500_000 {
		t.Errorf("SizeBytes after a 600 kB row = %d, want at least %d", after, before+500_000)
	}
}
