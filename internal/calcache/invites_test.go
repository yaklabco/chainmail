package calcache_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
)

func mailInvite(msg, uid, method string, seq int, stamp time.Time) calcache.MailInviteRow {
	return calcache.MailInviteRow{
		Account:    "acme",
		MessageRef: msg,
		UID:        uid,
		Method:     method,
		Sequence:   seq,
		DTStamp:    stamp,
		Raw:        []byte("BEGIN:VCALENDAR " + msg),
		FoundAt:    t0,
	}
}

func TestMailInvitesKeepTheLatestPerUID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := openCache(t)
	rows := []calcache.MailInviteRow{
		mailInvite("m1", "a@x", calendar.MethodRequest, 0, t0),
		mailInvite("m2", "a@x", calendar.MethodRequest, 1, t0.Add(-time.Hour)), // higher sequence wins over a later stamp
		mailInvite("m3", "a@x", calendar.MethodUpdate, 0, t0.Add(time.Hour)),
		mailInvite("m4", "b@x", calendar.MethodRequest, 2, t0),
		mailInvite("m5", "b@x", calendar.MethodCancel, 2, t0.Add(time.Minute)), // same sequence, later stamp
		mailInvite("m6", "c@x", calendar.MethodRequest, 0, t0),
	}
	other := mailInvite("m9", "a@x", calendar.MethodRequest, 5, t0)
	other.Account = "beta"
	rows = append(rows, other)
	for _, r := range rows {
		if err := s.PutMailInvite(ctx, r); err != nil {
			t.Fatalf("PutMailInvite %s: %v", r.MessageRef, err)
		}
	}
	// Storing the same message again replaces its row.
	again := mailInvite("m6", "c@x", calendar.MethodRequest, 0, t0)
	again.Raw = []byte("BEGIN:VCALENDAR m6 again")
	if err := s.PutMailInvite(ctx, again); err != nil {
		t.Fatal(err)
	}

	got, err := s.MailInvites(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, r := range got {
		msgs = append(msgs, r.MessageRef)
	}
	same(t, "MailInvites", []string{"m2", "m5", "m6"}, msgs)
	if string(got[2].Raw) != "BEGIN:VCALENDAR m6 again" {
		t.Errorf("m6 raw = %q, want the replaced row", got[2].Raw)
	}

	want := mailInvite("m2", "a@x", calendar.MethodRequest, 1, t0.Add(-time.Hour))
	want.Ref = calendar.MailInviteRef("acme", "a@x")
	same(t, "MailInvites[0]", want, got[0])

	one, err := s.MailInvite(ctx, calendar.MailInviteRef("acme", "a@x"))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "MailInvite", want, one)
	beta, err := s.MailInvite(ctx, calendar.MailInviteRef("beta", "a@x"))
	if err != nil || beta.MessageRef != "m9" {
		t.Errorf("MailInvite(beta) = %+v, %v; want m9", beta, err)
	}
	if _, err := s.MailInvite(ctx, calendar.MailInviteRef("acme", "zzz")); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("MailInvite(unknown) = %v, want ErrNotFound", err)
	}
	if none, err := s.MailInvites(ctx, "gamma"); err != nil || len(none) != 0 {
		t.Errorf("MailInvites(gamma) = %v, %v; want none", none, err)
	}
}

func TestPutMailInviteRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := openCache(t)
	r := mailInvite("m1", "a@x", calendar.MethodRequest, 0, t0)
	r.Ref = calendar.MailInviteRef("acme", "a@x")
	if err := s.PutMailInvite(ctx, r); err != nil {
		t.Errorf("PutMailInvite with the right ref: %v", err)
	}
	r.Ref = calendar.MailInviteRef("acme", "other")
	if err := s.PutMailInvite(ctx, r); err == nil {
		t.Error("PutMailInvite accepted a ref of another UID")
	}
	for _, bad := range []calcache.MailInviteRow{
		{MessageRef: "m", UID: "u", Method: "REQUEST"},
		{Account: "acme", UID: "u", Method: "REQUEST"},
		{Account: "acme", MessageRef: "m", Method: "REQUEST"},
	} {
		if err := s.PutMailInvite(ctx, bad); err == nil {
			t.Errorf("PutMailInvite(%+v) accepted a row without account, message ref or UID", bad)
		}
	}
}

