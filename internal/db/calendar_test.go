package db

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// Migration 0007 rebuilds the events table to admit the four calendar
// kinds, and changes nothing else. An event of every older kind keeps its
// ID and payload, the log's tail survives even when every event was
// trimmed, every group keeps its position, events_at is the only index on
// events, and the kind check admits every kind and refuses junk.
func TestCalendarMigrationKeepsTheLog(t *testing.T) {
	t.Parallel()
	upTo6 := stateMigrationsUpTo(t, 6)
	// One event of each kind the log held before 0007, as the engine, the
	// cache, the item store, the rules store and the due checker write them.
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
		{48, "t48", "", "", "rule", "", `{"rule":{"rule_id":"ru_01","name":"receipts","change":"created","revision":1,"actor":"1000/cli"}}`},
		{49, "t49", "", "", "item_due_soon", "", `{"due":{"item_id":"it_01","type":"task","due_date":"2026-11-09","state":"backlog"}}`},
		{50, "t50", "", "", "item_due", "", `{"due":{"item_id":"it_01","type":"task","due_date":"2026-11-09","state":"backlog"}}`},
		{51, "t51", "", "", "item_overdue", "", `{"due":{"item_id":"it_01","type":"task","due_date":"2026-11-09","state":"backlog"}}`},
	}
	var kept []string
	for _, e := range old {
		kept = append(kept, fmt.Sprintf("%d %s %s %s %s %s %s", e.id, e.at, e.account, e.folder, e.kind, e.ref, e.payload))
	}
	// Groups as clients, the cache and the rule runners left them. 0007
	// drops none.
	groups := []string{"bot 44", "cache.work 47", "rules.work 51"}

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
			before, err := Open(t.Context(), path, upTo6)
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
			for _, g := range []struct {
				name  string
				acked int64
			}{{"bot", 44}, {"cache.work", 47}, {"rules.work", 51}} {
				if _, err := before.ExecContext(t.Context(),
					"INSERT INTO event_groups (name, acked_id, created_at, last_seen_at) VALUES (?, ?, 't', 't')", g.name, g.acked); err != nil {
					t.Fatalf("group %s: %v", g.name, err)
				}
			}
			if tt.trimmed {
				if _, err := before.ExecContext(t.Context(), "DELETE FROM events"); err != nil {
					t.Fatal(err)
				}
			}
			if err := before.Close(); err != nil {
				t.Fatal(err)
			}

			conn := reopenMigrated(t, path, tt.trimmed, kept, groups)
			// The new kinds first, then every older kind: all accepted, and
			// the IDs continue from the carried-over sequence.
			for i, kind := range []string{
				"invite", "event_changed", "event_cancelled", "invite_reply",
				"new", "flags", "expunged", "status", "gap", "unreadable", "item", "item_mail",
				"rule", "item_due_soon", "item_due", "item_overdue",
			} {
				var id int64
				if err := conn.QueryRowContext(t.Context(),
					"INSERT INTO events (at, account, kind) VALUES ('t7', 'a', ?) RETURNING id", kind).Scan(&id); err != nil || id != int64(58+i) {
					t.Errorf("%s event id = %d, %v, want %d", kind, id, err, 58+i)
				}
			}
			for _, kind := range []string{"invites", "Invite", "event-changed", "event_canceled", "event", "calendar", "reply", ""} {
				if _, err := conn.ExecContext(t.Context(), "INSERT INTO events (at, account, kind) VALUES ('t', 'a', ?)", kind); err == nil {
					t.Errorf("event kind %q was accepted", kind)
				}
			}
			if got := queryStrings(t, conn,
				"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'events' ORDER BY name"); !slices.Equal(got, []string{"events_at"}) {
				t.Errorf("events indexes = %q, want only events_at", got)
			}
		})
	}
}

// The calendar kinds go into a fresh log with an account, a ce_ ref and a
// payload, as the calendar syncer writes them; an unknown kind fails.
func TestCalendarEventKindsCheck(t *testing.T) {
	t.Parallel()
	conn, err := OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	const event = "INSERT INTO events (at, account, kind, ref, payload) VALUES ('t', 'a', ?, 'ce_0123456789abcdefghjkmnpqrs', ?)"
	for _, tt := range []struct {
		kind, payload string
		ok            bool
	}{
		{"invite", `{"calendar":{"ref":"ce_0123456789abcdefghjkmnpqrs"}}`, true},
		{"event_changed", `{"calendar":{"ref":"ce_0123456789abcdefghjkmnpqrs","fields":["start"]}}`, true},
		{"event_cancelled", `{"calendar":{"ref":"ce_0123456789abcdefghjkmnpqrs"}}`, true},
		{"invite_reply", `{"calendar":{"ref":"ce_0123456789abcdefghjkmnpqrs","partstat":"ACCEPTED"}}`, true},
		{"calendar", `{}`, false},
		{"invite-reply", `{}`, false},
		{"event_cancel", `{}`, false},
	} {
		_, err := conn.ExecContext(t.Context(), event, tt.kind, tt.payload)
		if (err == nil) != tt.ok {
			t.Errorf("event kind %q: err = %v, want ok %v", tt.kind, err, tt.ok)
		}
	}
}
