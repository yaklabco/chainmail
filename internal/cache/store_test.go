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
	for _, table := range []string{"messages", "bodies", "body_failures", "messages_fts", "folders", "ref_recovery", "budget", "unreadable"} {
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
			name:   "corruption marker on an intact file",
			reason: "corrupt",
			spoil: func(t *testing.T, path string) {
				s, err := Open(t.Context(), path, OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				putMarker(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".corrupt", nil, 0o600); err != nil {
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
			if _, err := os.Stat(path + ".corrupt"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("corruption marker after recreate: %v, want none", err)
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

// notADBError returns the error SQLite gives when a file is not a database
// (SQLITE_NOTADB), which classify turns into ErrCorrupt.
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
	if !errors.Is(classify(err), ErrCorrupt) {
		t.Fatalf("reading a junk file: %v, want a corrupt-classified error", err)
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
	if !errors.Is(classify(err), ErrDiskFull) {
		t.Fatalf("insert past max_page_count: %v, want a full-classified error", err)
	}
	return err
}

func TestOpenErrKeepsClassification(t *testing.T) {
	t.Parallel()
	err := openErr("reading user_version", diskFullError(t))
	if !errors.Is(err, ErrDiskFull) {
		t.Errorf("openErr(full) = %v, want ErrDiskFull", err)
	}
	if _, ok := errors.AsType[*recreateError](err); ok {
		t.Errorf("openErr(full) = %v, want no recreate", err)
	}
	if !strings.Contains(err.Error(), "reading user_version") {
		t.Errorf("openErr(full) = %q, want the operation named", err)
	}
	if re, ok := errors.AsType[*recreateError](openErr("reading the schema", notADBError(t))); !ok || re.reason != "corrupt" {
		t.Errorf("openErr(not a database) = %v, want a corrupt recreate", re)
	}
}

func TestFailMarksCorruption(t *testing.T) {
	t.Parallel()
	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "cache.db")
		s := &Store{path: path, logger: slog.New(slog.DiscardHandler)}
		if err := s.fail("reading", notADBError(t)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("fail = %v, want ErrCorrupt", err)
		}
		if _, err := os.Stat(path + ".corrupt"); err != nil {
			t.Errorf("marker: %v, want it created", err)
		}
	})
	t.Run("full", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "cache.db")
		s := &Store{path: path, logger: slog.New(slog.DiscardHandler)}
		if err := s.fail("writing", diskFullError(t)); !errors.Is(err, ErrDiskFull) {
			t.Errorf("fail = %v, want ErrDiskFull", err)
		}
		if _, err := os.Stat(path + ".corrupt"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("marker: %v, want none for a full disk", err)
		}
	})
	t.Run("marker not writable", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "cache.db")
		if err := os.Mkdir(path+".corrupt", 0o700); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		s := &Store{path: path, logger: slog.New(slog.NewTextHandler(&logs, nil))}
		if err := s.fail("reading", notADBError(t)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("fail = %v, want ErrCorrupt", err)
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "cannot mark cache.db corrupt") {
			t.Errorf("log = %q, want a warning that the marker failed", logs.String())
		}
	})
}

// TestCorruptPageMarksAndRecreates damages a table page after Open has
// accepted the file. It overwrites the whole root page of ref_recovery, a
// page in the middle of the file, and keeps the 100-byte header and page 1
// intact, so Open still sees a valid user_version. The root page is used
// because every scan of the table reads it, so SQLite reliably reports
// SQLITE_CORRUPT (an invalid b-tree page type) on the first query.
func TestCorruptPageMarksAndRecreates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 400)
		INSERT INTO ref_recovery SELECT 'acme', 'INBOX', 1, i, '<' || i || hex(zeroblob(100)) || '>', 0 FROM n`); err != nil {
		t.Fatal(err)
	}
	var root, pageSize, pageCount int64
	if err := s.db.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_master WHERE name = 'ref_recovery'").Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT page_size, page_count FROM pragma_page_size(), pragma_page_count()").Scan(&pageSize, &pageCount); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if root <= 1 || root >= pageCount {
		t.Fatalf("ref_recovery root page %d of %d, want a middle page", root, pageCount)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	pageType := make([]byte, 1)
	if _, err := f.ReadAt(pageType, (root-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	if pageType[0] != 0x05 {
		t.Fatalf("root page type = %#x, want 0x05 (interior), so the table spans several pages", pageType[0])
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, int(pageSize)), (root-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path, OpenOptions{})
	if err != nil {
		t.Fatalf("reopen of a file with a damaged data page: %v, want it accepted", err)
	}
	if err := s.PruneRecovery(ctx, t0); !errors.Is(err, ErrCorrupt) {
		t.Errorf("PruneRecovery = %v, want ErrCorrupt", err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("marker after a corrupt error: %v, want it created", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	s, err = Open(ctx, path, OpenOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("Open after the marker: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	if !strings.Contains(logs.String(), "recreating cache.db") || !strings.Contains(logs.String(), "reason=corrupt") {
		t.Errorf("log = %q, want a recreate warning with reason=corrupt", logs.String())
	}
	if n := countRows(t, s, "SELECT count(*) FROM ref_recovery"); n != 0 {
		t.Errorf("ref_recovery has %d rows after recreate, want 0", n)
	}
	if _, err := os.Stat(path + ".corrupt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker after recreate: %v, want it removed", err)
	}
	putMarker(t, s)
	if err := s.PruneRecovery(ctx, t0); err != nil {
		t.Errorf("PruneRecovery after recreate: %v", err)
	}
}
