package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/watch"
)

// readPart is the rig's ReadPart: it counts the read and runs readFn.
func (r *inviteRig) readPart(ctx context.Context, c cache.InviteCandidate) (cache.FoundInvite, error) {
	r.reads.Add(1)
	if r.readFn == nil {
		return cache.FoundInvite{}, errors.New("the rig reads no part")
	}
	return r.readFn(ctx, c)
}

// queue hands cands to the hook's OnDeferredInvites, as the cache builder
// does, and fails the test if they are not queued.
func (r *inviteRig) queue(cands []cache.InviteCandidate) {
	r.t.Helper()
	if err := r.h.onDeferred(r.t.Context(), cands); err != nil {
		r.t.Fatalf("queueing deferred invite parts: %v", err)
	}
}

// fetches returns every queued invite fetch, due or not.
func (r *inviteRig) fetches() []calcache.InviteFetchRow {
	r.t.Helper()
	rows, err := r.cal.DueInviteFetches(r.t.Context(), r.now().Add(365*24*time.Hour), 1000)
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

// candidate is a deferred REQUEST part 2 of account's message msg.
func candidate(account, msg string) cache.InviteCandidate {
	return cache.InviteCandidate{
		Account: account, MessageRef: msg,
		Part: mailstore.CalendarPart{Part: "2", Method: "REQUEST", Charset: "utf-8", Size: 321},
	}
}

// reads returns a readFn that gives msg's REQUEST for uid.
func readsRequest(uid string) func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
	return func(_ context.Context, c cache.InviteCandidate) (cache.FoundInvite, error) {
		return found(c.Account, c.MessageRef, "REQUEST", request(uid)), nil
	}
}

// A part the builder deferred is queued in calendar.db with its account,
// message ref, part, method, charset and size, due at once. The retry
// pass reads it through ReadPart and runs the hook on it: the invite is
// stored and announced, the account poked, and the queued row removed.
func TestInviteFetchQueueStoresDeferredParts(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	r.queue([]cache.InviteCandidate{candidate("none", "msg-1")})
	rows := r.fetches()
	want := calcache.InviteFetchRow{
		Account: "none", MessageRef: "msg-1", Part: "2", Method: "REQUEST", Charset: "utf-8", Size: 321,
		NextAt: inviteNow, FoundAt: inviteNow,
	}
	if len(rows) != 1 || rows[0].Account != want.Account || rows[0].MessageRef != want.MessageRef || rows[0].Part != want.Part ||
		rows[0].Method != want.Method || rows[0].Charset != want.Charset || rows[0].Size != want.Size || rows[0].Attempts != 0 ||
		!rows[0].NextAt.Equal(want.NextAt) || !rows[0].FoundAt.Equal(want.FoundAt) {
		t.Fatalf("queued = %+v, want [%+v]", rows, want)
	}

	r.readFn = readsRequest("u1")
	r.h.pass(t.Context(), []string{"none"})
	if got := r.invites("none"); len(got) != 1 || got[0].UID != "u1" || got[0].MessageRef != "msg-1" {
		t.Errorf("stored = %+v, want u1 from msg-1", got)
	}
	if n := len(r.events()); n != 1 {
		t.Errorf("published %d invite events, want 1", n)
	}
	if p := r.pokesSoFar(); !slices.Equal(p, []string{"none"}) {
		t.Errorf("pokes = %q, want [none]", p)
	}
	if rows := r.fetches(); len(rows) != 0 {
		t.Errorf("queued = %+v after the read, want none", rows)
	}
}

// A part that cannot be read yet (a busy pool) stays queued with its next
// try put off, and is not read before then; the pass after the backoff
// reads and stores it.
func TestInviteFetchQueueBacksOffABusyPool(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	r.queue([]cache.InviteCandidate{candidate("none", "msg-1")})
	r.readFn = func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
		return cache.FoundInvite{}, mailstore.ErrBusy
	}
	r.h.pass(t.Context(), []string{"none"})
	rows := r.fetches()
	if len(rows) != 1 || rows[0].Attempts != 1 || !rows[0].NextAt.Equal(inviteNow.Add(inviteFetchBackoffBase)) {
		t.Fatalf("queued = %+v, want one row with 1 attempt, next at %v", rows, inviteNow.Add(inviteFetchBackoffBase))
	}
	r.h.pass(t.Context(), []string{"none"})
	if n := r.reads.Load(); n != 1 {
		t.Errorf("read %d times before the backoff ended, want 1", n)
	}

	r.advance(inviteFetchBackoffBase)
	r.readFn = readsRequest("u1")
	r.h.pass(t.Context(), []string{"none"})
	if got := r.invites("none"); len(got) != 1 || got[0].UID != "u1" {
		t.Errorf("stored = %+v, want u1", got)
	}
	if rows := r.fetches(); len(rows) != 0 {
		t.Errorf("queued = %+v after the read, want none", rows)
	}
}

