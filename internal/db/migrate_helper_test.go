package db

import (
	"database/sql"
	"slices"
	"testing"
)

// reopenMigrated opens the state database at path with every migration and
// checks that the event log came through the latest one: user_version 7, the
// events sequence at 57, the kept events (none when trimmed) and the group
// positions. It returns the open database for the caller's own checks.
func reopenMigrated(t *testing.T, path string, trimmed bool, kept, groups []string) *sql.DB {
	t.Helper()
	conn, err := OpenState(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	var version int
	if err := conn.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 7 {
		t.Errorf("user_version = %d, %v, want 7", version, err)
	}
	var tail int64
	if err := conn.QueryRowContext(t.Context(), "SELECT seq FROM sqlite_sequence WHERE name = 'events'").Scan(&tail); err != nil || tail != 57 {
		t.Errorf("events sequence = %d, %v, want 57", tail, err)
	}
	want := kept
	if trimmed {
		want = nil
	}
	if got := storedEvents(t, conn); !slices.Equal(got, want) {
		t.Errorf("events =\n%q\nwant\n%q", got, want)
	}
	if got := queryStrings(t, conn, "SELECT name || ' ' || acked_id FROM event_groups ORDER BY name"); !slices.Equal(got, groups) {
		t.Errorf("groups = %q, want %q", got, groups)
	}
	return conn
}
