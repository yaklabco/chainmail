package cache

import (
	"database/sql"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/ref"
)

func putBody(t *testing.T, s *Store, r ref.Ref, raw []byte, read bool) {
	t.Helper()
	ok, err := s.PutBody(t.Context(), r, Body{Raw: raw}, read)
	if err != nil || !ok {
		t.Fatalf("PutBody(%v): ok %v, err %v", r, ok, err)
	}
}

// lastReadAt reads the last_read_at of r's body.
func lastReadAt(t *testing.T, s *Store, r ref.Ref) sql.NullInt64 {
	t.Helper()
	var at sql.NullInt64
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT b.last_read_at FROM bodies b JOIN messages m ON m.id = b.msg WHERE "+msgKeyM, refArgs(r)...).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestPutBody(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	r := testRef("INBOX", 1)
	ok, err := s.PutBody(t.Context(), r, Body{Raw: rawMessage("x", "y")}, false)
	if err != nil || ok {
		t.Fatalf("PutBody without a row: ok %v, err %v; want false, nil", ok, err)
	}
	putRows(t, s, testRow("INBOX", 1))
	raw := rawMessage("Report 1", "hello")
	if ok, err := s.PutBody(t.Context(), r, Body{Raw: raw, TextClipped: true}, false); err != nil || !ok {
		t.Fatalf("PutBody: ok %v, err %v", ok, err)
	}
	got, ok, err := s.Body(t.Context(), r)
	if err != nil || !ok {
		t.Fatalf("Body: ok %v, err %v", ok, err)
	}
	if !reflect.DeepEqual(got, Body{Raw: raw, TextClipped: true}) {
		t.Errorf("Body = %+v, want the stored raw with TextClipped", got)
	}
	if at := lastReadAt(t, s, r); at.Valid {
		t.Errorf("last_read_at = %v after an unread put, want NULL", at.Int64)
	}
	if n := countRows(t, s, "SELECT count(*) FROM bodies WHERE size_bytes = ? AND fetched_at = ?", len(raw), t0.Unix()); n != 1 {
		t.Error("size_bytes or fetched_at not stamped")
	}
	clock.Set(t0.Add(time.Hour))
	putBody(t, s, r, raw, true)
	if at := lastReadAt(t, s, r); at.Int64 != t0.Add(time.Hour).Unix() {
		t.Errorf("last_read_at = %v after a read put, want %d", at, t0.Add(time.Hour).Unix())
	}
	clock.Set(t0.Add(2 * time.Hour))
	putBody(t, s, r, raw, false)
	if at := lastReadAt(t, s, r); at.Int64 != t0.Add(time.Hour).Unix() {
		t.Errorf("last_read_at = %v after an unread re-put, want it kept at %d", at, t0.Add(time.Hour).Unix())
	}
	if _, ok, err := s.Body(t.Context(), testRef("INBOX", 2)); err != nil || ok {
		t.Errorf("Body of a missing row: ok %v, err %v", ok, err)
	}
}

func TestPutBodyEmptyRaw(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putRows(t, s, testRow("INBOX", 1))
	putBody(t, s, testRef("INBOX", 1), nil, false)
	got, ok, err := s.Body(t.Context(), testRef("INBOX", 1))
	if err != nil || !ok || len(got.Raw) != 0 {
		t.Errorf("Body = %+v, %v, %v; want an empty body", got, ok, err)
	}
}

func TestBodyTermsOnlyWithBody(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putRows(t, s, testRow("INBOX", 1))
	wantUIDs(t, "body term before the body", match(t, s, "INBOX", `"zebra"`))
	putBody(t, s, testRef("INBOX", 1), rawMessage("Report 1", "A Zébra crossing"), false)
	wantUIDs(t, "body term with the body", match(t, s, "INBOX", `body:"zebra"`), 1)
	wantUIDs(t, "header term with the body", match(t, s, "INBOX", `subject:"report"`), 1)
	html := []byte("Subject: h\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <b>giraffe</b></p>\r\n")
	putBody(t, s, testRef("INBOX", 1), html, false)
	wantUIDs(t, "old body term after a new body", match(t, s, "INBOX", `"zebra"`))
	wantUIDs(t, "HTML body term", match(t, s, "INBOX", `"giraffe"`), 1)
}

func TestPutBodyClearsFailure(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putRows(t, s, testRow("INBOX", 1))
	if _, err := s.RecordBodyFailure(t.Context(), testRef("INBOX", 1), "fetch_failed"); err != nil {
		t.Fatal(err)
	}
	putBody(t, s, testRef("INBOX", 1), rawMessage("Report 1", "ok"), false)
	if n := countRows(t, s, "SELECT count(*) FROM body_failures"); n != 0 {
		t.Errorf("body_failures has %d rows after PutBody, want 0", n)
	}
}

func TestMarkRead(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	putRows(t, s, testRow("INBOX", 1), testRow("INBOX", 2))
	putBody(t, s, testRef("INBOX", 1), rawMessage("Report 1", "x"), false)
	clock.Set(t0.Add(3 * time.Hour))
	if err := s.MarkRead(t.Context(), testRef("INBOX", 1)); err != nil {
		t.Fatal(err)
	}
	if at := lastReadAt(t, s, testRef("INBOX", 1)); at.Int64 != t0.Add(3*time.Hour).Unix() {
		t.Errorf("last_read_at = %v, want %d", at, t0.Add(3*time.Hour).Unix())
	}
	for _, r := range []ref.Ref{testRef("INBOX", 2), testRef("INBOX", 9)} {
		if err := s.MarkRead(t.Context(), r); err != nil {
			t.Errorf("MarkRead(%v) without a body: %v", r, err)
		}
	}
}

