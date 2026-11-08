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
