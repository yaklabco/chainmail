package cache

import (
	"reflect"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// putProgress gives testAccount's folder progress at testUIDValidity.
func putProgress(t *testing.T, s *Store, folders ...string) {
	t.Helper()
	for _, f := range folders {
		if err := s.PutProgress(t.Context(), Progress{Account: testAccount, Folder: f, UIDValidity: testUIDValidity}); err != nil {
			t.Fatal(err)
		}
	}
}

func markUnreadable(t *testing.T, s *Store, folder string, uidValidity uint32, uids ...uint32) {
	t.Helper()
	src := mailstore.Source{Folder: folder, UIDValidity: uidValidity}
	if err := s.MarkUnreadable(t.Context(), testAccount, src, uids); err != nil {
		t.Fatalf("MarkUnreadable: %v", err)
	}
}

// unreadableRows renders testAccount's unreadable records as folder, UID,
// attempts and the first and last times, in key order.
func unreadableRows(t *testing.T, s *Store) [][5]int64 {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT folder = 'INBOX', uid, attempts, first_at, last_at FROM unreadable
		 WHERE account = ? ORDER BY folder, uidvalidity, uid`, testAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out [][5]int64
	for rows.Next() {
		var r [5]int64
		if err := rows.Scan(&r[0], &r[1], &r[2], &r[3], &r[4]); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// MarkUnreadable records each UID once and counts its attempts. It records
// nothing for a folder without progress at that UIDVALIDITY, so no record
// outlives its folder, nor for a message the cache holds.
func TestMarkUnreadable(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	putProgress(t, s, "INBOX")
	putRows(t, s, testRow("INBOX", 4))
	markUnreadable(t, s, "INBOX", testUIDValidity, 5, 6, 4)
	clock.Set(t0.Add(time.Minute))
	markUnreadable(t, s, "INBOX", testUIDValidity, 5)
	markUnreadable(t, s, "INBOX", testUIDValidity+1, 7)
	markUnreadable(t, s, "Sent", testUIDValidity, 8)
	markUnreadable(t, s, "INBOX", testUIDValidity)
	at, later := t0.Unix(), t0.Add(time.Minute).Unix()
	if got, want := unreadableRows(t, s), [][5]int64{{1, 5, 2, at, later}, {1, 6, 1, at, at}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable = %v, want %v", got, want)
	}
	got, err := s.UnreadableUIDs(t.Context(), testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[mailstore.Source][]uint32{{Folder: "INBOX", UIDValidity: testUIDValidity}: {5, 6}}; !reflect.DeepEqual(got, want) {
		t.Errorf("UnreadableUIDs = %v, want %v", got, want)
	}
}

// A row written for a recorded UID, by any writer, deletes its record in
// the same transaction: the message has been read.
func TestPutRowsForgetsUnreadable(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putProgress(t, s, "INBOX")
	markUnreadable(t, s, "INBOX", testUIDValidity, 5, 6, 7)
	putRows(t, s, testRow("INBOX", 5))
	if _, err := s.PutNewRows(t.Context(), []Row{testRow("INBOX", 7)}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, want := unreadableRows(t, s), [][5]int64{{1, 6, 1, t0.Unix(), t0.Unix()}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable = %v, want %v", got, want)
	}
	if err := s.ForgetUnreadable(t.Context(), testAccount, mailstore.Source{Folder: "INBOX", UIDValidity: testUIDValidity}, []uint32{6}); err != nil {
		t.Fatal(err)
	}
	if got := unreadableRows(t, s); got != nil {
		t.Errorf("unreadable after ForgetUnreadable = %v, want none", got)
	}
}

// DeleteRow, which applies an expunged event, also ends the message's
// record: the engine reports the expunge of mail it could not read too.
func TestDeleteRowForgetsUnreadable(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putProgress(t, s, "INBOX")
	markUnreadable(t, s, "INBOX", testUIDValidity, 5, 6)
	if err := s.DeleteRow(t.Context(), testRef("INBOX", 5)); err != nil {
		t.Fatal(err)
	}
	if got, want := unreadableRows(t, s), [][5]int64{{1, 6, 1, t0.Unix(), t0.Unix()}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable = %v, want UID 6's alone", got)
	}
}

// Records drop with their folder and their account.
func TestUnreadableDropsWithItsFolder(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putProgress(t, s, "INBOX", "Sent")
	markUnreadable(t, s, "INBOX", testUIDValidity, 5)
	markUnreadable(t, s, "Sent", testUIDValidity, 6)
	if err := s.DropFolder(t.Context(), testAccount, "Sent", false); err != nil {
		t.Fatal(err)
	}
	if got, want := unreadableRows(t, s), [][5]int64{{1, 5, 1, t0.Unix(), t0.Unix()}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable after DropFolder = %v, want INBOX's", got)
	}
	if err := s.DropAccount(t.Context(), testAccount); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM unreadable"); n != 0 {
		t.Errorf("%d unreadable records after DropAccount, want 0", n)
	}
}

// A row read without its body structure keeps AttachmentsUnknown, and its
// summary says so. A later full read clears it.
func TestRowAttachmentsUnknown(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	want := testRow("INBOX", 5)
	want.AttachmentsUnknown = true
	putRows(t, s, want)
	got, ok, err := s.Row(t.Context(), want.Ref())
	if err != nil || !ok {
		t.Fatalf("Row: ok %v, err %v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Row =\n%+v\nwant\n%+v", got, want)
	}
	if !got.Summary().AttachmentsUnknown {
		t.Error("Summary lost AttachmentsUnknown")
	}
	putRows(t, s, testRow("INBOX", 5))
	if got, _, _ := s.Row(t.Context(), want.Ref()); got.AttachmentsUnknown {
		t.Error("a full read left AttachmentsUnknown set")
	}
}

// Incomplete counts each folder's unreadable records and rows read without
// their body structure, for folders with either, in folder order.
func TestIncomplete(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putProgress(t, s, "INBOX", "Sent", "Drafts")
	unknown := func(folder string, uid uint32) Row {
		r := testRow(folder, uid)
		r.AttachmentsUnknown = true
		return r
	}
	other := unknown("INBOX", 9)
	other.Account = "other"
	putRows(t, s, unknown("INBOX", 1), unknown("INBOX", 2), testRow("INBOX", 3), unknown("Sent", 4), testRow("Drafts", 5), other)
	markUnreadable(t, s, "INBOX", testUIDValidity, 6)
	markUnreadable(t, s, "Trash", testUIDValidity, 7) // no progress: not recorded
	got, err := s.Incomplete(t.Context(), testAccount)
	if err != nil {
		t.Fatal(err)
	}
	want := []FolderIncomplete{
		{Folder: "INBOX", Unreadable: 1, AttachmentsUnknown: 2},
		{Folder: "Sent", AttachmentsUnknown: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Incomplete = %+v, want %+v", got, want)
	}
}
