package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// queryStrings runs query, which selects one text column, and returns the
// values in order. It closes its Rows before returning.
func queryStrings(t *testing.T, conn *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any failure
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Migration 0006 adds the rule tables and rebuilds the events table to
// admit the rule and due kinds. An event of every older kind keeps its ID
// and payload, the log's tail survives even when every event was trimmed,
// events_at is the only index on events, the kind check admits every kind
// and refuses junk, a client group named like a rules group is dropped
// while every other group keeps its position, and items keep their rows
// under the new due-date index.
func TestRulesMigrationKeepsTheLog(t *testing.T) {
	t.Parallel()
	upTo5 := stateMigrationsUpTo(t, 5)
	// One event of each kind the log held before 0006, as the engine, the
	// cache and the item store write them.
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
		{46, "t46", "", "", "item", "", `{"item":{"item_id":"it_01","type":"task","revision":1,"op":"created"}}`},
		{47, "t47", "a", "INBOX", "item_mail", "cm:a:INBOX:1:40", `{"item_id":"it_01","from_self":false,"message":{"subject":"hi"}}`},
	}
	var kept []string
	for _, e := range old {
		kept = append(kept, fmt.Sprintf("%d %s %s %s %s %s %s", e.id, e.at, e.account, e.folder, e.kind, e.ref, e.payload))
	}
	// Groups as clients and the cache left them. Only names matching
	// rules.% go: "rules" and "rulesx.work" are ordinary client groups.
	groups := []struct {
		name  string
		acked int64
	}{
		{"bot", 44}, {"cache.work", 47}, {"rules.work", 45}, {"rules.personal", 0}, {"rules", 41}, {"rulesx.work", 42},
	}
	wantGroups := []string{"bot 44", "cache.work 47", "rules 41", "rulesx.work 42"}

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
			before, err := Open(t.Context(), path, upTo5)
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
			for _, g := range groups {
				if _, err := before.ExecContext(t.Context(),
					"INSERT INTO event_groups (name, acked_id, created_at, last_seen_at) VALUES (?, ?, 't', 't')", g.name, g.acked); err != nil {
					t.Fatalf("group %s: %v", g.name, err)
				}
			}
			if _, err := before.ExecContext(t.Context(),
				"INSERT INTO items (id, type, title, due_date, revision, created_at, updated_at, created_by, updated_by) "+
					"VALUES ('it_01', 'task', 'title', '2026-11-09', 1, 't', 't', 'u', 'u')"); err != nil {
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
			if got := queryStrings(t, conn, "SELECT name || ' ' || acked_id FROM event_groups ORDER BY name"); !slices.Equal(got, wantGroups) {
				t.Errorf("groups = %q, want %q", got, wantGroups)
			}
			// The new kinds first, then every older kind: all accepted, and
			// the IDs continue from the carried-over sequence.
			for i, kind := range []string{
				"rule", "item_due_soon", "item_due", "item_overdue",
				"new", "flags", "expunged", "status", "gap", "unreadable", "item", "item_mail",
			} {
				var id int64
				if err := conn.QueryRowContext(t.Context(),
					"INSERT INTO events (at, account, kind) VALUES ('t6', '', ?) RETURNING id", kind).Scan(&id); err != nil || id != int64(58+i) {
					t.Errorf("%s event id = %d, %v, want %d", kind, id, err, 58+i)
				}
			}
			for _, kind := range []string{"rules", "Rule", "item_due_late", "item-due", "due", "overdue", ""} {
				if _, err := conn.ExecContext(t.Context(), "INSERT INTO events (at, account, kind) VALUES ('t', 'a', ?)", kind); err == nil {
					t.Errorf("event kind %q was accepted", kind)
				}
			}
			if got := queryStrings(t, conn,
				"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'events' ORDER BY name"); !slices.Equal(got, []string{"events_at"}) {
				t.Errorf("events indexes = %q, want only events_at", got)
			}
			if got := queryStrings(t, conn, "SELECT id || ' ' || due_date FROM items"); !slices.Equal(got, []string{"it_01 2026-11-09"}) {
				t.Errorf("items = %q, want the one item kept", got)
			}
			for _, name := range []string{
				"rules", "rule_revisions", "rule_runs", "rule_applied", "item_due_sent",
				"rules_live_name", "rules_by_priority", "rules_by_next_run", "rule_runs_by_rule", "rule_runs_started",
				"rule_applied_by_key", "items_by_due",
			} {
				if got := queryStrings(t, conn, "SELECT name FROM sqlite_master WHERE name = ?", name); len(got) != 1 {
					t.Errorf("%s missing after the migration", name)
				}
			}
		})
	}
}

