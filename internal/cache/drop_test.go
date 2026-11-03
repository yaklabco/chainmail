package cache

import (
	"testing"
	"time"
)

func TestDeleteRowCascades(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putRows(t, s, testRow("INBOX", 1), testRow("INBOX", 2))
	putBody(t, s, testRef("INBOX", 1), rawMessage("Report 1", "zebra"), false)
	putBody(t, s, testRef("INBOX", 2), rawMessage("Report 2", "zebra"), false)
	if _, err := s.RecordBodyFailure(t.Context(), testRef("INBOX", 1), "fetch_failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRow(t.Context(), testRef("INBOX", 1)); err != nil {
		t.Fatalf("DeleteRow: %v", err)
	}
	for table, want := range map[string]int{"messages": 1, "bodies": 1, "body_failures": 0} {
		if n := countRows(t, s, "SELECT count(*) FROM "+table); n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
	}
	wantUIDs(t, "match body term", match(t, s, "INBOX", `"zebra"`), 2)
	wantUIDs(t, "match header term", match(t, s, "INBOX", `"report"`), 2)
	if err := s.DeleteRow(t.Context(), testRef("INBOX", 1)); err != nil {
		t.Errorf("DeleteRow of a missing row: %v", err)
	}
}

func TestDropFolder(t *testing.T) {
	t.Parallel()
	for _, keep := range []bool{false, true} {
		s, clock := openStore(t)
		noID := testRow("INBOX", 3)
		noID.MessageID = ""
		putRows(t, s, testRow("INBOX", 1), testRow("INBOX", 2), noID, testRow("Sent", 4))
		putBody(t, s, testRef("INBOX", 1), rawMessage("Report 1", "zebra"), false)
		putBody(t, s, testRef("Sent", 4), rawMessage("Report 4", "zebra"), false)
		for _, f := range []string{"INBOX", "Sent"} {
			if err := s.PutProgress(t.Context(), Progress{Account: testAccount, Folder: f, UIDValidity: testUIDValidity}); err != nil {
				t.Fatal(err)
			}
		}
		clock.Set(t0.Add(time.Minute))
		if err := s.DropFolder(t.Context(), testAccount, "INBOX", keep); err != nil {
			t.Fatalf("DropFolder keep=%v: %v", keep, err)
		}
		if n := countRows(t, s, "SELECT count(*) FROM messages WHERE folder = 'INBOX'"); n != 0 {
			t.Errorf("keep=%v: %d INBOX rows left", keep, n)
		}
		if n := countRows(t, s, "SELECT count(*) FROM bodies"); n != 1 {
			t.Errorf("keep=%v: %d bodies, want only Sent's", keep, n)
		}
		wantUIDs(t, "INBOX match", match(t, s, "INBOX", `"zebra" OR "report"`))
		wantUIDs(t, "Sent match", match(t, s, "Sent", `"zebra"`), 4)
		if _, ok, _ := s.Progress(t.Context(), testAccount, "INBOX"); ok {
			t.Errorf("keep=%v: INBOX progress kept", keep)
		}
		if _, ok, _ := s.Progress(t.Context(), testAccount, "Sent"); !ok {
			t.Errorf("keep=%v: Sent progress dropped", keep)
		}
		wantRecovery := 0
		if keep {
			wantRecovery = 2 // UID 3 has no Message-ID
		}
		if n := countRows(t, s, "SELECT count(*) FROM ref_recovery WHERE removed_at = ?", t0.Add(time.Minute).Unix()); n != wantRecovery {
			t.Errorf("keep=%v: %d recovery rows, want %d", keep, n, wantRecovery)
		}
	}
}

func TestDropAccount(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	other := testRow("INBOX", 1)
	other.Account = "other"
	putRows(t, s, testRow("INBOX", 1), testRow("Sent", 2), other)
	if err := s.PutProgress(t.Context(), Progress{Account: testAccount, Folder: "INBOX", UIDValidity: testUIDValidity}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropFolder(t.Context(), testAccount, "Sent", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddBudget(t.Context(), testAccount, "2026-11-03", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.DropAccount(t.Context(), testAccount); err != nil {
		t.Fatalf("DropAccount: %v", err)
	}
	for _, tt := range []struct {
		query string
		want  int
	}{
		{"SELECT count(*) FROM messages WHERE account = 'acme'", 0},
		{"SELECT count(*) FROM messages WHERE account = 'other'", 1},
		{"SELECT count(*) FROM folders", 0},
		{"SELECT count(*) FROM ref_recovery", 0},
		{"SELECT count(*) FROM budget", 1},
	} {
		if n := countRows(t, s, tt.query); n != tt.want {
			t.Errorf("%s = %d, want %d", tt.query, n, tt.want)
		}
	}
}
