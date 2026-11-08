package db

import (
	"database/sql"
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
	if version != 6 {
		t.Errorf("user_version = %d, want 6", version)
	}
	for _, table := range []string{
		"audit", "idempotency", "account_limits", "sync_folders", "sync_uids", "events", "event_groups",
		"items", "item_revision", "item_criteria", "item_blockers", "item_links", "item_mail_seen", "item_requests",
		"rules", "rule_revisions", "rule_runs", "rule_applied", "item_due_sent",
	} {
		var name string
		if err := conn.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name); err != nil {
			t.Errorf("%s table missing: %v", table, err)
		}
	}
	for _, index := range []string{
		"events_at", "items_by_parent", "items_by_revision", "item_criteria_by_item",
		"item_blockers_by_blocker", "item_links_by_thread", "item_mail_seen_at", "item_requests_at",
		"rules_live_name", "rules_by_priority", "rules_by_next_run", "rule_runs_by_rule", "rule_runs_started",
		"rule_applied_by_key", "items_by_due",
	} {
		var name string
		if err := conn.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", index).Scan(&name); err != nil {
			t.Errorf("%s index missing: %v", index, err)
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
	for _, kind := range []string{
		"new", "flags", "expunged", "status", "gap", "unreadable", "item", "item_mail",
		"rule", "item_due_soon", "item_due", "item_overdue",
	} {
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

// stateMigrationsUpTo returns the first n state migrations as a MapFS, so a
// test can build a database as an older release left it.
func stateMigrationsUpTo(t *testing.T, n int) fstest.MapFS {
	t.Helper()
	sub, err := fs.Sub(stateMigrations, "migrations/state")
	if err != nil {
		t.Fatal(err)
	}
	names, err := migrationNames(sub)
	if err != nil {
		t.Fatal(err)
	}
	if n > len(names) {
		t.Fatalf("want %d migrations, have %d", n, len(names))
	}
	out := fstest.MapFS{}
	for _, name := range names[:n] {
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// storedEvents returns every events row in ID order, one string per row.
// It closes its Rows before returning.
func storedEvents(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), "SELECT id, at, account, folder, kind, ref, payload FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any failure
	var out []string
	for rows.Next() {
		var (
			id                                      int64
			at, account, folder, kind, ref, payload string
		)
		if err := rows.Scan(&id, &at, &account, &folder, &kind, &ref, &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s %s %s %s %s %s", id, at, account, folder, kind, ref, payload))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Migration 0005 adds the item tables and rebuilds the events table to admit
// item and item_mail events. An event of every older kind keeps its ID and
// payload, the log's tail survives even when every event was trimmed, the
// index on at is back, and the kind check still refuses unknown kinds.
func TestItemsMigrationKeepsTheLog(t *testing.T) {
	t.Parallel()
	upTo4 := stateMigrationsUpTo(t, 4)
	// One event of each kind the log held before 0005, as the engine and the
	// cache store them.
	old := []struct {
		id                                      int64
		at, account, folder, kind, ref, payload string
	}{
		{40, "t40", "a", "INBOX", "new", "cm:a:INBOX:1:40", `{"message":{"subject":"hi"}}`},
		{41, "t41", "a", "INBOX", "flags", "cm:a:INBOX:1:40", `{"flags":["\\Seen"],"labels":[]}`},
		{42, "t42", "a", "INBOX", "expunged", "cm:a:INBOX:1:39", `{}`},
		{43, "t43", "a", "INBOX", "status", "", `{"state":"watching","detail":"idle"}`},
		{44, "t44", "a", "Archive", "gap", "", `{"reason":"uidvalidity","next_id":0}`},
		{45, "t45", "a", "INBOX", "unreadable", "cm:a:INBOX:1:41", `{}`},
	}
	var kept []string
	for _, e := range old {
		kept = append(kept, fmt.Sprintf("%d %s %s %s %s %s %s", e.id, e.at, e.account, e.folder, e.kind, e.ref, e.payload))
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
			before, err := Open(t.Context(), path, upTo4)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range old {
				if _, err := before.ExecContext(t.Context(),
					"INSERT INTO events (id, at, account, folder, kind, ref, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
					e.id, e.at, e.account, e.folder, e.kind, e.ref, e.payload); err != nil {
					t.Fatalf("event %d: %v", e.id, err)
				}
			}
			if _, err := before.ExecContext(t.Context(), "UPDATE sqlite_sequence SET seq = 57 WHERE name = 'events'"); err != nil {
				t.Fatal(err)
			}
			if tt.trimmed {
				if _, err := before.ExecContext(t.Context(), "DELETE FROM events"); err != nil {
					t.Fatal(err)
				}
			}
			if err := before.Close(); err != nil {
				t.Fatal(err)
			}

			conn, err := OpenState(t.Context(), path)
			if err != nil {
				t.Fatalf("OpenState: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
			var version int
			if err := conn.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 6 {
				t.Errorf("user_version = %d, %v, want 6", version, err)
			}
			var tail int64
			if err := conn.QueryRowContext(t.Context(), "SELECT seq FROM sqlite_sequence WHERE name = 'events'").Scan(&tail); err != nil || tail != 57 {
				t.Errorf("events sequence = %d, %v, want 57", tail, err)
			}
			want := kept
			if tt.trimmed {
				want = nil
			}
			if got := storedEvents(t, conn); !slices.Equal(got, want) {
				t.Errorf("events =\n%q\nwant\n%q", got, want)
			}
			for i, kind := range []string{"item", "item_mail"} {
				var id int64
				if err := conn.QueryRowContext(t.Context(),
					"INSERT INTO events (at, account, kind) VALUES ('t5', '', ?) RETURNING id", kind).Scan(&id); err != nil || id != int64(58+i) {
					t.Errorf("%s event id = %d, %v, want %d", kind, id, err, 58+i)
				}
			}
			for _, kind := range []string{"moved", "Item", "items", "item-mail", ""} {
				if _, err := conn.ExecContext(t.Context(), "INSERT INTO events (at, account, kind) VALUES ('t', 'a', ?)", kind); err == nil {
					t.Errorf("event kind %q was accepted", kind)
				}
			}
			var index string
			if err := conn.QueryRowContext(t.Context(),
				"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'events'").Scan(&index); err != nil || index != "events_at" {
				t.Errorf("events index = %q, %v, want events_at", index, err)
			}
			var items int
			if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM items").Scan(&items); err != nil || items != 0 {
				t.Errorf("items = %d, %v, want an empty table", items, err)
			}
		})
	}
}

// The item tables enforce their checks and foreign keys: the nine types,
// four states, two areas and four priorities, a non-negative estimate, a
// parent that exists, the single revision row, no self blocker, and the
// primary keys of links, seen rows and requests.
func TestItemTablesCheckValues(t *testing.T) {
	t.Parallel()
	conn, err := OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	ctx := t.Context()

	var revision int64
	if err := conn.QueryRowContext(ctx, "SELECT value FROM item_revision WHERE id = 1").Scan(&revision); err != nil || revision != 0 {
		t.Errorf("item_revision = %d, %v, want 0", revision, err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO item_revision (id, value) VALUES (2, 0)"); err == nil {
		t.Error("a second item_revision row was accepted")
	}

	const item = "INSERT INTO items (id, type, title, state, area, priority, estimate, parent_id, revision, created_at, updated_at, created_by, updated_by) " +
		"VALUES (?, ?, 'title', ?, ?, ?, ?, ?, 1, 't', 't', '1000/cli', '1000/cli')"
	for i, typ := range []string{"objective", "key-result", "initiative", "epic", "feature", "story", "bug", "support-ticket", "task"} {
		if _, err := conn.ExecContext(ctx, item, fmt.Sprintf("it_type%d", i), typ, "backlog", "work", nil, nil, nil); err != nil {
			t.Errorf("item type %q: %v", typ, err)
		}
	}
	for i, state := range []string{"backlog", "in-progress", "in-review", "done"} {
		if _, err := conn.ExecContext(ctx, item, fmt.Sprintf("it_state%d", i), "task", state, nil, nil, nil, nil); err != nil {
			t.Errorf("item state %q: %v", state, err)
		}
	}
	// The rows run in order: it_child needs it_root.
	for _, tt := range []struct {
		name, id, typ, state             string
		area, priority, estimate, parent any
		ok                               bool
	}{
		{"root with area", "it_root", "initiative", "backlog", "work", nil, nil, nil, true},
		{"child with priority, estimate and parent", "it_child", "task", "in-progress", nil, "p1", 1.5, "it_root", true},
		{"personal area and zero estimate", "it_home", "epic", "done", "personal", "p3", 0.0, nil, true},
		{"unknown type", "it_bad1", "milestone", "backlog", "work", nil, nil, nil, false},
		{"unknown state", "it_bad2", "task", "blocked", nil, nil, nil, nil, false},
		{"unknown area", "it_bad3", "epic", "backlog", "home", nil, nil, nil, false},
		{"unknown priority", "it_bad4", "task", "backlog", nil, "p4", nil, nil, false},
		{"negative estimate", "it_bad5", "task", "backlog", nil, nil, -1.0, nil, false},
		{"unknown parent", "it_bad6", "task", "backlog", nil, nil, nil, "it_missing", false},
		{"duplicate id", "it_root", "epic", "backlog", "work", nil, nil, nil, false},
	} {
		_, err := conn.ExecContext(ctx, item, tt.id, tt.typ, tt.state, tt.area, tt.priority, tt.estimate, tt.parent)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok %v", tt.name, err, tt.ok)
		}
	}

	if _, err := conn.ExecContext(ctx,
		"INSERT INTO items (id, type, title, revision, created_at, updated_at, created_by, updated_by) VALUES ('it_defaults', 'task', 'title', 1, 't', 't', 'u', 'u')"); err != nil {
		t.Fatalf("item with defaults: %v", err)
	}
	var (
		description, state                       string
		ongoing                                  int
		noArea, noPriority, noParent, notDeleted bool
	)
	if err := conn.QueryRowContext(ctx,
		"SELECT description, state, ongoing, area IS NULL, priority IS NULL, parent_id IS NULL, deleted_at IS NULL FROM items WHERE id = 'it_defaults'").
		Scan(&description, &state, &ongoing, &noArea, &noPriority, &noParent, &notDeleted); err != nil {
		t.Fatal(err)
	}
	if description != "" || state != "backlog" || ongoing != 0 || !noArea || !noPriority || !noParent || !notDeleted {
		t.Errorf("defaults = %q %q %d area null %v priority null %v parent null %v live %v, want '' backlog 0 and all true",
			description, state, ongoing, noArea, noPriority, noParent, notDeleted)
	}

	// The rows run in order: duplicates follow the row they repeat.
	for _, tt := range []struct {
		name, query string
		ok          bool
	}{
		{"criterion", "INSERT INTO item_criteria (id, item_id, position, text) VALUES ('cr_1', 'it_root', 0, 'c')", true},
		{"criterion of an unknown item", "INSERT INTO item_criteria (id, item_id, position, text) VALUES ('cr_2', 'it_missing', 0, 'c')", false},
		{"duplicate criterion id", "INSERT INTO item_criteria (id, item_id, position, text) VALUES ('cr_1', 'it_child', 0, 'c')", false},
		{"blocker", "INSERT INTO item_blockers (item_id, blocked_by_id) VALUES ('it_child', 'it_home')", true},
		{"duplicate blocker", "INSERT INTO item_blockers (item_id, blocked_by_id) VALUES ('it_child', 'it_home')", false},
		{"self blocker", "INSERT INTO item_blockers (item_id, blocked_by_id) VALUES ('it_child', 'it_child')", false},
		{"blocked by an unknown item", "INSERT INTO item_blockers (item_id, blocked_by_id) VALUES ('it_child', 'it_missing')", false},
		{"link", "INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES ('it_child', 'a', 'k1', 't', 'u')", true},
		{"same thread on another item", "INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES ('it_root', 'a', 'k1', 't', 'u')", true},
		{"same key on another account", "INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES ('it_child', 'b', 'k1', 't', 'u')", true},
		{"duplicate link", "INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES ('it_child', 'a', 'k1', 't', 'u')", false},
		{"link to an unknown item", "INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES ('it_missing', 'a', 'k2', 't', 'u')", false},
		{"seen", "INSERT INTO item_mail_seen (item_id, account, message_key, at) VALUES ('it_child', 'a', 'm1', 't')", true},
		{"duplicate seen", "INSERT INTO item_mail_seen (item_id, account, message_key, at) VALUES ('it_child', 'a', 'm1', 't')", false},
		{"seen needs no item row", "INSERT INTO item_mail_seen (item_id, account, message_key, at) VALUES ('it_gone', 'a', 'm1', 't')", true},
		{"request", "INSERT INTO item_requests (key, request_hash, item_id, at) VALUES ('k1', 'h', 'it_child', 't')", true},
		{"duplicate request key", "INSERT INTO item_requests (key, request_hash, item_id, at) VALUES ('k1', 'h2', 'it_root', 't')", false},
		{"request for an unknown item", "INSERT INTO item_requests (key, request_hash, item_id, at) VALUES ('k2', 'h', 'it_missing', 't')", false},
	} {
		_, err := conn.ExecContext(ctx, tt.query)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}
