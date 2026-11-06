package cache

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

func (c *stubClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

const (
	testAccount     = "acme"
	testUIDValidity = 7
)

// testRow is a row of testAccount in folder, arriving uid hours before t0.
func testRow(folder string, uid uint32) Row {
	at := t0.Add(-time.Duration(uid) * time.Hour)
	return Row{
		Account:      testAccount,
		Folder:       folder,
		UIDValidity:  testUIDValidity,
		UID:          uid,
		InternalDate: at,
		Date:         at.Add(-time.Minute),
		Size:         1000 + int64(uid),
		Subject:      fmt.Sprintf("Report %d", uid),
		From:         []mailstore.Address{{Name: "Ann Lee", Email: "ann@example.com"}},
		To:           []mailstore.Address{{Name: "Bob", Email: "bob@example.org"}},
		MessageID:    fmt.Sprintf("m%d@example.com", uid),
		ThreadID:     fmt.Sprintf("t%d", uid),
		Flags:        []string{`\Seen`},
	}
}

// testRef is the ref of testRow(folder, uid).
func testRef(folder string, uid uint32) ref.Ref {
	return ref.Ref{Account: testAccount, Folder: folder, UIDValidity: testUIDValidity, UID: uid}
}

// rawMessage is a plain-text MIME message, as Message.Raw holds one.
func rawMessage(subject, text string) []byte {
	return []byte("From: Ann Lee <ann@example.com>\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + text + "\r\n")
}

func putRows(t *testing.T, s *Store, rows ...Row) {
	t.Helper()
	if err := s.PutRows(t.Context(), rows); err != nil {
		t.Fatalf("PutRows: %v", err)
	}
}

// match returns the UIDs of testAccount's rows in folder whose FTS terms
// match fts, highest first. It queries messages_fts directly.
func match(t *testing.T, s *Store, folder, fts string) []uint32 {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT m.uid FROM messages m WHERE m.account = ? AND m.folder = ?
		 AND m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?) ORDER BY m.uid DESC`,
		testAccount, folder, fts)
	if err != nil {
		t.Fatalf("MATCH %q: %v", fts, err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var uids []uint32
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			t.Fatal(err)
		}
		uids = append(uids, uid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return uids
}

// wantUIDs fails the test unless got equals want.
func wantUIDs(t *testing.T, what string, got []uint32, want ...uint32) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// fullRow sets every Row field.
func fullRow() Row {
	return Row{
		Account:       testAccount,
		Folder:        "[Gmail]/All Mail",
		UIDValidity:   11,
		UID:           42,
		GmailMsgID:    1781234567890123456,
		GmailThrID:    1781234567890000000,
		InternalDate:  time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC),
		Date:          time.Date(2026, 10, 30, 7, 59, 0, 0, time.UTC),
		Size:          48213,
		Subject:       "Invoice 77",
		From:          []mailstore.Address{{Name: "Acme Billing", Email: "billing@acme.example"}},
		To:            []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}, {Email: "ops@example.com"}},
		Cc:            []mailstore.Address{{Name: "Cy", Email: "cy@example.com"}},
		Bcc:           []mailstore.Address{{Name: "Dee", Email: "dee@example.com"}},
		MessageID:     "inv77@acme.example",
		InReplyTo:     []string{"<inv76@acme.example>"},
		References:    []string{"<inv75@acme.example>", "<inv76@acme.example>"},
		ThreadID:      "18b7c0ffee000000",
		Flags:         []string{`\Seen`, "$Label1"},
		Labels:        []string{`\Inbox`, "Bills"},
		HasAttachment: true,
		Attachments:   []mailstore.Attachment{{Part: "2", Filename: "Invoice-77.PDF", ContentType: "application/pdf", Size: 40000}},
	}
}

func TestPutRowsRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	want := fullRow()
	putRows(t, s, want)
	got, ok, err := s.Row(t.Context(), want.Ref())
	if err != nil || !ok {
		t.Fatalf("Row: ok %v, err %v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Row =\n%+v\nwant\n%+v", got, want)
	}
	empty := testRow("INBOX", 1)
	empty.From, empty.To, empty.Flags = nil, nil, nil
	putRows(t, s, empty)
	got, _, err = s.Row(t.Context(), empty.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, empty) {
		t.Errorf("row with empty lists =\n%+v\nwant\n%+v", got, empty)
	}
}

func TestRowViews(t *testing.T) {
	t.Parallel()
	r := fullRow()
	wantRef := ref.Ref{Account: testAccount, Folder: "[Gmail]/All Mail", UIDValidity: 11, UID: 42, GmailMsgID: 1781234567890123456}
	if r.Ref() != wantRef {
		t.Errorf("Ref = %+v, want %+v", r.Ref(), wantRef)
	}
	wantSum := mailstore.Summary{
		Ref: wantRef, Date: r.Date, InternalDate: r.InternalDate, From: r.From, To: r.To,
		Subject: r.Subject, Flags: r.Flags, Size: r.Size, HasAttachment: true, ThreadID: r.ThreadID, Labels: r.Labels,
		MessageID: "inv77@acme.example",
	}
	if got := r.Summary(); !reflect.DeepEqual(got, wantSum) {
		t.Errorf("Summary =\n%+v\nwant\n%+v", got, wantSum)
	}
	raw := rawMessage("Invoice 77", "pay me")
	wantMsg := &mailstore.Message{
		Summary: wantSum, Cc: r.Cc, MessageID: r.MessageID, InReplyTo: r.InReplyTo, References: r.References,
		Raw: raw, TextClipped: true, Attachments: r.Attachments,
	}
	if got := r.Message(Body{Raw: raw, TextClipped: true}); !reflect.DeepEqual(got, wantMsg) {
		t.Errorf("Message =\n%+v\nwant\n%+v", got, wantMsg)
	}
}

func TestWriteColumnsMatchRowArgs(t *testing.T) {
	t.Parallel()
	args, err := rowArgs(fullRow(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != len(writeColumns) {
		t.Errorf("rowArgs gives %d values for %d columns", len(args), len(writeColumns))
	}
}

func TestPutRowsIsIdempotent(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	r := testRow("INBOX", 3)
	putRows(t, s, r)
	var id1 int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id1); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(time.Hour))
	putRows(t, s, r, r)
	if n := countRows(t, s, "SELECT count(*) FROM messages"); n != 1 {
		t.Fatalf("messages has %d rows after three puts of one key, want 1", n)
	}
	var (
		id2      int64
		cachedAt int64
	)
	if err := s.db.QueryRowContext(t.Context(), "SELECT id, cached_at FROM messages").Scan(&id2, &cachedAt); err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("id changed from %d to %d on upsert", id1, id2)
	}
	if cachedAt != t0.Add(time.Hour).Unix() {
		t.Errorf("cached_at = %d, want the time of the last put", cachedAt)
	}
	wantUIDs(t, "match report", match(t, s, "INBOX", `"report"`), 3)
}

func TestPutRowsRefusesIncompleteKey(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	for name, spoil := range map[string]func(*Row){
		"account":     func(r *Row) { r.Account = "" },
		"folder":      func(r *Row) { r.Folder = "" },
		"uidvalidity": func(r *Row) { r.UIDValidity = 0 },
		"uid":         func(r *Row) { r.UID = 0 },
	} {
		bad := testRow("INBOX", 2)
		spoil(&bad)
		if err := s.PutRows(t.Context(), []Row{testRow("INBOX", 1), bad}); err == nil {
			t.Errorf("PutRows with no %s succeeded", name)
		}
	}
	if n := countRows(t, s, "SELECT count(*) FROM messages"); n != 0 {
		t.Errorf("messages has %d rows, want none written", n)
	}
}

func TestNormColumns(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	r := fullRow()
	r.Subject = "Invoice\r\n 77 DUE"
	r.From = []mailstore.Address{{Name: "Acme BILLING", Email: "Billing@Acme.Example"}}
	r.To = []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}, {Email: "ops@example.com"}}
	r.Cc = nil
	putRows(t, s, r)
	var got [8]string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT subject_norm, from_norm, to_norm, cc_norm, bcc_norm, flags_norm, labels_norm, attachments_norm FROM messages`).
		Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &got[6], &got[7]); err != nil {
		t.Fatal(err)
	}
	want := [8]string{
		"invoice   77 due",
		"\nacme billing billing@acme.example\n",
		"\nann ann@example.com\n ops@example.com\n",
		"\n",
		"\ndee dee@example.com\n",
		"\n\\seen\n$label1\n",
		"\n\\inbox\nbills\n",
		"\ninvoice-77.pdf\n",
	}
	names := [8]string{"subject_norm", "from_norm", "to_norm", "cc_norm", "bcc_norm", "flags_norm", "labels_norm", "attachments_norm"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %q, want %q", names[i], got[i], want[i])
		}
	}
	// The element match T7 compiles finds a label exactly and not a prefix.
	for _, tt := range []struct {
		pattern string
		want    int
	}{
		{"%\nbills\n%", 1},
		{"%\nbill\n%", 0},
		{"%\n\\inbox\n%", 1},
	} {
		if n := countRows(t, s, "SELECT count(*) FROM messages WHERE labels_norm LIKE ?", tt.pattern); n != tt.want {
			t.Errorf("labels_norm LIKE %q matched %d rows, want %d", tt.pattern, n, tt.want)
		}
	}
}

