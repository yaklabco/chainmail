package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
)

// testInviteICS is a small iTIP REQUEST.
const testInviteICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nMETHOD:REQUEST\r\n" +
	"BEGIN:VEVENT\r\nUID:inv-1@example.com\r\nDTSTAMP:20261101T120000Z\r\n" +
	"DTSTART:20261110T170000Z\r\nDTEND:20261110T173000Z\r\nSUMMARY:Sync\r\n" +
	"ORGANIZER:mailto:ann@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:me@example.com\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// inviteSink records what OnInvites receives.
type inviteSink struct {
	mu    sync.Mutex
	calls int
	got   []FoundInvite
}

func (s *inviteSink) on(_ context.Context, invites []FoundInvite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.got = append(s.got, invites...)
}

func (s *inviteSink) invites() []FoundInvite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FoundInvite(nil), s.got...)
}

// calMsg is acctMsg with a calendar part of content type ct holding data.
func calMsg(name, ct string, data []byte) fake.Message {
	m := acctMsg("INBOX", name)
	m.Attachments = []fake.Attachment{{Part: "2", ContentType: ct, Data: data}}
	return m
}

// inviteRig is an acctRig on one INBOX whose Accounts hand found invites
// to the returned sink, started and caught up on the empty folder.
func inviteRig(t *testing.T, fs *fake.Store) (*acctRig, *inviteSink) {
	t.Helper()
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	sink := &inviteSink{}
	r.onInvites = sink.on
	r.renew()
	return r, sink
}

// A new message with a calendar part of method REQUEST is read once, and
// OnInvites gets it with the part, the message ref and the decoded bytes.
// The bytes count against the day's budget beside the header. Parts with
// another method, without one, or over calendar.MaxICSBytes are not read,
// nor are messages without calendar parts. A message with two invite
// parts gives the first.
func TestBuilderFindsInvites(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	data := []byte(testInviteICS)
	invite := fs.Add(calMsg("invite", "text/calendar; method=request; charset=UTF-8", data))
	fs.Add(calMsg("reply", "text/calendar; method=REPLY", data))
	fs.Add(calMsg("plain-ics", "application/ics; name=invite.ics", data))
	fs.Add(calMsg("huge", "text/calendar; method=REQUEST", bytes.Repeat([]byte("x"), calendar.MaxICSBytes)))
	fs.Add(acctMsg("INBOX", "no-calendar"))
	two := acctMsg("INBOX", "two")
	two.Attachments = []fake.Attachment{
		{Part: "2", ContentType: "text/calendar; method=CANCEL", Data: data},
		{Part: "3", ContentType: "application/ics; method=CANCEL", Data: data},
	}
	twoRef := fs.Add(two)
	r.pass("INBOX")
	r.waitApplied()

	got := sink.invites()
	if len(got) != 2 {
		t.Fatalf("found %d invites, want 2: %+v", len(got), got)
	}
	want := []struct {
		rf     ref.Ref
		part   string
		method string
	}{{invite, "2", "REQUEST"}, {twoRef, "2", "CANCEL"}}
	for i, w := range want {
		g := got[i]
		if g.Account != "acme" || g.MessageRef != w.rf.String() || g.Part.Part != w.part || g.Part.Method != w.method {
			t.Errorf("invite %d = %+v, want account acme, message %s, part %s, method %s", i, g, w.rf, w.part, w.method)
		}
		if !bytes.Equal(g.Data, data) {
			t.Errorf("invite %d data = %q, want %q", i, g.Data, data)
		}
	}
	if got[0].Part.Charset != "utf-8" {
		t.Errorf("charset = %q, want utf-8", got[0].Part.Charset)
	}
	if n := fs.Calls("StreamPart"); n != 2 {
		t.Errorf("StreamPart ran %d times, want 2", n)
	}
	wantBudget := 6*headerBudgetBytes + 2*int64(len(data))
	if b := r.budget("2026-11-02"); b != wantBudget {
		t.Errorf("budget = %d, want %d for six headers and two invites", b, wantBudget)
	}
}

