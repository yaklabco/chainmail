package cache

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// inviteLogs gives r's Account a logger that writes to the returned
// buffer. Call it before start.
func inviteLogs(r *acctRig) *keepLogs {
	logs := &keepLogs{}
	r.a.logger = slog.New(slog.NewTextHandler(logs, nil))
	return logs
}

// The builder reads invite parts as background work
// (mailstore.BackgroundPartStreamer), so a pool whose connections
// foreground requests hold makes the read fail at once with ErrBusy
// instead of queueing beside them.
func TestBuilderReadsInvitesAsBackgroundWork(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	r.pass("INBOX")
	r.waitApplied()

	if got := sink.invites(); len(got) != 1 {
		t.Fatalf("found %d invites, want 1", len(got))
	}
	if n := fs.BackgroundCalls("StreamPart"); n != 1 {
		t.Errorf("background StreamPart calls = %d, want 1: the invite read must be admitted as background work", n)
	}
}

// When the pool stays busy for the whole invite step, the step gives up at
// its deadline: the message is cached, its event acknowledged, OnInvites
// not called, the builder records no error, and the part skipped is
// counted in a log line and not read again.
func TestBuilderProceedsWhenTheInviteStepTimesOut(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	r.a.inviteTimeout = 20 * time.Millisecond
	logs := inviteLogs(r)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	fs.Fail("StreamPart", mailstore.ErrBusy)
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
	if out := logs.String(); !strings.Contains(out, "cache ran out of time for invite parts and skipped them") || !strings.Contains(out, "count=1") {
		t.Errorf("logs = %q, want the skipped part counted", out)
	}
	reads := fs.Calls("StreamPart")
	fs.Fail("StreamPart", nil)
	fs.Add(acctMsg("INBOX", "plain"))
	r.pass("INBOX")
	r.waitApplied()
	if n := fs.Calls("StreamPart"); n != reads {
		t.Errorf("StreamPart ran %d more times on the next segment, want the skipped part left alone", n-reads)
	}
}

// A segment reads at most inviteCap invite parts; the rest are skipped and
// counted in a log line, and the next segment does not read them.
func TestBuilderCapsInvitePartsPerSegment(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	r.a.inviteCap = 1
	logs := inviteLogs(r)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	first := fs.Add(calMsg("one", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	fs.Add(calMsg("two", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	r.pass("INBOX")
	r.waitApplied()

	got := sink.invites()
	if len(got) != 1 || got[0].MessageRef != first.String() {
		t.Fatalf("found %d invites, want only the first message's", len(got))
	}
	if n := fs.Calls("StreamPart"); n != 1 {
		t.Errorf("StreamPart ran %d times, want 1", n)
	}
	if out := logs.String(); !strings.Contains(out, "cache skipped invite parts over the per-segment cap") || !strings.Contains(out, "count=1") {
		t.Errorf("logs = %q, want the capped part counted", out)
	}
}

// StreamPartBackground serves the lying part as StreamPart does, so the
// builder's background read sees it too.
func (m *lyingSizeMail) StreamPartBackground(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
	info, rc, err := m.Store.StreamPartBackground(ctx, r, part)
	if err != nil {
		return info, nil, err
	}
	_ = rc.Close() // the fake's reader holds nothing
	return info, &countingReader{r: strings.NewReader(strings.Repeat("x", m.size)), n: &m.read}, nil
}

// holdingMail is a fake whose background part stream takes cache.db's
// only connection, in a transaction, before it returns, so the budget
// charge after the read finds the database blocked.
type holdingMail struct {
	*fake.Store
	st *Store
	tx chan *sql.Tx
}

func (m *holdingMail) StreamPartBackground(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
	tx, err := m.st.db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	m.tx <- tx
	return m.Store.StreamPartBackground(ctx, r, part)
}

// The budget charge of an invite part runs detached from the step's
// cancellation but bounded by its own short timeout, as the read path's
// bookkeeping is: a cache.db write that cannot get the connection gives up
// within the timeout, and the part read is still returned.
func TestReadInviteBoundsABlockedBudgetCharge(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, _ := inviteRig(t, fs)
	hold := &holdingMail{Store: fs, st: r.store, tx: make(chan *sql.Tx, 1)}
	r.mail = hold
	r.a.keepTimeout = 20 * time.Millisecond
	logs := inviteLogs(r)
	rf := fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	c := inviteCandidate{ref: rf, part: mailstore.CalendarPart{Part: "2", Method: "REQUEST", Size: int64(len(testInviteICS))}}

	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, data, err := r.a.readInvite(t.Context(), c)
		done <- result{data, err}
	}()
	tx := <-hold.tx
	defer func() { _ = tx.Rollback() }() // releases the connection; nothing was written
	select {
	case got := <-done:
		if got.err != nil || string(got.data) != testInviteICS {
			t.Errorf("readInvite = %q, %v; want the part", got.data, got.err)
		}
	case <-time.After(lockGuard):
		_ = tx.Rollback()
		<-done
		t.Fatal("the budget charge of an invite part waited for a blocked cache.db past its timeout")
	}
	if out := logs.String(); !strings.Contains(out, "cache could not count an invite part against the budget") {
		t.Errorf("logs = %q, want the failed charge logged", out)
	}
}

// deferSink records what OnDeferredInvites receives.
type deferSink struct {
	mu  sync.Mutex
	got []InviteCandidate
}

func (s *deferSink) on(_ context.Context, cands []InviteCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, cands...)
	return nil
}

func (s *deferSink) candidates() []InviteCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]InviteCandidate(nil), s.got...)
}

