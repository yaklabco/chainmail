package cache

import (
	"reflect"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	// testRow(uid) arrives uid hours before t0; the window starts 10 hours back.
	putRows(t, s, testRow("INBOX", 1), testRow("INBOX", 2), testRow("INBOX", 20), testRow("Sent", 3))
	raw1, raw20 := rawMessage("a", "one"), rawMessage("b", "twenty")
	putBody(t, s, testRef("INBOX", 1), raw1, false)
	putBody(t, s, testRef("INBOX", 20), raw20, false)
	for range finalAttempts {
		if _, err := s.RecordBodyFailure(t.Context(), testRef("INBOX", 2), "fetch_failed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordBodyFailure(t.Context(), testRef("Sent", 3), "fetch_failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProgress(t.Context(), Progress{Account: testAccount, Folder: "Drafts", UIDValidity: 1}); err != nil {
		t.Fatal(err)
	}
	other := testRow("INBOX", 4)
	other.Account = "other"
	putRows(t, s, other)
	got, err := s.Stats(t.Context(), testAccount, t0.Add(-10*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []FolderStats{
		{Folder: "Drafts"},
		{
			Folder: "INBOX", Headers: 3, Bodies: 2, BodyBytes: int64(len(raw1) + len(raw20)),
			WindowMessages: 2, WindowBodies: 1, BodyFailures: 1,
		},
		{Folder: "Sent", Headers: 1, WindowMessages: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Stats =\n%+v\nwant\n%+v", got, want)
	}
}