func TestRecordBodyFailure(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	putRows(t, s, testRow("INBOX", 1))
	r := testRef("INBOX", 1)
	for i, reason := range []string{"fetch_failed", "parse_failed", "fetch_failed"} {
		clock.Set(t0.Add(time.Duration(i) * time.Hour))
		n, err := s.RecordBodyFailure(t.Context(), r, reason)
		if err != nil || n != i+1 {
			t.Fatalf("attempt %d: n %d, err %v", i+1, n, err)
		}
	}
	var first, last int64
	var reason string
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT first_attempt_at, last_attempt_at, reason FROM body_failures").Scan(&first, &last, &reason); err != nil {
		t.Fatal(err)
	}
	if first != t0.Unix() || last != t0.Add(2*time.Hour).Unix() || reason != "fetch_failed" {
		t.Errorf("first %d last %d reason %q, want %d %d fetch_failed", first, last, reason, t0.Unix(), t0.Add(2*time.Hour).Unix())
	}
	if n, err := s.RecordBodyFailure(t.Context(), testRef("INBOX", 9), "gone"); err != nil || n != 0 {
		t.Errorf("failure of a missing row: n %d, err %v; want 0, nil", n, err)
	}
	if _, err := s.RecordBodyFailure(t.Context(), r, ""); err == nil {
		t.Error("an empty reason was accepted")
	}
}

func TestBodylessSince(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t)
	// testRow(uid) arrives uid hours before t0.
	putRows(t, s, testRow("INBOX", 1), testRow("INBOX", 2), testRow("INBOX", 3), testRow("INBOX", 4),
		testRow("INBOX", 5), testRow("INBOX", 6), testRow("INBOX", 30), testRow("Sent", 7))
	putBody(t, s, testRef("INBOX", 2), rawMessage("r", "b"), false)
	fail := func(uid uint32, n int, at time.Time) {
		clock.Set(at)
		for range n {
			if _, err := s.RecordBodyFailure(t.Context(), testRef("INBOX", uid), "fetch_failed"); err != nil {
				t.Fatal(err)
			}
		}
	}
	fail(3, 3, t0.Add(-10*time.Hour))   // final
	fail(4, 1, t0.Add(-10*time.Hour))   // retry is due
	fail(5, 1, t0.Add(-30*time.Minute)) // retried too recently
	since := t0.Add(-24 * time.Hour)
	retryAfter := t0.Add(-time.Hour)
	refs, err := s.BodylessSince(t.Context(), testAccount, "INBOX", since, finalAttempts, retryAfter, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, r := range refs {
		got = append(got, r.UID)
	}
	wantUIDs(t, "BodylessSince", got, 1, 4, 6)
	refs, err = s.BodylessSince(t.Context(), testAccount, "INBOX", since, finalAttempts, retryAfter, 2)
	if err != nil || len(refs) != 2 || refs[0] != testRef("INBOX", 1) {
		t.Errorf("limit 2 = %v, %v; want UIDs 1 and 4", refs, err)
	}
	if refs, err := s.BodylessSince(t.Context(), testAccount, "INBOX", since, finalAttempts, retryAfter, 0); err != nil || refs != nil {
		t.Errorf("limit 0 = %v, %v; want nil", refs, err)
	}
}

func TestPutRowsKeepsBody(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	r := testRow("INBOX", 5)
	putRows(t, s, r)
	putBody(t, s, r.Ref(), rawMessage(r.Subject, "the zebra crossing"), true)
	r.Subject = "Renamed quarterly"
	putRows(t, s, r)
	if _, ok, err := s.Body(t.Context(), r.Ref()); err != nil || !ok {
		t.Fatalf("Body after upsert: ok %v, err %v; want it kept", ok, err)
	}
	wantUIDs(t, "match body term", match(t, s, "INBOX", `"zebra"`), 5)
	wantUIDs(t, "match new subject", match(t, s, "INBOX", `subject:"quarterly"`), 5)
	wantUIDs(t, "match old subject", match(t, s, "INBOX", `subject:"report"`))
}

func TestDiskFullIsClassified(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	putRows(t, s, testRow("INBOX", 1))
	var pages int64
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// max_page_count stands in for a full disk on every platform. PRAGMA
	// takes no bound parameters.
	if _, err := s.db.ExecContext(t.Context(), "PRAGMA max_page_count = "+strconv.FormatInt(pages+2, 10)); err != nil {
		t.Fatal(err)
	}
	big := rawMessage("big", strings.Repeat("word ", 1<<18))
	_, err := s.PutBody(t.Context(), testRef("INBOX", 1), Body{Raw: big}, false)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("PutBody over max_page_count: err = %v, want ErrDiskFull", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v also matches ErrCorrupt", err)
	}
	if _, ok, err := s.Body(t.Context(), testRef("INBOX", 1)); err != nil || ok {
		t.Errorf("Body after the failed write: ok %v, err %v; want nothing stored", ok, err)
	}
}