// deferRig is inviteRig whose Accounts also hand deferred invite parts to
// the returned sink.
func deferRig(t *testing.T, fs *fake.Store) (*acctRig, *inviteSink, *deferSink) {
	t.Helper()
	r, sink := inviteRig(t, fs)
	deferred := &deferSink{}
	r.onDeferred = deferred.on
	r.renew()
	return r, sink, deferred
}

// readDeferred reads each deferred part with ReadInvite and returns the
// invites read.
func readDeferred(t *testing.T, a *Account, cands []InviteCandidate) []FoundInvite {
	t.Helper()
	var out []FoundInvite
	for _, c := range cands {
		f, err := a.ReadInvite(t.Context(), c)
		if err != nil {
			t.Fatalf("ReadInvite(%s): %v", c.MessageRef, err)
		}
		out = append(out, f)
	}
	return out
}

// No invite part is lost to the per-segment cap: of 21 invites in one
// segment the step reads 20 and hands the 21st to OnDeferredInvites, with
// the account, message ref, part, method, charset and size, and
// ReadInvite reads it later. The step logs the deferral with a count.
func TestBuilderDefersInvitePartsOverTheCap(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink, deferred := deferRig(t, fs)
	logs := inviteLogs(r)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	data := []byte(testInviteICS)
	var refs []ref.Ref
	for i := range maxInvitesPerSegment + 1 {
		refs = append(refs, fs.Add(calMsg(fmt.Sprintf("invite-%02d", i), "text/calendar; method=REQUEST; charset=UTF-8", data)))
	}
	r.pass("INBOX")
	r.waitApplied()

	if got := sink.invites(); len(got) != maxInvitesPerSegment {
		t.Fatalf("found %d invites in the step, want %d", len(got), maxInvitesPerSegment)
	}
	cands := deferred.candidates()
	last := refs[len(refs)-1]
	want := InviteCandidate{
		Account: "acme", MessageRef: last.String(),
		Part: mailstore.CalendarPart{Part: "2", Method: "REQUEST", Charset: "utf-8"},
	}
	if len(cands) == 1 && cands[0].Part.Size > 0 {
		want.Part.Size = cands[0].Part.Size // the encoded size the header reported
	}
	if len(cands) != 1 || cands[0] != want {
		t.Fatalf("deferred = %+v, want [%+v]", cands, want)
	}
	if out := logs.String(); !strings.Contains(out, "cache deferred invite parts over the per-segment cap") || !strings.Contains(out, "count=1") {
		t.Errorf("logs = %q, want the deferred part counted", out)
	}
	later := readDeferred(t, r.a, cands)
	if len(later) != 1 || later[0].MessageRef != last.String() || later[0].Account != "acme" ||
		later[0].Part != want.Part || !bytes.Equal(later[0].Data, data) {
		t.Errorf("ReadInvite = %+v, want the 21st invite", later)
	}
	all := make(map[string]bool)
	for _, f := range append(sink.invites(), later...) {
		all[f.MessageRef] = true
	}
	if len(all) != len(refs) {
		t.Errorf("%d distinct invites read in all, want %d", len(all), len(refs))
	}
}