// A part whose message or folder is gone, that is over the size limit, that
// does not decode (a bad transfer encoding, or a form the server sends
// that chainmail cannot read), or that is no usable invite (not iCalendar,
// no invite, too large to parse) is dropped from the queue, with nothing
// stored: no later read changes any of those.
func TestInviteFetchQueueDropsWhatCannotBeRead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		read func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error)
	}{
		{"gone", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, fmt.Errorf("%w: message", mailstore.ErrNotFound)
		}},
		{"stale", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, mailstore.ErrStaleRef
		}},
		{"too large", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, cache.ErrInviteTooLarge
		}},
		{"not an invite", func(_ context.Context, c cache.InviteCandidate) (cache.FoundInvite, error) {
			return found(c.Account, c.MessageRef, "REQUEST", []byte("not a calendar")), nil
		}},
		{"bad transfer encoding", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, cache.ErrInviteUndecodable
		}},
		{"malformed", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, fmt.Errorf("%w: body", mailstore.ErrMalformed)
		}},
		{"invalid ics", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, fmt.Errorf("reading: %w", calendar.ErrInvalidICS)
		}},
		{"not an invite error", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, calendar.ErrNotInvite
		}},
		{"ics too large", func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
			return cache.FoundInvite{}, calendar.ErrTooLarge
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newInviteRig(t)
			r.queue([]cache.InviteCandidate{candidate("none", "msg-1")})
			r.readFn = tt.read
			r.h.pass(t.Context(), []string{"none"})
			if rows := r.fetches(); len(rows) != 0 {
				t.Errorf("queued = %+v, want the part dropped", rows)
			}
			if got := r.invites("none"); len(got) != 0 {
				t.Errorf("stored = %+v, want nothing", got)
			}
		})
	}
}

// The backoff after a failed read starts at five minutes and doubles up to
// six hours. A row is never dropped for its attempts.
func TestInviteFetchBackoff(t *testing.T) {
	t.Parallel()
	want := []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute,
		160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour,
	}
	for i, w := range want {
		if got := inviteFetchBackoff(i + 1); got != w {
			t.Errorf("inviteFetchBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := inviteFetchBackoff(1000); got != inviteFetchBackoffMax {
		t.Errorf("inviteFetchBackoff(1000) = %v, want %v", got, inviteFetchBackoffMax)
	}

	r := newInviteRig(t)
	r.queue([]cache.InviteCandidate{candidate("none", "msg-1")})
	r.readFn = func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
		return cache.FoundInvite{}, mailstore.ErrUnavailable
	}
	for range 12 {
		r.advance(inviteFetchBackoffMax)
		r.h.pass(t.Context(), []string{"none"})
	}
	if rows := r.fetches(); len(rows) != 1 || rows[0].Attempts != 12 {
		t.Errorf("queued = %+v, want the row kept after 12 failures", rows)
	}
}

// One pass reads at most inviteFetchBatch queued parts, the ones queued
// longest ago first.
func TestInviteFetchPassIsBounded(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	var cands []cache.InviteCandidate
	for i := range inviteFetchBatch + 1 {
		cands = append(cands, candidate("none", fmt.Sprintf("msg-%03d", i)))
	}
	r.queue(cands)
	var read []string
	r.readFn = func(_ context.Context, c cache.InviteCandidate) (cache.FoundInvite, error) {
		read = append(read, c.MessageRef)
		return cache.FoundInvite{}, mailstore.ErrBusy
	}
	r.h.pass(t.Context(), []string{"none"})
	if len(read) != inviteFetchBatch {
		t.Errorf("one pass read %d parts, want %d", len(read), inviteFetchBatch)
	}
}

// A fetch pass stops between parts once its context ends, and a read cut
// short by that does not count as an attempt.
func TestInviteFetchPassStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	r.queue([]cache.InviteCandidate{candidate("none", "msg-1"), candidate("none", "msg-2"), candidate("none", "msg-3")})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.readFn = func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
		cancel()
		return cache.FoundInvite{}, context.Canceled
	}
	r.h.pass(ctx, []string{"none"})
	if n := r.reads.Load(); n != 1 {
		t.Errorf("a cancelled pass read %d parts, want 1", n)
	}
	for _, row := range r.fetches() {
		if row.Attempts != 0 {
			t.Errorf("%s has %d attempts after a cancelled read, want 0", row.MessageRef, row.Attempts)
		}
	}
}

