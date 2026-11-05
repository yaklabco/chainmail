package db

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
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
	if version != 4 {
		t.Errorf("user_version = %d, want 4", version)
	}
	for _, table := range []string{"audit", "idempotency", "account_limits", "sync_folders", "sync_uids", "events", "event_groups"} {
		var name string
		if err := conn.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name); err != nil {
			t.Errorf("%s table missing: %v", table, err)
		}
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

func TestMigrationGapAppliesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.db")
	gap := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")},
		"0003_c.sql": {Data: []byte("CREATE TABLE c (x INTEGER);")},
	}
	if _, err := Open(t.Context(), path, gap); err == nil {
		t.Fatal("Open accepted a numbering gap")
	}
	conn, err := Open(t.Context(), path, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	var version, tables int
	if err := conn.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != 0 || tables != 0 {
		t.Errorf("user_version = %d, tables = %d, want both 0", version, tables)
	}
}

func TestOpenRejectsURIMetacharacters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a?b.db", "a#b.db", "a%b.db"} {
		path := filepath.Join(dir, name)
		_, err := Open(t.Context(), path, fstest.MapFS{})
		if err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("Open(%q): err = %v, want a path error", path, err)
		}
	}
}

func TestNegativeSchemaVersionIsRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.db")
	empty := fstest.MapFS{}
	conn, err := Open(t.Context(), path, empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "PRAGMA user_version = -1"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	one := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")}}
	if _, err := Open(t.Context(), path, one); err == nil || !strings.Contains(err.Error(), "invalid schema version -1") {
		t.Errorf("err = %v, want an invalid-schema-version error", err)
	}
}