// A pool busy for the whole step defers the part rather than dropping it;
// ReadInvite fails at once with ErrBusy while the pool stays busy, and
// reads the part once a connection is idle.
func TestBuilderDefersInvitesWhenThePoolIsBusy(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink, deferred := deferRig(t, fs)
	r.a.inviteTimeout = 20 * time.Millisecond
	logs := inviteLogs(r)
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	fs.Fail("StreamPart", mailstore.ErrBusy)
	rf := fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
	r.pass("INBOX")
	r.waitApplied()

	if got := sink.invites(); len(got) != 0 {
		t.Fatalf("found %d invites with a busy pool, want none", len(got))
	}
	cands := deferred.candidates()
	if len(cands) != 1 || cands[0].MessageRef != rf.String() {
		t.Fatalf("deferred = %+v, want the invite of %s", cands, rf)
	}
	if out := logs.String(); !strings.Contains(out, "cache ran out of time for invite parts and deferred them") {
		t.Errorf("logs = %q, want the deferral logged", out)
	}
	before := fs.Calls("StreamPart")
	if _, err := r.a.ReadInvite(t.Context(), cands[0]); !errors.Is(err, mailstore.ErrBusy) {
		t.Errorf("ReadInvite on a busy pool = %v, want ErrBusy", err)
	}
	if n := fs.Calls("StreamPart") - before; n != 1 {
		t.Errorf("ReadInvite tried %d times on a busy pool, want once", n)
	}
	fs.Fail("StreamPart", nil)
	if got := readDeferred(t, r.a, cands); len(got) != 1 || !bytes.Equal(got[0].Data, []byte(testInviteICS)) {
		t.Errorf("ReadInvite = %+v, want the invite", got)
	}
}

// A transient stream failure defers the part; a message that is gone is
// dropped, not deferred.
func TestBuilderDefersInvitesAfterAStreamError(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		err     error
		deferIt bool
	}{
		{"unavailable", mailstore.ErrUnavailable, true},
		{"gone", mailstore.ErrNotFound, false},
		{"stale", mailstore.ErrStaleRef, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", false)
			r, sink, deferred := deferRig(t, fs)
			r.pass("INBOX")
			r.start()
			r.waitApplied()

			fs.Fail("StreamPart", tt.err)
			rf := fs.Add(calMsg("invite", "text/calendar; method=REQUEST", []byte(testInviteICS)))
			r.pass("INBOX")
			r.waitApplied()
			if _, ok := r.row(rf); !ok {
				t.Error("the message was not cached")
			}
			if got := sink.invites(); len(got) != 0 {
				t.Errorf("found %d invites, want none", len(got))
			}
			if n := len(deferred.candidates()); (n == 1) != tt.deferIt || n > 1 {
				t.Errorf("deferred %d parts, want deferred = %t", n, tt.deferIt)
			}
			if e := r.a.lastError(); e != "" {
				t.Errorf("last error = %q, want none", e)
			}
		})
	}
}