// The transfer encoding of a part in a message as received is decoded.
func TestBuilderDecodesInviteParts(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	enc := base64.StdEncoding.EncodeToString([]byte(testInviteICS))
	raw := "From: Ann <ann@example.com>\r\nTo: me@example.com\r\nSubject: Sync\r\n" +
		"Message-ID: <raw@example.com>\r\nDate: Mon, 02 Nov 2026 08:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nYou are invited.\r\n" +
		"--b1\r\nContent-Type: text/calendar; charset=utf-8; method=REQUEST\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		enc + "\r\n--b1--\r\n"
	rf, err := fs.Append(t.Context(), "INBOX", nil, acctStart, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	r.pass("INBOX")
	r.waitApplied()

	got := sink.invites()
	if len(got) != 1 {
		t.Fatalf("found %d invites, want 1", len(got))
	}
	if got[0].MessageRef != rf.String() || got[0].Part.Part != "2" || got[0].Part.Method != "REQUEST" {
		t.Errorf("invite = %+v, want message %s, part 2, method REQUEST", got[0], rf)
	}
	if !bytes.Equal(got[0].Data, []byte(testInviteICS)) {
		t.Errorf("data = %q, want the decoded calendar", got[0].Data)
	}
}

// A part that fails while streaming is skipped and logged: the message is
// cached, its event acknowledged, OnInvites not called, and the builder
// records no error.
func TestBuilderInviteStreamFailureKeepsBuilding(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	fs.FailStreamAfter(10, mailstore.ErrUnavailable)
	rf := fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	r.pass("INBOX")
	r.waitApplied()

	if _, ok := r.row(rf); !ok {
		t.Error("the message was not cached")
	}
	if got, tail := r.acked(), r.tail(); got != tail {
		t.Errorf("acked %d, want the tail %d", got, tail)
	}
	sink.mu.Lock()
	calls := sink.calls
	sink.mu.Unlock()
	if calls != 0 {
		t.Errorf("OnInvites ran %d times, want none", calls)
	}
	if e := r.a.lastError(); e != "" {
		t.Errorf("last error = %q, want none", e)
	}
}

// A part whose decoded bytes run past calendar.MaxICSBytes, though the
// server reported a smaller size, is dropped after MaxICSBytes + 1 bytes.
func TestBuilderDropsInvitesThatOutgrowTheLimit(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	lying := &lyingSizeMail{Store: fs, size: calendar.MaxICSBytes + 100}
	r.mail = lying
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	r.pass("INBOX")
	r.waitApplied()

	if got := sink.invites(); len(got) != 0 {
		t.Errorf("found %d invites, want none", len(got))
	}
	if n := lying.read.Load(); n != calendar.MaxICSBytes+1 {
		t.Errorf("read %d bytes, want %d", n, calendar.MaxICSBytes+1)
	}
}

// Without OnInvites the builder reads no calendar part, and mail that
// backfill fetches is never read for invites.
func TestBuilderInvitesOnlyOnNewMailWithAHook(t *testing.T) {
	t.Parallel()
	t.Run("no hook", func(t *testing.T) {
		t.Parallel()
		fs := fake.New("acme", false)
		r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
		r.pass("INBOX")
		r.start()
		r.waitApplied()
		rf := fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
		r.pass("INBOX")
		r.waitApplied()
		if _, ok := r.row(rf); !ok {
			t.Error("the message was not cached")
		}
		if n := fs.Calls("StreamPart"); n != 0 {
			t.Errorf("StreamPart ran %d times, want none", n)
		}
	})
	t.Run("backfill", func(t *testing.T) {
		t.Parallel()
		fs := fake.New("acme", false)
		r, sink := inviteRig(t, fs)
		old := fs.Add(calMsg("old", "text/calendar; method=REQUEST", []byte(testInviteICS)))
		r.pass("INBOX")
		r.start()
		r.waitBackfilled("INBOX")
		r.waitApplied()
		if _, ok := r.row(old); !ok {
			t.Fatal("backfill did not cache the message")
		}
		if got := sink.invites(); len(got) != 0 {
			t.Errorf("found %d invites in backfilled mail, want none", len(got))
		}
	})
}

// lyingSizeMail serves every calendar part as size bytes of data, more
// than the header said, and counts the bytes read.
type lyingSizeMail struct {
	*fake.Store
	size int
	read atomic.Int64
}

func (m *lyingSizeMail) StreamPart(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
	info, rc, err := m.Store.StreamPart(ctx, r, part)
	if err != nil {
		return info, nil, err
	}
	_ = rc.Close() // the fake's reader holds nothing
	return info, &countingReader{r: strings.NewReader(strings.Repeat("x", m.size)), n: &m.read}, nil
}

type countingReader struct {
	r *strings.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (*countingReader) Close() error { return nil }