// A row keeps the charset its raw bytes are in. Announced is set by
// MarkInviteAnnounced on every row of the ref, and storing a row again
// never clears it.
func TestMailInviteCharsetAndAnnounced(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := openCache(t)
	r := mailInvite("m1", "a@x", calendar.MethodRequest, 0, t0)
	r.Charset = "iso-8859-1"
	r.Raw = []byte("BEGIN:VCALENDAR caf\xe9")
	if err := s.PutMailInvite(ctx, r); err != nil {
		t.Fatal(err)
	}
	later := mailInvite("m2", "a@x", calendar.MethodRequest, 1, t0)
	if err := s.PutMailInvite(ctx, later); err != nil {
		t.Fatal(err)
	}
	ref := calendar.MailInviteRef("acme", "a@x")
	got, err := s.MailInvite(ctx, ref)
	if err != nil || got.Announced {
		t.Fatalf("MailInvite = %+v, %v; want an unannounced row", got, err)
	}
	if err := s.MarkInviteAnnounced(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMailInvite(ctx, r); err != nil { // stored again, Announced false
		t.Fatal(err)
	}
	rows, err := s.PendingInvites(ctx, "acme", 100)
	if err != nil || len(rows) != 0 {
		t.Errorf("PendingInvites after MarkInviteAnnounced = %+v, %v; want none", rows, err)
	}
	got, err = s.MailInvite(ctx, ref)
	if err != nil || !got.Announced || got.MessageRef != "m2" {
		t.Errorf("MailInvite = %+v, %v; want m2, announced", got, err)
	}
	if err := s.MarkInviteAnnounced(ctx, calendar.MailInviteRef("acme", "zzz")); err != nil {
		t.Errorf("MarkInviteAnnounced of an unknown ref = %v, want nil", err)
	}

	// The charset comes back with the row's bytes.
	r2 := mailInvite("m3", "b@x", calendar.MethodRequest, 0, t0)
	r2.Charset = "windows-1252"
	r2.Raw = []byte("BEGIN:VCALENDAR \x93q\x94")
	if err := s.PutMailInvite(ctx, r2); err != nil {
		t.Fatal(err)
	}
	b, err := s.MailInvite(ctx, calendar.MailInviteRef("acme", "b@x"))
	if err != nil || b.Charset != "windows-1252" || string(b.Raw) != string(r2.Raw) {
		t.Errorf("MailInvite(b) = %+v, %v; want the charset and the bytes stored", b, err)
	}
}

// PendingInvites returns, per UID of the account, the latest row when it
// is a REQUEST, the UID was first found as a REQUEST, no row of the UID is
// announced and the latest row is not settled, however long ago the UID
// was first found: the UIDs first found longest ago come first.
func TestPendingInvites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := openCache(t)
	at := func(r calcache.MailInviteRow, found time.Time) calcache.MailInviteRow {
		r.FoundAt = found
		return r
	}
	since := t0.Add(-time.Hour)
	rows := []calcache.MailInviteRow{
		mailInvite("m1", "u1", calendar.MethodRequest, 0, t0),                                 // pending
		mailInvite("m2", "u2", calendar.MethodRequest, 0, t0),                                 // announced below
		at(mailInvite("m3", "u3", calendar.MethodCancel, 0, t0), t0),                          // first found as a CANCEL
		at(mailInvite("m4", "u3", calendar.MethodRequest, 1, t0), t0.Add(time.Minute)),        // ...
		mailInvite("m5", "u4", calendar.MethodRequest, 0, t0),                                 // later cancelled
		mailInvite("m6", "u4", calendar.MethodCancel, 1, t0),                                  // ...
		at(mailInvite("m7", "u5", calendar.MethodRequest, 0, t0), since.Add(-8*24*time.Hour)), // found long ago
		mailInvite("m8", "u6", calendar.MethodUpdate, 0, t0),                                  // never a REQUEST
		mailInvite("m9", "u7", calendar.MethodRequest, 0, t0),                                 // pending, updated
		mailInvite("m10", "u7", calendar.MethodRequest, 1, t0),                                // ...the latest
	}
	other := mailInvite("m11", "u8", calendar.MethodRequest, 0, t0)
	other.Account = "beta"
	rows = append(rows, other)
	for _, r := range rows {
		if err := s.PutMailInvite(ctx, r); err != nil {
			t.Fatalf("PutMailInvite %s: %v", r.MessageRef, err)
		}
	}
	if err := s.MarkInviteAnnounced(ctx, calendar.MailInviteRef("acme", "u2")); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingInvites(ctx, "acme", 100)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, r := range got {
		msgs = append(msgs, r.MessageRef)
	}
	same(t, "PendingInvites", []string{"m7", "m1", "m10"}, msgs)

	got, err = s.PendingInvites(ctx, "acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	msgs = msgs[:0]
	for _, r := range got {
		msgs = append(msgs, r.MessageRef)
	}
	same(t, "PendingInvites with a limit of 2", []string{"m7", "m1"}, msgs)
}

// MarkInviteSettled settles one row, so PendingInvites leaves its UID out
// while that row is the latest, and storing the row again keeps it
// settled. A later row of the UID from another message is not settled and
// brings the UID back.
func TestMarkInviteSettled(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := openCache(t)
	first := mailInvite("m1", "u1", calendar.MethodRequest, 0, t0)
	if err := s.PutMailInvite(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInviteSettled(ctx, "acme", "m1", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMailInvite(ctx, first); err != nil { // stored again, Settled false
		t.Fatal(err)
	}
	pending := func() []string {
		t.Helper()
		rows, err := s.PendingInvites(ctx, "acme", 100)
		if err != nil {
			t.Fatal(err)
		}
		var msgs []string
		for _, r := range rows {
			msgs = append(msgs, r.MessageRef)
		}
		return msgs
	}
	same(t, "PendingInvites after MarkInviteSettled", []string(nil), pending())
	got, err := s.MailInvite(ctx, calendar.MailInviteRef("acme", "u1"))
	if err != nil || !got.Settled {
		t.Errorf("MailInvite = %+v, %v; want the row settled", got, err)
	}
	if err := s.PutMailInvite(ctx, mailInvite("m2", "u1", calendar.MethodRequest, 1, t0)); err != nil {
		t.Fatal(err)
	}
	same(t, "PendingInvites after a later REQUEST", []string{"m2"}, pending())
	if err := s.MarkInviteSettled(ctx, "acme", "nope", "u9"); err != nil {
		t.Errorf("MarkInviteSettled of an unknown row = %v, want nil", err)
	}
}

// inviteFetch is a deferred invite part of acme's message msg found at
// found and due at next.
func inviteFetch(msg string, found, next time.Time) calcache.InviteFetchRow {
	return calcache.InviteFetchRow{
		Account: "acme", MessageRef: msg, Part: "2", Method: calendar.MethodRequest,
		Charset: "utf-8", Size: 1234, NextAt: next, FoundAt: found,
	}
}

// Deferred invite fetches are stored once per account, message and part:
// storing a key again keeps its attempts and backoff. DueInviteFetches
// returns those due at a time, oldest found first, at most limit;
// DeferInviteFetch moves one's attempts and next try, and
// DeleteInviteFetch removes it.
func TestInviteFetches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := openCache(t)
	rows := []calcache.InviteFetchRow{
		inviteFetch("m1", t0.Add(time.Minute), t0),
		inviteFetch("m2", t0, t0),
		inviteFetch("m3", t0.Add(-time.Hour), t0.Add(time.Hour)), // not due yet
	}
	other := inviteFetch("m4", t0.Add(2*time.Minute), t0)
	other.Account = "beta"
	rows = append(rows, other)
	if err := s.PutInviteFetches(ctx, rows); err != nil {
		t.Fatal(err)
	}
	due := func(limit int) []string {
		t.Helper()
		got, err := s.DueInviteFetches(ctx, t0, limit)
		if err != nil {
			t.Fatal(err)
		}
		var msgs []string
		for _, r := range got {
			msgs = append(msgs, r.Account+"/"+r.MessageRef)
		}
		return msgs
	}
	same(t, "DueInviteFetches", []string{"acme/m2", "acme/m1", "beta/m4"}, due(10))
	same(t, "DueInviteFetches with a limit of 2", []string{"acme/m2", "acme/m1"}, due(2))

	got, err := s.DueInviteFetches(ctx, t0, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("DueInviteFetches = %+v, %v", got, err)
	}
	want := rows[1]
	if g := got[0]; g.Part != want.Part || g.Method != want.Method || g.Charset != want.Charset || g.Size != want.Size ||
		!g.FoundAt.Equal(want.FoundAt) || !g.NextAt.Equal(want.NextAt) || g.Attempts != 0 {
		t.Errorf("row = %+v, want %+v", g, want)
	}

	moved := got[0]
	moved.Attempts, moved.NextAt = 3, t0.Add(time.Hour)
	if err := s.DeferInviteFetch(ctx, moved); err != nil {
		t.Fatal(err)
	}
	same(t, "DueInviteFetches after DeferInviteFetch", []string{"acme/m1", "beta/m4"}, due(10))
	if err := s.PutInviteFetches(ctx, []calcache.InviteFetchRow{rows[1]}); err != nil { // found again: kept as it is
		t.Fatal(err)
	}
	later, err := s.DueInviteFetches(ctx, t0.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range later {
		if r.MessageRef == "m2" && r.Attempts != 3 {
			t.Errorf("m2 attempts = %d after it was stored again, want 3", r.Attempts)
		}
	}
	if err := s.DeleteInviteFetch(ctx, "acme", "m1", "2"); err != nil {
		t.Fatal(err)
	}
	same(t, "DueInviteFetches after DeleteInviteFetch", []string{"beta/m4"}, due(10))
	if err := s.PutInviteFetches(ctx, []calcache.InviteFetchRow{{Account: "acme", Part: "2"}}); err == nil {
		t.Error("PutInviteFetches accepted a row without a message ref")
	}
}
