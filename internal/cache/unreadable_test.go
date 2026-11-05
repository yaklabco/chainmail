package cache

import (
	"reflect"
	"slices"
	"strings"
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
// summary says so. A later full read clears it. A degraded read of a row
// read whole keeps the attachments it knew, and their filename: terms.
func TestRowAttachmentsUnknown(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	degraded := func(uid uint32) Row {
		r := testRow("INBOX", uid)
		r.AttachmentsUnknown = true
		return r
	}
	whole := func(uid uint32) Row {
		r := testRow("INBOX", uid)
		r.HasAttachment = true
		r.Attachments = []mailstore.Attachment{{Part: "2", Filename: "Invoice.pdf", ContentType: "application/pdf", Size: 300}}
		return r
	}
	row := func(uid uint32) Row {
		t.Helper()
		got, ok, err := s.Row(t.Context(), testRef("INBOX", uid))
		if err != nil || !ok {
			t.Fatalf("Row %d: ok %v, err %v", uid, ok, err)
		}
		return got
	}

	putRows(t, s, degraded(5))
	if got := row(5); !reflect.DeepEqual(got, degraded(5)) {
		t.Errorf("Row =\n%+v\nwant\n%+v", got, degraded(5))
	}
	if !row(5).Summary().AttachmentsUnknown {
		t.Error("Summary lost AttachmentsUnknown")
	}
	putRows(t, s, whole(5))
	if got := row(5); !reflect.DeepEqual(got, whole(5)) {
		t.Errorf("after a full read Row =\n%+v\nwant\n%+v", got, whole(5))
	}

	putRows(t, s, whole(6))
	putRows(t, s, degraded(6))
	if got := row(6); !reflect.DeepEqual(got, whole(6)) {
		t.Errorf("after a degraded read of a whole row Row =\n%+v\nwant\n%+v", got, whole(6))
	}
	src := mailstore.Source{Folder: "INBOX", UIDValidity: testUIDValidity}
	uids, _, err := s.SearchUIDs(t.Context(), testAccount, src, Where{SQL: "m.attachments_norm LIKE ?", Args: []any{"%invoice.pdf%"}}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(uids, []uint32{6, 5}) {
		t.Errorf("filename matches = %v, want [6 5]", uids)
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

// HasUnreadable reports whether any of the folders holds a message
// recorded as unreadable. Rows read without their body structure do not
// count, and no folders means none.
func TestHasUnreadable(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putProgress(t, s, "INBOX", "Sent", "Archive")
	markUnreadable(t, s, "Sent", testUIDValidity, 5)
	degraded := testRow("Archive", 6)
	degraded.AttachmentsUnknown = true
	putRows(t, s, degraded)
	tests := []struct {
		folders []string
		want    bool
	}{
		{folders: []string{"INBOX", "Sent"}, want: true},
		{folders: []string{"Sent"}, want: true},
		{folders: []string{"INBOX", "Archive"}, want: false},
		{folders: nil, want: false},
	}
	for _, tt := range tests {
		got, err := s.HasUnreadable(t.Context(), testAccount, tt.folders)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("HasUnreadable(%v) = %v, want %v", tt.folders, got, tt.want)
		}
	}
	if got, err := s.HasUnreadable(t.Context(), "other", []string{"Sent"}); err != nil || got {
		t.Errorf("HasUnreadable of another account = %v, %v; want false", got, err)
	}
}

// Incomplete runs from indexes only. Cache status calls it on every
// account, and a scan of an account's messages would hold the store's one
// connection for about a second on a large account.
func TestIncompleteUsesIndexes(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+incompleteQuery, testAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "USING COVERING INDEX messages_attachments_unknown") {
		t.Errorf("plan does not read messages through messages_attachments_unknown:\n%s", all)
	}
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN messages") || strings.HasPrefix(step, "SCAN unreadable") {
			t.Errorf("plan scans a table: %q\n%s", step, all)
		}
	}
}
