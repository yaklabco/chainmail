package calcache_test

import (
	"bytes"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
)

// openCache opens a fresh calendar.db in a temporary directory.
func openCache(t *testing.T) (*calcache.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), calcache.FileName)
	s, err := calcache.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	return s, path
}

// rawQuery runs one scalar query against the file at path on its own
// connection.
func rawQuery(t *testing.T, path, query string, dest any) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // a close error changes nothing here
	if err := conn.QueryRowContext(t.Context(), query).Scan(dest); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func rawExec(t *testing.T, path, stmt string) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // a close error changes nothing here
	if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// seed stores one calendar with one event, so a recreate is visible.
func seed(t *testing.T, s *calcache.Store) {
	t.Helper()
	ctx := t.Context()
	if err := s.SetCalendars(ctx, "acme", []calendar.Calendar{{ID: "work", Access: calendar.AccessWrite}}, "work"); err != nil {
		t.Fatal(err)
	}
	ch := calendar.Changes{Upserts: []calendar.Event{single("p1", t0, t0.Add(time.Hour))}, Token: "t1", Full: true}
	if err := s.Apply(ctx, "acme", "work", ch, nil, t0); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	t.Parallel()
	s, path := openCache(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var version int
	rawQuery(t, path, "PRAGMA user_version", &version)
	if version != calcache.SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, calcache.SchemaVersion)
	}
	var journal string
	rawQuery(t, path, "PRAGMA journal_mode", &journal)
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	for _, table := range []string{"accounts", "calendars", "cal_events", "mail_invites"} {
		var n int
		rawQuery(t, path, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '"+table+"'", &n)
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestOpenKeepsData(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), calcache.FileName)
	s, err := calcache.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = calcache.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	if s.Recreated() != "" {
		t.Errorf("Recreated = %q after a plain reopen, want empty", s.Recreated())
	}
	cals, events, err := s.Stats(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if cals != 1 || events != 1 {
		t.Errorf("Stats after reopen = %d, %d; want 1, 1", cals, events)
	}
}

func TestOpenRejectsURIPath(t *testing.T) {
	t.Parallel()
	_, err := calcache.Open(t.Context(), filepath.Join(t.TempDir(), "a?b.db"))
	if err == nil {
		t.Fatal("Open accepted a path with '?'")
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
				s, err := calcache.Open(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				seed(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				rawExec(t, path, "PRAGMA user_version = 99")
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
				s, err := calcache.Open(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				seed(t, s)
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
				rawExec(t, path, "CREATE TABLE other (x INTEGER)")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), calcache.FileName)
			tt.spoil(t, path)
			s, err := calcache.Open(t.Context(), path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
			if got := s.Recreated(); got != tt.reason {
				t.Errorf("Recreated = %q, want %q", got, tt.reason)
			}
			cals, events, err := s.Stats(t.Context(), "acme")
			if err != nil {
				t.Fatal(err)
			}
			if cals != 0 || events != 0 {
				t.Errorf("Stats after recreate = %d, %d; want 0, 0", cals, events)
			}
			if _, err := os.Stat(path + ".corrupt"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("corruption marker after recreate: %v, want none", err)
			}
			seed(t, s)
		})
	}
}

// TestCorruptPageMarksAndRecreates damages a table page after Open, so a
// later read fails as corrupt, leaves the marker, and the next Open starts
// over.
func TestCorruptPageMarksAndRecreates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), calcache.FileName)
	s, err := calcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Fold the WAL into the file so the damage below is what SQLite reads.
	rawExec(t, path, "PRAGMA wal_checkpoint(TRUNCATE)")
	var pageSize int
	rawQuery(t, path, "PRAGMA page_size", &pageSize)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= pageSize {
		t.Fatalf("calendar.db has %d bytes, want more than one page", len(data))
	}
	// Page 1 keeps the header and the schema, so Open accepts the file;
	// every table and index page after it is junk.
	for i := pageSize; i < len(data); i++ {
		data[i] = 0xff
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err = calcache.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a damaged page: %v", err)
	}
	if _, _, err := s.Stats(ctx, "acme"); err == nil {
		t.Fatal("Stats on a damaged page succeeded")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("marker after a corrupt error: %v, want it created", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = calcache.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	if s.Recreated() != "corrupt" {
		t.Errorf("Recreated = %q, want corrupt", s.Recreated())
	}
	if _, _, err := s.Stats(ctx, "acme"); err != nil {
		t.Errorf("Stats after recreate: %v", err)
	}
}

// A calendar.db of schema version 1, whose mail_invites has no charset or
// announced column, is recreated, and the new columns work.
func TestOpenRecreatesSchemaVersion1(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), calcache.FileName)
	rawExec(t, path, `CREATE TABLE mail_invites (account TEXT NOT NULL, message_ref TEXT NOT NULL, uid TEXT NOT NULL,
		ref TEXT NOT NULL, method TEXT NOT NULL, sequence INTEGER NOT NULL, dtstamp TEXT NOT NULL, raw BLOB NOT NULL,
		found_at TEXT NOT NULL, PRIMARY KEY (account, message_ref, uid))`)
	rawExec(t, path, "PRAGMA user_version = 1")
	s, err := calcache.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) // a close error at test end changes nothing
	if why := s.Recreated(); why != "version" {
		t.Errorf("Recreated = %q, want version", why)
	}
	r := calcache.MailInviteRow{Account: "acme", MessageRef: "m1", UID: "u1", Method: "REQUEST", Charset: "iso-8859-1", Raw: []byte("x")}
	if err := s.PutMailInvite(t.Context(), r); err != nil {
		t.Fatalf("PutMailInvite: %v", err)
	}
}