func TestFTSHeaderTerms(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	r := testRow("INBOX", 1)
	r.Subject = "Café opening"
	r.From = []mailstore.Address{{Name: "Zoë Quinn", Email: "zoe@quinn.example"}}
	putRows(t, s, r, testRow("INBOX", 2))
	for _, tt := range []struct {
		fts  string
		want []uint32
	}{
		{`"cafe"`, []uint32{1}},
		{`"zoe"`, []uint32{1}},
		{`names:"quinn"`, []uint32{1}},
		{`addresses:"quinn"`, []uint32{1}},
		{`addresses:"zoe" AND names:"zoe"`, []uint32{1}},
		{`"bob"`, []uint32{2, 1}},
		{`subject:"bob"`, nil},
		{`"report"`, []uint32{2}},
	} {
		wantUIDs(t, "match "+tt.fts, match(t, s, "INBOX", tt.fts), tt.want...)
	}
}

func TestSetFlags(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	r := testRow("INBOX", 1)
	putRows(t, s, r)
	ok, err := s.SetFlags(t.Context(), r.Ref(), []string{`\Flagged`}, []string{"Work"})
	if err != nil || !ok {
		t.Fatalf("SetFlags: ok %v, err %v", ok, err)
	}
	got, _, err := s.Row(t.Context(), r.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Flags, []string{`\Flagged`}) || !reflect.DeepEqual(got.Labels, []string{"Work"}) {
		t.Errorf("flags %v labels %v, want [\\Flagged] [Work]", got.Flags, got.Labels)
	}
	if n := countRows(t, s, "SELECT count(*) FROM messages WHERE flags_norm = ? AND labels_norm = ?", "\n\\flagged\n", "\nwork\n"); n != 1 {
		t.Error("flags_norm and labels_norm were not updated")
	}
	ok, err = s.SetFlags(t.Context(), testRef("INBOX", 99), nil, nil)
	if err != nil || ok {
		t.Errorf("SetFlags of a missing row: ok %v, err %v; want false, nil", ok, err)
	}
}