// The rule tables enforce their checks and keys: max_per_run 1 to 5000, one
// live rule per name, revisions only of a rule that exists and only with a
// known change, run triggers and outcomes, applied outcomes and the
// act-once key, and the due stages with one row per item, due date and
// stage. Columns left out take the defaults the rules store relies on.
func TestRuleTablesCheckValues(t *testing.T) {
	t.Parallel()
	conn, err := OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	ctx := t.Context()

	if _, err := conn.ExecContext(ctx,
		"INSERT INTO rules (id, name, query, actions, revision, created_at, updated_at, created_by, updated_by) "+
			"VALUES ('ru_defaults', 'defaults', 'from:a', '[]', 1, 't', 't', 'u', 'u')"); err != nil {
		t.Fatalf("rule with defaults: %v", err)
	}
	var (
		description, accounts, schedule, disabledReason       string
		onNew, priority, stop, maxPerRun, enabled, failedRuns int
		live, unscheduled, neverRun                           bool
	)
	if err := conn.QueryRowContext(ctx,
		"SELECT description, accounts, on_new, schedule, priority, stop, max_per_run, enabled, disabled_reason, failed_runs, "+
			"deleted_at IS NULL, next_run_at IS NULL, last_run_at IS NULL FROM rules WHERE id = 'ru_defaults'").
		Scan(&description, &accounts, &onNew, &schedule, &priority, &stop, &maxPerRun, &enabled, &disabledReason, &failedRuns,
			&live, &unscheduled, &neverRun); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%q %q %d %q %d %d %d %d %q %d %v %v %v",
		description, accounts, onNew, schedule, priority, stop, maxPerRun, enabled, disabledReason, failedRuns, live, unscheduled, neverRun)
	if want := `"" "[]" 0 "" 100 0 500 1 "" 0 true true true`; got != want {
		t.Errorf("rule defaults = %s, want %s", got, want)
	}

	const (
		rule = "INSERT INTO rules (id, name, query, actions, max_per_run, revision, created_at, updated_at, deleted_at, created_by, updated_by) " +
			"VALUES (?, ?, 'q', '[]', ?, 1, 't', 't', ?, 'u', 'u')"
		revision = "INSERT INTO rule_revisions (rule_id, revision, change, rule, actor, at) VALUES (?, ?, ?, '{}', 'u', 't')"
		run      = "INSERT INTO rule_runs (rule_id, account, trigger_kind, started_at, finished_at, outcome) VALUES ('ru_one', 'a', ?, 't', 't', ?)"
		applied  = "INSERT INTO rule_applied (rule_id, account, message_key, run_id, outcome, at) VALUES (?, 'a', ?, 1, ?, 't')"
		due      = "INSERT INTO item_due_sent (item_id, due_date, stage, at) VALUES (?, ?, ?, 't')"
	)
	type row struct {
		name, query string
		args        []any
		ok          bool
	}
	// The rows run in order: later rows depend on earlier ones.
	rows := []row{
		{"lowest max_per_run", rule, []any{"ru_one", "one", 1, nil}, true},
		{"highest max_per_run", rule, []any{"ru_two", "two", 5000, nil}, true},
		{"zero max_per_run", rule, []any{"ru_three", "three", 0, nil}, false},
		{"max_per_run above 5000", rule, []any{"ru_four", "four", 5001, nil}, false},
		{"second live rule with a name", rule, []any{"ru_five", "one", 500, nil}, false},
		{"deleted rule with a live name", rule, []any{"ru_six", "one", 500, "t"}, true},
		{"two deleted rules with one name", rule, []any{"ru_seven", "one", 500, "t"}, true},
		{"duplicate rule id", rule, []any{"ru_one", "five", 500, nil}, false},
	}
	for i, change := range []string{"created", "updated", "deleted", "restored", "disabled", "enabled"} {
		rows = append(rows, row{"revision change " + change, revision, []any{"ru_one", i + 1, change}, true})
	}
	rows = append(rows,
		row{"unknown revision change", revision, []any{"ru_one", 7, "renamed"}, false},
		row{"duplicate revision", revision, []any{"ru_one", 1, "updated"}, false},
		row{"revision of an unknown rule", revision, []any{"ru_missing", 1, "created"}, false},
	)
	for _, trigger := range []string{"new", "schedule", "manual"} {
		rows = append(rows, row{"run trigger " + trigger, run, []any{trigger, "ok"}, true})
	}
	for _, outcome := range []string{"ok", "capped", "failed", "cache_not_ready", "interrupted"} {
		rows = append(rows, row{"run outcome " + outcome, run, []any{"manual", outcome}, true})
	}
	rows = append(rows,
		row{"unknown run trigger", run, []any{"hourly", "ok"}, false},
		row{"unknown run outcome", run, []any{"new", "partial"}, false},
	)
	for i, outcome := range []string{"pending", "applied", "held"} {
		rows = append(rows, row{"applied outcome " + outcome, applied, []any{"ru_one", fmt.Sprintf("k%d", i), outcome}, true})
	}
	rows = append(rows,
		row{"unknown applied outcome", applied, []any{"ru_one", "k9", "skipped"}, false},
		row{"duplicate applied key", applied, []any{"ru_one", "k0", "applied"}, false},
		row{"same key for another rule", applied, []any{"ru_two", "k0", "applied"}, true},
		row{"applied needs no rule row", applied, []any{"ru_gone", "k0", "applied"}, true},
	)
	for _, stage := range []string{"item_due_soon", "item_due", "item_overdue"} {
		rows = append(rows, row{"due stage " + stage, due, []any{"it_a", "2026-11-09", stage}, true})
	}
	rows = append(rows,
		row{"unknown due stage", due, []any{"it_a", "2026-11-09", "item_due_late"}, false},
		row{"duplicate due stage", due, []any{"it_a", "2026-11-09", "item_due"}, false},
		row{"same stage for a moved due date", due, []any{"it_a", "2026-11-10", "item_due"}, true},
		row{"due row needs no item row", due, []any{"it_gone", "2026-11-09", "item_due"}, true},
	)
	for _, tt := range rows {
		_, err := conn.ExecContext(ctx, tt.query, tt.args...)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok %v", tt.name, err, tt.ok)
		}
	}

	var counts, reason, hint string
	if err := conn.QueryRowContext(ctx,
		"SELECT matched || ' ' || applied || ' ' || held || ' ' || skipped || ' ' || failed, reason, hint FROM rule_runs WHERE id = 1").
		Scan(&counts, &reason, &hint); err != nil {
		t.Fatal(err)
	}
	if counts != "0 0 0 0 0" || reason != "" || hint != "" {
		t.Errorf("run defaults = %q %q %q, want zero counts and empty reason and hint", counts, reason, hint)
	}
	if got := queryStrings(t, conn, "SELECT CAST(id AS TEXT) FROM rule_runs ORDER BY id"); !slices.Equal(got, []string{"1", "2", "3", "4", "5", "6", "7", "8"}) {
		t.Errorf("run ids = %q, want 1 to 8", got)
	}
	if got := queryStrings(t, conn, "SELECT note FROM rule_applied WHERE rule_id = 'ru_one' AND message_key = 'k0'"); !slices.Equal(got, []string{""}) {
		t.Errorf("applied note = %q, want an empty default", got)
	}
}