// Through the server: of 21 invites in one segment, the cache worker's
// OnInvites stores 20 and its OnDeferredInvites queues the 21st, which the
// retry pass reads from the mail store through the account's cache worker
// and stores, so all 21 end up in calendar.db.
func TestInviteFetchesThroughTheServer(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	s, _ := newCacheServerOptions(t, cacheConfig, Options{
		NewStore: func(config.ResolvedAccount) mailstore.Store { return fk },
		Now:      func() time.Time { return inviteNow },
	})
	r := newInviteRig(t)
	s.calcache = r.cal
	s.watchLog = r.log
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	opts := s.cacheAccountOptions(acct)
	if opts.OnInvites == nil || opts.OnDeferredInvites == nil {
		t.Fatal("the cache worker has no invite hooks with calendar.db open")
	}
	s.cacheAccounts = map[string]*cache.Account{"host": cache.NewAccount(opts)}

	var now []cache.FoundInvite
	for i := range 20 {
		now = append(now, found("host", fmt.Sprintf("msg-%02d", i), "REQUEST", request(fmt.Sprintf("u%02d", i))))
	}
	opts.OnInvites(t.Context(), now)
	data := request("u20")
	rf := fk.Add(fake.Message{
		Folder: "INBOX", Subject: "Planning", Date: inviteNow, Body: "invite",
		From:        []mailstore.Address{{Email: "ann@example.com"}},
		To:          []mailstore.Address{{Email: "me@example.com"}},
		Attachments: []fake.Attachment{{Part: "2", ContentType: "text/calendar; method=REQUEST; charset=utf-8", Data: data}},
	})
	if err := opts.OnDeferredInvites(t.Context(), []cache.InviteCandidate{{
		Account: "host", MessageRef: rf.String(),
		Part: mailstore.CalendarPart{Part: "2", Method: "REQUEST", Charset: "utf-8", Size: int64(len(data))},
	}}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.invites("host")); n != 20 {
		t.Fatalf("stored %d invites before the retry pass, want 20", n)
	}
	s.serverInviteHook(s.calcache, s.pokeCalendar).pass(t.Context(), []string{"host"})
	if n := len(r.invites("host")); n != 21 {
		t.Errorf("stored %d invites after the retry pass, want 21", n)
	}
	if rows := r.fetches(); len(rows) != 0 {
		t.Errorf("queued = %+v, want none", rows)
	}
	if n := fk.BackgroundCalls("StreamPart"); n != 1 {
		t.Errorf("background part reads = %d, want 1", n)
	}
}

// A failure that may pass keeps the part queued with its next read put
// off: the server unavailable, a busy pool, a login needed or refused, the
// account's cache not running, or an error nothing classifies.
func TestInviteFetchQueueKeepsWhatMayPass(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"unavailable": fmt.Errorf("%w: connection reset", mailstore.ErrUnavailable),
		"busy":        mailstore.ErrBusy,
		"login":       auth.ErrAuthRequired,
		"rejected":    auth.ErrRejected,
		"no cache":    errInviteNoCache,
		"unknown":     errors.New("something else"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newInviteRig(t)
			r.queue([]cache.InviteCandidate{candidate("none", "msg-1")})
			r.readFn = func(context.Context, cache.InviteCandidate) (cache.FoundInvite, error) {
				return cache.FoundInvite{}, err
			}
			r.h.pass(t.Context(), []string{"none"})
			if rows := r.fetches(); len(rows) != 1 || rows[0].Attempts != 1 {
				t.Errorf("queued = %+v, want the part kept with 1 attempt", rows)
			}
		})
	}
}

