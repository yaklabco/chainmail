package daemon_test

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// itemStrings runs query, which selects one text column, on the harness's
// state database and returns the values in order.
func itemStrings(t *testing.T, h *daemontest.Harness, query string) []string {
	t.Helper()
	rows, err := h.DB.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any iteration error
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

// TestAdminItemsAreCreatedOnce checks that a new daemon has the two Admin
// items before Start returns, that creating them writes no event and no
// audit row (they are bootstrap rows, found by revision), and that a
// restart on the same state adds nothing.
func TestAdminItemsAreCreatedOnce(t *testing.T) {
	t.Parallel()
	const (
		itemsQuery  = "SELECT id || ' ' || type || ' ' || title || ' ' || COALESCE(area, '') || ' ' || ongoing || ' ' || created_by FROM items ORDER BY id"
		eventsQuery = "SELECT payload FROM events WHERE kind = 'item' ORDER BY id"
	)
	by := fmt.Sprintf("%d/daemon", os.Getuid())
	want := []string{
		"it_admin_personal initiative Admin personal 1 " + by,
		"it_admin_work initiative Admin work 1 " + by,
	}
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	if got := itemStrings(t, h, itemsQuery); !slices.Equal(got, want) {
		t.Fatalf("items = %q, want %q", got, want)
	}
	if payloads := itemStrings(t, h, eventsQuery); len(payloads) != 0 {
		t.Fatalf("item events = %q, want none: the event log starts empty", payloads)
	}
	again := daemontest.Start(t, hostingerConfig, daemontest.Options{Reuse: h})
	if got := itemStrings(t, again, itemsQuery); !slices.Equal(got, want) {
		t.Errorf("after a restart items = %q, want %q", got, want)
	}
	if n := len(itemStrings(t, again, eventsQuery)); n != 0 {
		t.Errorf("after a restart %d item events, want none", n)
	}
	if rows := again.AuditRows(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none: startup is not a call", rows)
	}
}