func TestComposeTablesCheckValues(t *testing.T) {
	t.Parallel()
	conn, err := OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	const idem = "INSERT INTO idempotency (account, key, request_hash, state, created_at, updated_at) VALUES ('a', ?, x'00', ?, 't', 't')"
	if _, err := conn.ExecContext(t.Context(), idem, "k1", "done"); err != nil {
		t.Errorf("valid idempotency row: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), idem, "k2", "sent"); err == nil {
		t.Error("idempotency state 'sent' was accepted")
	}
	if _, err := conn.ExecContext(t.Context(), idem, "k1", "done"); err == nil {
		t.Error("duplicate (account, key) was accepted")
	}
	const limit = "INSERT INTO account_limits (account, imap_cap, probed_at) VALUES (?, ?, 't')"
	if _, err := conn.ExecContext(t.Context(), limit, "a", 1); err != nil {
		t.Errorf("valid account_limits row: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), limit, "b", 0); err == nil {
		t.Error("imap_cap 0 was accepted")
	}
}

func TestWatchTablesCheckValues(t *testing.T) {
	t.Parallel()
	conn, err := OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	const event = "INSERT INTO events (at, account, kind) VALUES ('t', 'a', ?)"
	for _, kind := range []string{"new", "flags", "expunged", "status", "gap", "unreadable"} {
		if _, err := conn.ExecContext(t.Context(), event, kind); err != nil {
			t.Errorf("event kind %q: %v", kind, err)
		}
	}
	if _, err := conn.ExecContext(t.Context(), event, "moved"); err == nil {
		t.Error("event kind 'moved' was accepted")
	}
	const folder = "INSERT INTO sync_folders (account, folder, condstore, mode) VALUES ('a', ?, ?, ?)"
	for _, tt := range []struct {
		folder    string
		condstore int
		mode      string
		ok        bool
	}{
		{"f1", 0, "", true},
		{"f2", 1, "idle", true},
		{"f3", 0, "poll", true},
		{"f4", 2, "", false},
		{"f5", 0, "push", false},
	} {
		_, err := conn.ExecContext(t.Context(), folder, tt.folder, tt.condstore, tt.mode)
		if (err == nil) != tt.ok {
			t.Errorf("sync_folders condstore %d mode %q: err = %v, want ok %v", tt.condstore, tt.mode, err, tt.ok)
		}
	}
	const uid = "INSERT INTO sync_uids (account, folder, uid) VALUES ('a', 'f', ?)"
	if _, err := conn.ExecContext(t.Context(), uid, 7); err != nil {
		t.Errorf("valid sync_uids row: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), uid, 7); err == nil {
		t.Error("duplicate (account, folder, uid) was accepted")
	}
}

// Migration 0004 rebuilds the events table to admit unreadable events. It
// keeps every event with its ID, the log's tail even when every event was
// trimmed, the index on at, and the kind check.
func TestUnreadableEventsMigrationKeepsTheLog(t *testing.T) {
	t.Parallel()
	sub, err := fs.Sub(stateMigrations, "migrations/state")
	if err != nil {
		t.Fatal(err)
	}
	upTo3 := fstest.MapFS{}
	for _, name := range []string{"0001_audit.sql", "0002_compose.sql", "0003_watch.sql"} {
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatal(err)
		}
		upTo3[name] = &fstest.MapFile{Data: data}
	}
	for _, tt := range []struct {
		name    string
		trimmed bool // every event was deleted, leaving only the sequence
	}{
		{name: "events kept"},
		{name: "every event trimmed", trimmed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.db")
			old, err := Open(t.Context(), path, upTo3)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				"INSERT INTO events (id, at, account, folder, kind, ref, payload) VALUES (40, 't1', 'a', 'INBOX', 'new', 'r40', '{\"message\":{}}')",
				"INSERT INTO events (id, at, account, folder, kind, ref, payload) VALUES (41, 't2', 'a', 'INBOX', 'expunged', 'r41', '{}')",
				"UPDATE sqlite_sequence SET seq = 57 WHERE name = 'events'",
			} {
				if _, err := old.ExecContext(t.Context(), q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if tt.trimmed {
				if _, err := old.ExecContext(t.Context(), "DELETE FROM events"); err != nil {
					t.Fatal(err)
				}
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			conn, err := OpenState(t.Context(), path)
			if err != nil {
				t.Fatalf("OpenState: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
			var tail int64
			if err := conn.QueryRowContext(t.Context(), "SELECT seq FROM sqlite_sequence WHERE name = 'events'").Scan(&tail); err != nil || tail != 57 {
				t.Errorf("events sequence = %d, %v, want 57", tail, err)
			}
			var events []string
			rows, err := conn.QueryContext(t.Context(), "SELECT id, at, account, folder, kind, ref, payload FROM events ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var (
					id                                      int64
					at, account, folder, kind, ref, payload string
				)
				if err := rows.Scan(&id, &at, &account, &folder, &kind, &ref, &payload); err != nil {
					t.Fatal(err)
				}
				events = append(events, fmt.Sprintf("%d %s %s %s %s %s %s", id, at, account, folder, kind, ref, payload))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close() // read-only; rows.Err reported any failure
			want := []string{`40 t1 a INBOX new r40 {"message":{}}`, "41 t2 a INBOX expunged r41 {}"}
			if tt.trimmed {
				want = nil
			}
			if !slices.Equal(events, want) {
				t.Errorf("events = %q, want %q", events, want)
			}
			var id int64
			if err := conn.QueryRowContext(t.Context(),
				"INSERT INTO events (at, account, kind) VALUES ('t3', 'a', 'unreadable') RETURNING id").Scan(&id); err != nil || id != 58 {
				t.Errorf("unreadable event id = %d, %v, want 58", id, err)
			}
			if _, err := conn.ExecContext(t.Context(), "INSERT INTO events (at, account, kind) VALUES ('t', 'a', 'moved')"); err == nil {
				t.Error("event kind 'moved' was accepted")
			}
			var index string
			if err := conn.QueryRowContext(t.Context(),
				"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'events'").Scan(&index); err != nil || index != "events_at" {
				t.Errorf("events index = %q, %v, want events_at", index, err)
			}
		})
	}
}