// A queue write that fails is returned to the cache builder, which then
// applies the segment again: the immediate invites a second time (one
// event, however often) and the deferred part, now queued, which the retry
// pass stores and announces.
func TestInviteFetchQueueWriteFailureIsReturned(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	ctx := t.Context()
	conn, err := sql.Open("sqlite", "file:"+r.calPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	if _, err := conn.ExecContext(ctx, "CREATE TRIGGER queue_down BEFORE INSERT ON invite_fetches BEGIN SELECT RAISE(ABORT, 'queue down'); END"); err != nil {
		t.Fatal(err)
	}
	segment := func() error {
		r.hook(ctx, []cache.FoundInvite{found("none", "msg-1", "REQUEST", request("u1"))})
		return r.h.onDeferred(ctx, []cache.InviteCandidate{candidate("none", "msg-2")})
	}
	if err := segment(); err == nil {
		t.Fatal("onDeferred = nil with calendar.db refusing the queue write, want the error")
	}
	if rows := r.fetches(); len(rows) != 0 {
		t.Fatalf("queued = %+v after a failed write, want none", rows)
	}
	if _, err := conn.ExecContext(ctx, "DROP TRIGGER queue_down"); err != nil {
		t.Fatal(err)
	}
	if err := segment(); err != nil {
		t.Fatalf("onDeferred on the replay = %v, want nil", err)
	}
	if rows := r.fetches(); len(rows) != 1 || rows[0].MessageRef != "msg-2" {
		t.Fatalf("queued = %+v after the replay, want msg-2", rows)
	}
	if n := len(r.events()); n != 1 {
		t.Fatalf("published %d invite events after the replay, want u1's once", n)
	}

	r.readFn = readsRequest("u2")
	r.h.pass(ctx, []string{"none"})
	if got := r.rowOf("none", "msg-2", "u2"); got.UID != "u2" {
		t.Errorf("stored = %+v, want u2 from msg-2", got)
	}
	if n := len(r.events()); n != 2 {
		t.Errorf("published %d invite events, want u1's and u2's", n)
	}
}

// foundAtOf returns the found_at stored for account's message msg and
// uid, read through a second connection to calendar.db, since the store
// reads only each UID's latest row.
func (r *inviteRig) foundAtOf(account, msg, uid string) time.Time {
	r.t.Helper()
	conn, err := sql.Open("sqlite", "file:"+r.calPath+"?mode=ro")
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // a read-only connection: nothing to flush
	var s string
	if err := conn.QueryRowContext(r.t.Context(), "SELECT found_at FROM mail_invites WHERE account = ? AND message_ref = ? AND uid = ?",
		account, msg, uid).Scan(&s); err != nil {
		r.t.Fatal(err)
	}
	at, err := watch.ParseTime(s)
	if err != nil {
		r.t.Fatal(err)
	}
	return at
}

// A deferred part keeps the time its message was found, the cache's, from
// the queue row through to the stored invite, not the time the retry pass
// read it. So an original REQUEST that the builder deferred still began
// its UID when a CANCEL of the UID was stored at once, and a later REQUEST
// with a higher SEQUENCE is announced.
func TestInviteDeferredRequestKeepsItsDiscoveryTime(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	ctx := t.Context()
	segmentAt := inviteNow.Add(-time.Minute)
	original := candidate("none", "msg-1")
	original.FoundAt = segmentAt
	r.queue([]cache.InviteCandidate{original})
	if rows := r.fetches(); len(rows) != 1 || !rows[0].FoundAt.Equal(segmentAt) || !rows[0].NextAt.Equal(inviteNow) {
		t.Fatalf("queued = %+v, want msg-1 found at %v, due at %v", rows, segmentAt, inviteNow)
	}

	cancelled := found("none", "msg-2", "CANCEL", inviteICS("CANCEL", "u1", 1, "ann@example.com", "20261110T170000Z", "20261110T173000Z"))
	cancelled.FoundAt = segmentAt.Add(time.Second)
	r.advance(time.Minute)
	r.hook(ctx, []cache.FoundInvite{cancelled})

	r.advance(5 * time.Minute)
	r.readFn = readsRequest("u1")
	r.h.pass(ctx, []string{"none"})
	if at := r.foundAtOf("none", "msg-1", "u1"); !at.Equal(segmentAt) {
		t.Errorf("msg-1 found at %v, want the queue row's %v", at, segmentAt)
	}
	if at := r.foundAtOf("none", "msg-2", "u1"); !at.Equal(cancelled.FoundAt) {
		t.Errorf("msg-2 found at %v, want the invite's %v", at, cancelled.FoundAt)
	}
	if n := len(r.events()); n != 0 {
		t.Fatalf("published %d invite events for a cancelled UID, want none", n)
	}

	r.advance(time.Hour)
	again := found("none", "msg-3", "REQUEST", inviteICS("REQUEST", "u1", 2, "ann@example.com", "20261110T170000Z", "20261110T173000Z"))
	r.hook(ctx, []cache.FoundInvite{again})
	evs := r.events()
	if len(evs) != 1 || evs[0].Calendar == nil || evs[0].Calendar.MessageRef != "msg-3" {
		t.Fatalf("events = %+v, want one invite event from msg-3", evs)
	}
	if at := r.foundAtOf("none", "msg-3", "u1"); !at.Equal(r.now()) {
		t.Errorf("msg-3 found at %v, want the hook's clock %v for an invite without a time", at, r.now())
	}
}