func TestRowsOrder(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putRows(t, s, testRow("INBOX", 1), testRow("INBOX", 2), testRow("INBOX", 3), testRow("Sent", 4))
	src := mailstore.Source{Folder: "INBOX", UIDValidity: testUIDValidity}
	rows, err := s.Rows(t.Context(), src, testAccount, []uint32{3, 9, 1, 4, 2})
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, r := range rows {
		got = append(got, r.UID)
	}
	wantUIDs(t, "Rows", got, 3, 1, 2)
	rows, err = s.Rows(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: 8}, testAccount, []uint32{1})
	if err != nil || len(rows) != 0 {
		t.Errorf("Rows under another UIDVALIDITY = %v, %v; want none", rows, err)
	}
	many := make([]uint32, 1200)
	for i := range many {
		many[i] = uint32(i + 1)
	}
	rows, err = s.Rows(t.Context(), src, testAccount, many)
	if err != nil || len(rows) != 3 {
		t.Errorf("Rows of 1200 UIDs = %d rows, %v; want 3", len(rows), err)
	}
}

func TestThreadRows(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	a, b, c := testRow("INBOX", 1), testRow("Sent", 2), testRow("INBOX", 3)
	for _, r := range []*Row{&a, &b, &c} {
		r.ThreadID = "th"
	}
	a.Date = t0.Add(-time.Hour)
	b.Date = t0.Add(-3 * time.Hour)
	c.Date = t0.Add(-time.Hour)
	c.InternalDate = a.InternalDate.Add(-time.Minute)
	other := testRow("INBOX", 4)
	other.Account = "other"
	other.ThreadID = "th"
	putRows(t, s, a, b, c, other, testRow("INBOX", 5))
	rows, err := s.ThreadRows(t.Context(), testAccount, "th")
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, r := range rows {
		got = append(got, r.UID)
	}
	wantUIDs(t, "ThreadRows", got, 2, 3, 1)
	rows, err = s.ThreadRows(t.Context(), testAccount, "")
	if err != nil || len(rows) != 0 {
		t.Errorf("ThreadRows of an empty thread ID = %v, %v; want none", rows, err)
	}
}

// TestRootThreadRows checks the standard server's thread rule over cached
// rows: a row joins the thread rooted at p@x when its Message-ID is p@x or
// its References or In-Reply-To hold p@x as a whole entry, bare or in
// angle brackets, whatever its own thread ID. An ID that only contains
// p@x, another account's row, and an empty root find nothing.
func TestRootThreadRows(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	root := testRow("INBOX", 1)
	root.MessageID = "p@x"
	byRefs := testRow("Sent", 2)
	byRefs.References = []string{"other@x", "p@x"}
	bracketed := testRow("INBOX", 3)
	bracketed.References = []string{"<p@x>"}
	byReply := testRow("Archive", 4)
	byReply.InReplyTo = []string{"p@x"}
	nearMiss := testRow("INBOX", 5)
	nearMiss.References = []string{"xp@x", "p@x.org"}
	nearMiss.InReplyTo = []string{"<xp@x>"}
	other := testRow("INBOX", 6)
	other.Account = "other"
	other.References = []string{"p@x"}
	putRows(t, s, root, byRefs, bracketed, byReply, nearMiss, other)
	rows, err := s.RootThreadRows(t.Context(), testAccount, "p@x")
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, r := range rows {
		got = append(got, r.UID)
	}
	wantUIDs(t, "RootThreadRows", got, 4, 3, 2, 1)
	rows, err = s.RootThreadRows(t.Context(), testAccount, "")
	if err != nil || len(rows) != 0 {
		t.Errorf("RootThreadRows of an empty root = %v, %v; want none", rows, err)
	}
}
