package cache

import (
	"reflect"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/ref"
)

func TestMessageIDOf(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	noID := testRow("INBOX", 3)
	noID.MessageID = ""
	putRows(t, s, testRow("INBOX", 1), testRow("Old", 2), noID)
	if err := s.DropFolder(t.Context(), testAccount, "Old", true); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		r      ref.Ref
		want   string
		wantOK bool
	}{
		{testRef("INBOX", 1), "m1@example.com", true},
		{testRef("Old", 2), "m2@example.com", true},
		{testRef("INBOX", 3), "", false},
		{testRef("INBOX", 9), "", false},
	} {
		got, ok, err := s.MessageIDOf(t.Context(), tt.r)
		if err != nil || ok != tt.wantOK || got != tt.want {
			t.Errorf("MessageIDOf(%s/%d) = %q, %v, %v; want %q, %v", tt.r.Folder, tt.r.UID, got, ok, err, tt.want, tt.wantOK)
		}
	}
}

func TestFindByMessageID(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	a, b := testRow("INBOX", 1), testRow("Archive", 5)
	b.MessageID = a.MessageID
	b.GmailMsgID = 77
	other := testRow("INBOX", 1)
	other.Account = "other"
	putRows(t, s, a, b, other, testRow("INBOX", 2))
	got, err := s.FindByMessageID(t.Context(), testAccount, a.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	want := []ref.Ref{b.Ref(), a.Ref()}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindByMessageID = %+v, want %+v", got, want)
	}
	if got, err := s.FindByMessageID(t.Context(), testAccount, ""); err != nil || got != nil {
		t.Errorf("FindByMessageID(\"\") = %v, %v; want nil", got, err)
	}
}

func TestPruneRecovery(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	putRows(t, s, testRow("A", 1), testRow("B", 2))
	if err := s.DropFolder(t.Context(), testAccount, "A", true); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(48 * time.Hour))
	if err := s.DropFolder(t.Context(), testAccount, "B", true); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneRecovery(t.Context(), t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.MessageIDOf(t.Context(), testRef("A", 1)); ok {
		t.Error("recovery row removed before the cutoff survived")
	}
	if _, ok, _ := s.MessageIDOf(t.Context(), testRef("B", 2)); !ok {
		t.Error("recovery row removed after the cutoff was pruned")
	}
}

// TestRecoverRef checks the recovery rule the cache view and the daemon's
// links share: r's Message-ID, from its row or ref_recovery, matched
// against the account's other rows, with exactly one match the answer.
func TestRecoverRef(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	moved, copied, twice, alone := testRow("INBOX", 1), testRow("Archive", 5), testRow("INBOX", 2), testRow("INBOX", 3)
	copied.MessageID = moved.MessageID
	dupA, dupB := testRow("Archive", 6), testRow("Archive", 7)
	dupA.MessageID, dupB.MessageID = twice.MessageID, twice.MessageID
	gone := testRow("Old", 4)
	gone.MessageID = alone.MessageID
	other := testRow("Archive", 8)
	other.Account, other.MessageID = "other", moved.MessageID
	putRows(t, s, moved, copied, twice, dupA, dupB, alone, gone, other)
	if err := s.DropFolder(t.Context(), testAccount, "Old", true); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		r      ref.Ref
		want   ref.Ref
		wantOK bool
	}{
		{name: "one other row", r: moved.Ref(), want: copied.Ref(), wantOK: true},
		{name: "from ref_recovery", r: testRef("Old", 4), want: alone.Ref(), wantOK: true},
		{name: "several matches", r: twice.Ref()},
		{name: "only its own row", r: testRef("INBOX", 3)},
		{name: "no Message-ID known", r: testRef("INBOX", 99)},
	} {
		got, ok, err := s.RecoverRef(t.Context(), testAccount, tt.r)
		if err != nil || ok != tt.wantOK || got != tt.want {
			t.Errorf("%s: RecoverRef(%s/%d) = %+v, %v, %v; want %+v, %v", tt.name, tt.r.Folder, tt.r.UID, got, ok, err, tt.want, tt.wantOK)
		}
	}
}