// ReadInvite reports a message that is gone as mailstore.ErrNotFound or
// mailstore.ErrStaleRef, a
// part over the limit as ErrInviteTooLarge, and refuses a part of another
// account's message as not found. It counts what it reads against the
// budget.
func TestReadInvite(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, _ := inviteRig(t, fs)
	data := []byte(testInviteICS)
	rf := fs.Add(calMsg("invite", "text/calendar; method=REQUEST", data))
	c := InviteCandidate{Account: "acme", MessageRef: rf.String(), Part: mailstore.CalendarPart{Part: "2", Method: "REQUEST", Size: int64(len(data))}}

	got, err := r.a.ReadInvite(t.Context(), c)
	if err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("ReadInvite = %+v, %v; want the part", got, err)
	}
	if b := r.budget(r.a.day()); b != int64(len(data)) {
		t.Errorf("budget = %d, want %d", b, len(data))
	}

	other := c
	other.Account = "beta"
	if _, err := r.a.ReadInvite(t.Context(), other); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("ReadInvite of another account's part = %v, want ErrNotFound", err)
	}

	r.mail = &lyingSizeMail{Store: fs, size: calendar.MaxICSBytes + 100}
	if _, err := r.a.ReadInvite(t.Context(), c); !errors.Is(err, ErrInviteTooLarge) {
		t.Errorf("ReadInvite of a part past the limit = %v, want ErrInviteTooLarge", err)
	}
	r.mail = fs

	if res := fs.Expunge(t.Context(), []ref.Ref{rf}); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	if _, err := r.a.ReadInvite(t.Context(), c); !errors.Is(err, mailstore.ErrNotFound) && !errors.Is(err, mailstore.ErrStaleRef) {
		t.Errorf("ReadInvite of an expunged message = %v, want ErrNotFound or ErrStaleRef", err)
	}
}

// errQueueDown is a queue write that fails.
var errQueueDown = errors.New("the invite queue is down")

// failingDeferSink records each OnDeferredInvites call, and fails the
// first fail of them with errQueueDown.
type failingDeferSink struct {
	mu    sync.Mutex
	fail  int
	calls [][]InviteCandidate
}

func (s *failingDeferSink) on(_ context.Context, cands []InviteCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slices.Clone(cands))
	if s.fail > 0 {
		s.fail--
		return errQueueDown
	}
	return nil
}

func (s *failingDeferSink) snapshot() [][]InviteCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// When OnDeferredInvites fails, the deferred parts are not queued, so the
// segment is not acknowledged: the step fails and backs off, and the same
// segment is applied again, which reads the immediate invites again (an
// idempotent upsert for the hook) and hands the deferred part over again.
func TestBuilderReplaysASegmentWhoseDeferredPartsWereNotQueued(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r, sink := inviteRig(t, fs)
	queue := &failingDeferSink{fail: 1}
	r.onDeferred = queue.on
	r.renew()
	r.a.inviteCap = 1
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	data := []byte(testInviteICS)
	first := fs.Add(calMsg("invite-1", "text/calendar; method=REQUEST; charset=UTF-8", data))
	second := fs.Add(calMsg("invite-2", "text/calendar; method=REQUEST; charset=UTF-8", data))
	r.pass("INBOX")
	r.clock.expectAfter(t, workRetryBase) // the builder's backoff after the failed queue write
	if got, tail := r.acked(), r.tail(); got >= tail {
		t.Fatalf("acked %d with the deferred part not queued, want below the tail %d", got, tail)
	}
	if calls := queue.snapshot(); len(calls) != 1 {
		t.Fatalf("OnDeferredInvites ran %d times before the backoff, want 1", len(calls))
	}
	r.clock.Advance(workRetryBase)
	r.waitApplied()

	calls := queue.snapshot()
	if len(calls) != 2 || len(calls[1]) != 1 || calls[1][0].MessageRef != second.String() {
		t.Fatalf("OnDeferredInvites calls = %+v, want the part of %s handed over again", calls, second)
	}
	if got, tail := r.acked(), r.tail(); got != tail {
		t.Errorf("acked %d after the replay, want the tail %d", got, tail)
	}
	if e := r.a.lastError(); e != "" {
		t.Errorf("last error = %q after the replay, want none", e)
	}
	found := sink.invites()
	if len(found) != 2 {
		t.Fatalf("OnInvites got %d invites, want the first message's twice", len(found))
	}
	for i, f := range found {
		if f.MessageRef != first.String() {
			t.Errorf("invite %d = %s, want %s", i, f.MessageRef, first)
		}
	}
}
