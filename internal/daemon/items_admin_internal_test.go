package daemon

import (
	"strings"
	"testing"
)

// TestServeCreatesTheAdminItems checks that New writes no Admin items, so
// Serve creates them with its caller's context, and that Serve reports a
// failure to create them instead of serving without them.
func TestServeCreatesTheAdminItems(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	var n int
	if err := stateDB.QueryRowContext(t.Context(), "SELECT count(*) FROM items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("items after New = %d, want 0: Serve creates the Admin items", n)
	}
	// A closed state database fails the first store call Serve makes.
	if err := stateDB.Close(); err != nil {
		t.Fatal(err)
	}
	err := s.Serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "creating the Admin items") {
		t.Errorf("Serve = %v, want the Admin items error", err)
	}
}
