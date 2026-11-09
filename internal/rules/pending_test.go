package rules_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

// openPendingStore is a rule store over a fresh state.db.
func openPendingStore(t *testing.T) (*sql.DB, *rules.Store) {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	now := func() time.Time { return time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC) }
	return conn, rules.NewStore(rules.StoreOptions{DB: conn, Log: watch.NewLog(conn, now, nil), Now: now})
}

// TestClearPendingDropsOnlyPendingRows checks that ClearPending removes a
// pending row and leaves applied and held rows, and a key with no row, as
// they are.
func TestClearPendingDropsOnlyPendingRows(t *testing.T) {
	t.Parallel()
	conn, st := openPendingStore(t)
	ctx := t.Context()
	const ruleID, account = "ru_cleartest", "host"
	pending, applied, held, none := "i:1:5:INBOX", "i:1:6:INBOX", "i:1:7:INBOX", "i:1:8:INBOX"
	for _, k := range []string{pending, applied, held} {
		if err := st.MarkPending(ctx, ruleID, account, k, 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkApplied(ctx, ruleID, account, applied, 3, rules.AppliedDone, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkApplied(ctx, ruleID, account, held, 3, rules.AppliedHeld, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{pending, applied, held, none} {
		if err := rules.ClearPending(ctx, conn, ruleID, account, k); err != nil {
			t.Fatalf("ClearPending(%s): %v", k, err)
		}
	}
	got, err := st.Applied(ctx, ruleID, account, []string{pending, applied, held, none})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[applied].Outcome != rules.AppliedDone || got[held].Outcome != rules.AppliedHeld {
		t.Errorf("rows = %+v, want only the applied and held rows", got)
	}
}
