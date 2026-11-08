package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/watch"
)

// inviteNow is the hook's clock in these tests, before every test event.
var inviteNow = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// inviteICS is an iTIP message of method for uid at sequence seq, from
// organizer, on 2026-11-10 17:00 to 17:30 UTC unless end is set.
func inviteICS(method, uid string, seq int, organizer, start, end string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nMETHOD:" + method + "\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20261101T120000Z\r\nSEQUENCE:" + string(rune('0'+seq)) + "\r\n" +
		"DTSTART:" + start + "\r\nDTEND:" + end + "\r\nSUMMARY:Planning\r\n" +
		"ORGANIZER;CN=Ann:mailto:" + organizer + "\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:me@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n")
}

func request(uid string) []byte {
	return inviteICS("REQUEST", uid, 0, "ann@example.com", "20261110T170000Z", "20261110T173000Z")
}

func found(account, msg string, method string, data []byte) cache.FoundInvite {
	return cache.FoundInvite{
		Account: account, MessageRef: msg,
		Part: mailstore.CalendarPart{Part: "2", Method: method, Charset: "utf-8", Size: int64(len(data))},
		Data: data,
	}
}

// inviteRig is the invite hook over a real calendar.db and state.db, with
// the accounts "none" (store none) and "cal" (a calendar store).
type inviteRig struct {
	t     *testing.T
	cal   *calcache.Store
	log   *watch.Log
	mu    sync.Mutex
	pokes []string
	hook  func(ctx context.Context, invites []cache.FoundInvite)
	// h is the hook behind hook. failing counts the publishes still to
	// fail, and publishes every publish tried.
	h         *inviteHook
	failing   atomic.Int32
	publishes atomic.Int32
	// nowNS is the hook's clock, inviteNow unless a test moves it.
	nowNS atomic.Int64
	// readFn is the hook's ReadPart; set it before the hook reads a
	// deferred part. reads counts the reads.
	readFn func(ctx context.Context, c cache.InviteCandidate) (cache.FoundInvite, error)
	reads  atomic.Int32
}

func newInviteRig(t *testing.T) *inviteRig {
	t.Helper()
	dir := t.TempDir()
	stateDB, err := db.OpenState(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() }) // a close error at test end changes nothing
	cal, err := calcache.Open(t.Context(), filepath.Join(dir, calcache.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cal.Close() }) // a close error at test end changes nothing
	r := &inviteRig{t: t, cal: cal, log: watch.NewLog(stateDB, func() time.Time { return inviteNow }, nil)}
	r.nowNS.Store(inviteNow.UnixNano())
	r.h = newInviteHook(inviteHookOptions{
		Cache:     cal,
		Log:       r.log,
		Publish:   r.publish,
		Poke:      r.poke,
		StoreNone: func(account string) bool { return account == "none" },
		Self:      func(addr string) bool { return strings.EqualFold(addr, "me@example.com") },
		Now:       r.now,
		ReadPart:  r.readPart,
	})
	r.hook = r.h.onInvites
	return r
}

// errPublish is the failure of a publish the rig fails.
var errPublish = errors.New("publish failed")

// publish is the hook's Publish: it fails while failing is above zero,
// counting it down, and otherwise publishes to the rig's log.
func (r *inviteRig) publish(ctx context.Context, e watch.Event) error {
	r.publishes.Add(1)
	if r.failing.Add(-1) >= 0 {
		return errPublish
	}
	r.failing.Store(0)
	return r.log.Publish(ctx, e)
}

func (r *inviteRig) poke(account string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pokes = append(r.pokes, account)
}

func (r *inviteRig) pokesSoFar() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pokes)
}

// events returns every invite event in the log.
func (r *inviteRig) events() []watch.Event {
	r.t.Helper()
	page, err := r.log.Read(r.t.Context(), watch.Query{Filter: watch.Filter{Kinds: []watch.Kind{watch.KindInvite}}, Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Events
}

func (r *inviteRig) invites(account string) []calcache.MailInviteRow {
	r.t.Helper()
	rows, err := r.cal.MailInvites(r.t.Context(), account)
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

// An invite found in mail is stored as a mail invite row with its message
// ref, method, sequence, DTSTAMP and data, and the account's syncer is
// poked. Only on a store none account does a new REQUEST UID publish an
// invite event, with the ce_ ref, the message ref, the times and the
// organizer and no event text.
func TestInviteHookStoresPokesAndPublishesForStoreNone(t *testing.T) {
	t.Parallel()
	for _, account := range []string{"none", "cal"} {
		t.Run(account, func(t *testing.T) {
			t.Parallel()
			r := newInviteRig(t)
			data := request("u1@example.com")
			r.hook(t.Context(), []cache.FoundInvite{found(account, "msg-1", "REQUEST", data)})

			rows := r.invites(account)
			if len(rows) != 1 {
				t.Fatalf("stored %d invites, want 1", len(rows))
			}
			ref := calendar.MailInviteRef(account, "u1@example.com")
			got := rows[0]
			if got.Ref != ref || got.MessageRef != "msg-1" || got.UID != "u1@example.com" || got.Method != "REQUEST" ||
				!got.DTStamp.Equal(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)) || !got.FoundAt.Equal(inviteNow) {
				t.Errorf("row = %+v, want ref %s, msg-1, the UID, REQUEST, the DTSTAMP, found now", got, ref)
			}
			if !bytes.Equal(got.Raw, data) {
				t.Errorf("raw = %q, want the part's data", got.Raw)
			}
			if p := r.pokesSoFar(); !slices.Equal(p, []string{account}) {
				t.Errorf("pokes = %q, want [%s]", p, account)
			}

			evs := r.events()
			if account != "none" {
				if len(evs) != 0 {
					t.Errorf("published %d invite events for a calendar store account, want none", len(evs))
				}
				return
			}
			if len(evs) != 1 {
				t.Fatalf("published %d invite events, want 1", len(evs))
			}
			e := evs[0]
			want := watch.CalendarChange{
				Ref: ref, MessageRef: "msg-1", Start: "2026-11-10T17:00:00Z", End: "2026-11-10T17:30:00Z", Organizer: "ann@example.com",
			}
			if e.Account != account || e.Ref != ref || e.Folder != "" || e.Calendar == nil || !calendarChangeEqual(*e.Calendar, want) {
				t.Errorf("event = %+v %+v, want account %s, ref %s, payload %+v", e, e.Calendar, account, ref, want)
			}
		})
	}
}

func calendarChangeEqual(a, b watch.CalendarChange) bool {
	return a.Ref == b.Ref && a.MessageRef == b.MessageRef && a.CalendarID == b.CalendarID && a.Start == b.Start &&
		a.End == b.End && a.Organizer == b.Organizer && slices.Equal(a.Fields, b.Fields) && a.Attendee == b.Attendee && a.PartStat == b.PartStat
}

// A UID already stored publishes nothing more, whether the new message is
// another REQUEST or an UPDATE; a CANCEL never publishes; and an invite
// James organizes, or one that has ended, is stored without an event.
func TestInviteHookPublishesOnlyNewRequestsNeedingAReply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		batch  [][]cache.FoundInvite
		events int
		rows   int
	}{
		{
			name: "repeat request in a later batch",
			batch: [][]cache.FoundInvite{
				{found("none", "msg-1", "REQUEST", request("u1"))},
				{found("none", "msg-2", "REQUEST", inviteICS("REQUEST", "u1", 1, "ann@example.com", "20261110T180000Z", "20261110T183000Z"))},
			},
			events: 1, rows: 1,
		},
		{
			name: "two parts of one UID in one batch",
			batch: [][]cache.FoundInvite{{
				found("none", "msg-1", "REQUEST", request("u1")),
				found("none", "msg-2", "REQUEST", request("u1")),
			}},
			events: 1, rows: 1,
		},
		{
			name:  "cancel",
			batch: [][]cache.FoundInvite{{found("none", "msg-1", "CANCEL", inviteICS("CANCEL", "u1", 1, "ann@example.com", "20261110T170000Z", "20261110T173000Z"))}},
			rows:  1,
		},
		{
			name:  "update of a UID never seen",
			batch: [][]cache.FoundInvite{{found("none", "msg-1", "UPDATE", inviteICS("UPDATE", "u1", 1, "ann@example.com", "20261110T170000Z", "20261110T173000Z"))}},
			rows:  1,
		},
		{
			name:  "own organizer",
			batch: [][]cache.FoundInvite{{found("none", "msg-1", "REQUEST", inviteICS("REQUEST", "u1", 0, "ME@example.com", "20261110T170000Z", "20261110T173000Z"))}},
			rows:  1,
		},
		{
			name:  "ended",
			batch: [][]cache.FoundInvite{{found("none", "msg-1", "REQUEST", inviteICS("REQUEST", "u1", 0, "ann@example.com", "20261101T170000Z", "20261101T173000Z"))}},
			rows:  1,
		},
		{
			name: "two UIDs",
			batch: [][]cache.FoundInvite{{
				found("none", "msg-1", "REQUEST", request("u1")),
				found("none", "msg-2", "REQUEST", request("u2")),
			}},
			events: 2, rows: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newInviteRig(t)
			for _, b := range tt.batch {
				r.hook(t.Context(), b)
			}
			if n := len(r.events()); n != tt.events {
				t.Errorf("published %d invite events, want %d", n, tt.events)
			}
			if n := len(r.invites("none")); n != tt.rows {
				t.Errorf("stored %d UIDs, want %d", n, tt.rows)
			}
		})
	}
}

// Data that is not an invite, or does not parse, stores nothing, pokes
// nothing and publishes nothing. A batch's good invites still land.
func TestInviteHookSkipsWhatDoesNotParse(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	reply := inviteICS("REPLY", "u9", 0, "ann@example.com", "20261110T170000Z", "20261110T173000Z")
	r.hook(t.Context(), []cache.FoundInvite{
		found("none", "msg-1", "REQUEST", []byte("not a calendar")),
		found("none", "msg-2", "REQUEST", reply),
		found("none", "msg-3", "REQUEST", request("u1")),
	})
	rows := r.invites("none")
	if len(rows) != 1 || rows[0].MessageRef != "msg-3" {
		t.Errorf("stored %+v, want only msg-3's invite", rows)
	}
	if n := len(r.events()); n != 1 {
		t.Errorf("published %d invite events, want 1", n)
	}

	r2 := newInviteRig(t)
	r2.hook(t.Context(), []cache.FoundInvite{found("cal", "msg-1", "REQUEST", []byte("not a calendar"))})
	if p := r2.pokesSoFar(); len(p) != 0 {
		t.Errorf("pokes = %q, want none", p)
	}
}

// A part in a single-byte charset is stored as UTF-8, so a reader parses
// the row without the charset.
func TestInviteHookStoresUTF8(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	data := bytes.Replace(request("u1"), []byte("Planning"), []byte("Caf\xe9"), 1)
	f := found("cal", "msg-1", "REQUEST", data)
	f.Part.Charset = "iso-8859-1"
	r.hook(t.Context(), []cache.FoundInvite{f})
	rows := r.invites("cal")
	if len(rows) != 1 {
		t.Fatalf("stored %d invites, want 1", len(rows))
	}
	inv, err := calendar.ParseInvite(rows[0].Raw, "")
	if err != nil {
		t.Fatalf("ParseInvite of the stored row: %v", err)
	}
	if inv.Events[0].Summary != "Café" {
		t.Errorf("summary = %q, want Café", inv.Events[0].Summary)
	}
}

// Without a calendar cache there is no hook, so the cache workers read no
// calendar part; nil dependencies otherwise are safe.
func TestInviteHookNeedsACalendarCache(t *testing.T) {
	t.Parallel()
	if h := newInviteHook(inviteHookOptions{}); h != nil {
		t.Error("newInviteHook without a cache returned a hook")
	}
	s := &Server{}
	if f := s.calendarInviteHook(nil, nil); f != nil {
		t.Error("calendarInviteHook without a cache returned a hook")
	}
	r := newInviteRig(t)
	h := newInviteHook(inviteHookOptions{Cache: r.cal})
	h.onInvites(t.Context(), []cache.FoundInvite{found("none", "msg-1", "REQUEST", request("u1"))})
	if n := len(r.invites("none")); n != 1 {
		t.Errorf("stored %d invites with nil dependencies, want 1", n)
	}
}

// The server's hook takes store none from each account's resolved
// calendar table and own addresses from the policy: an invite on the
// store none account is announced, one on the caldav account is not, and
// one James organizes is not either. Both accounts are poked.
func TestCalendarInviteHookUsesTheServerConfig(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, passwordConfig+`
[accounts.dav]
provider = "hostinger"
address = "dav@example.com"

[accounts.dav.calendar]
store = "caldav"
url = "https://dav.example.com/"
`)
	r := newInviteRig(t)
	s.watchLog = r.log
	s.opts.Now = func() time.Time { return inviteNow }
	hook := s.calendarInviteHook(r.cal, r.poke)
	if hook == nil {
		t.Fatal("no hook with a calendar cache")
	}
	own := inviteICS("REQUEST", "u2", 0, "me@example.com", "20261110T170000Z", "20261110T173000Z")
	hook(t.Context(), []cache.FoundInvite{
		found("host", "msg-1", "REQUEST", request("u1")),
		found("host", "msg-2", "REQUEST", own),
		found("dav", "msg-3", "REQUEST", request("u3")),
	})
	evs := r.events()
	if len(evs) != 1 || evs[0].Account != "host" || evs[0].Ref != calendar.MailInviteRef("host", "u1") {
		t.Errorf("events = %+v, want one for host's u1", evs)
	}
	if p := r.pokesSoFar(); !slices.Equal(p, []string{"host", "dav"}) {
		t.Errorf("pokes = %q, want [host dav]", p)
	}
	if !s.calendarStoreNone("host") || s.calendarStoreNone("dav") || s.calendarStoreNone("nobody") {
		t.Error("calendarStoreNone: want host true, dav false, an unknown account false")
	}
}

// latin1Invite is a REQUEST for uid in iso-8859-1 whose DESCRIPTION holds
// n bytes of \xe9, folded at 74 octets, so it grows by n bytes in UTF-8.
func latin1Invite(uid string, n int) []byte {
	var b bytes.Buffer
	b.WriteString(strings.TrimSuffix(string(request(uid)), "END:VEVENT\r\nEND:VCALENDAR\r\n"))
	line := []byte("DESCRIPTION:")
	for range n {
		if len(line) == 74 {
			b.Write(line)
			b.WriteString("\r\n")
			line = []byte(" ")
		}
		line = append(line, 0xe9)
	}
	b.Write(line)
	b.WriteString("\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	return b.Bytes()
}

// A single-byte part whose UTF-8 form would outgrow calendar.MaxICSBytes
// is stored as the part's own bytes with their charset, so the row reads
// back with calendar.ParseInvite(raw, charset), and the event still goes
// out for a store none account.
func TestInviteHookStoresThePartWhenUTF8WouldOutgrowTheLimit(t *testing.T) {
	t.Parallel()
	const n = 600_000
	data := latin1Invite("u1", n)
	if len(data) > calendar.MaxICSBytes || len(data)+n <= calendar.MaxICSBytes {
		t.Fatalf("test data: %d bytes, %d in UTF-8; want at most %d, then over it", len(data), len(data)+n, calendar.MaxICSBytes)
	}
	r := newInviteRig(t)
	f := found("none", "msg-1", "REQUEST", data)
	f.Part.Charset = "iso-8859-1"
	r.hook(t.Context(), []cache.FoundInvite{f})

	rows := r.invites("none")
	if len(rows) != 1 {
		t.Fatalf("stored %d invites, want 1", len(rows))
	}
	if rows[0].Charset != "iso-8859-1" || !bytes.Equal(rows[0].Raw, data) {
		t.Errorf("row charset %q, %d raw bytes; want iso-8859-1 and the part's %d bytes", rows[0].Charset, len(rows[0].Raw), len(data))
	}
	inv, err := calendar.ParseInvite(rows[0].Raw, rows[0].Charset)
	if err != nil {
		t.Fatalf("ParseInvite of the stored row: %v", err)
	}
	if d := inv.Events[0].Description; d != strings.Repeat("é", n) {
		t.Errorf("description has %d bytes, want %d é", len(d), n)
	}
	if evs := r.events(); len(evs) != 1 {
		t.Errorf("published %d invite events, want 1", len(evs))
	}
}

// A publish that fails leaves the invite pending: the row is stored
// unannounced, and the next hook call publishes it and marks it announced,
// so later deliveries of the UID publish nothing more.
func TestInviteHookRetriesAFailedPublish(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	r.failing.Store(1)
	r.hook(t.Context(), []cache.FoundInvite{found("none", "msg-1", "REQUEST", request("u1"))})
	if n := len(r.events()); n != 0 {
		t.Fatalf("published %d invite events with a failing log, want none", n)
	}
	rows := r.invites("none")
	if len(rows) != 1 || rows[0].Announced {
		t.Fatalf("rows = %+v, want one unannounced row", rows)
	}

	r.hook(t.Context(), []cache.FoundInvite{found("none", "msg-2", "UPDATE", inviteICS("UPDATE", "u2", 1, "ann@example.com", "20261110T170000Z", "20261110T173000Z"))})
	evs := r.events()
	if len(evs) != 1 || evs[0].Ref != calendar.MailInviteRef("none", "u1") || evs[0].Calendar == nil || evs[0].Calendar.MessageRef != "msg-1" {
		t.Fatalf("events = %+v, want u1's invite from msg-1", evs)
	}
	for _, row := range r.invites("none") {
		if row.UID == "u1" && !row.Announced {
			t.Error("u1 is not marked announced after its publish")
		}
	}

	r.hook(t.Context(), []cache.FoundInvite{found("none", "msg-3", "REQUEST", request("u1"))})
	if n := len(r.events()); n != 1 {
		t.Errorf("published %d invite events after another delivery of u1, want 1", n)
	}
}

// tickClock is a watch.Clock whose After reports each wait on asked and
// fires when the test sends on fire.
type tickClock struct {
	asked chan time.Duration
	fire  chan time.Time
}

func (c *tickClock) Now() time.Time { return inviteNow }

func (c *tickClock) After(d time.Duration) <-chan time.Time {
	c.asked <- d
	return c.fire
}

// The retry loop announces pending invites when it starts and then every
// inviteRetryInterval, for the store none accounts it is given, until its
// context ends.
func TestInviteRetryLoopRecovers(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	r.failing.Store(2) // the hook call and the loop's first pass
	r.hook(t.Context(), []cache.FoundInvite{found("none", "msg-1", "REQUEST", request("u1"))})

	clk := &tickClock{asked: make(chan time.Duration), fire: make(chan time.Time)}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.h.retryLoop(ctx, clk, []string{"none", "cal"}) })
	defer func() {
		cancel()
		wg.Wait()
	}()

	if d := <-clk.asked; d != inviteRetryInterval {
		t.Errorf("the loop waits %v, want %v", d, inviteRetryInterval)
	}
	if n := r.publishes.Load(); n != 2 {
		t.Errorf("tried %d publishes after the first pass, want 2", n)
	}
	if n := len(r.events()); n != 0 {
		t.Fatalf("published %d events while the log fails, want none", n)
	}
	clk.fire <- inviteNow
	<-clk.asked
	if n := len(r.events()); n != 1 {
		t.Errorf("published %d invite events after the next pass, want 1", n)
	}
	clk.fire <- inviteNow
	<-clk.asked
	if n := len(r.events()); n != 1 {
		t.Errorf("published %d invite events after a third pass, want still 1", n)
	}
}

// startBackground runs the retry loop over the server's calendar cache,
// so an invite left unannounced before a restart goes out, and its stop
// joins the loop.
func TestStartBackgroundAnnouncesPendingInvites(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	r := newInviteRig(t)
	s.calcache = r.cal
	s.watchLog = r.log
	s.opts.Now = func() time.Time { return inviteNow }
	row := calcache.MailInviteRow{
		Account: "host", MessageRef: "msg-1", UID: "u1", Method: calendar.MethodRequest,
		DTStamp: inviteNow, Raw: request("u1"), FoundAt: inviteNow,
	}
	if err := r.cal.PutMailInvite(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	stop := s.startBackground(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for len(r.events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	evs := r.events()
	if len(evs) != 1 || evs[0].Account != "host" || evs[0].Ref != calendar.MailInviteRef("host", "u1") {
		t.Errorf("events = %+v, want host's u1 invite", evs)
	}
}

// now is the rig's clock.
func (r *inviteRig) now() time.Time { return time.Unix(0, r.nowNS.Load()).UTC() }

// advance moves the rig's clock on by d.
func (r *inviteRig) advance(d time.Duration) { r.nowNS.Add(int64(d)) }

// rowOf returns the stored row of account's message msg for uid.
func (r *inviteRig) rowOf(account, msg, uid string) calcache.MailInviteRow {
	r.t.Helper()
	got, err := r.cal.MailInvite(r.t.Context(), calendar.MailInviteRef(account, uid))
	if err != nil {
		r.t.Fatal(err)
	}
	if got.MessageRef != msg {
		r.t.Fatalf("latest row of %s is from %s, want %s", uid, got.MessageRef, msg)
	}
	return got
}

// A pending invite stays pending for as long as its meeting is still to
// come: a publish that failed is retried eight days later, for a meeting
// next month, however long the outage lasted.
func TestInviteHookRetriesAnUpcomingMeetingAfterAWeek(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	r.failing.Store(1)
	nextMonth := inviteICS("REQUEST", "u1", 0, "ann@example.com", "20261202T170000Z", "20261202T173000Z")
	r.hook(t.Context(), []cache.FoundInvite{found("none", "msg-1", "REQUEST", nextMonth)})
	if n := len(r.events()); n != 0 {
		t.Fatalf("published %d invite events with a failing log, want none", n)
	}
	r.advance(8 * 24 * time.Hour)
	r.h.announce(t.Context(), "none")
	evs := r.events()
	if len(evs) != 1 || evs[0].Ref != calendar.MailInviteRef("none", "u1") {
		t.Fatalf("events = %+v, want u1's invite after eight days", evs)
	}
	if row := r.rowOf("none", "msg-1", "u1"); !row.Announced {
		t.Error("u1 is not marked announced after its publish")
	}
}

// A pending invite that will never get an event (it ended, James
// organizes it, it is cancelled, or its stored data does not parse) is
// settled the first time it is looked at, so later passes do not read it
// again; a pending one whose publish failed is not.
func TestInviteHookSettlesInvitesThatNeedNoEvent(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	cancelled := bytes.Replace(request("u3"), []byte("SUMMARY:"), []byte("STATUS:CANCELLED\r\nSUMMARY:"), 1)
	r.failing.Store(1)
	r.hook(t.Context(), []cache.FoundInvite{
		found("none", "msg-1", "REQUEST", inviteICS("REQUEST", "u1", 0, "ann@example.com", "20261101T170000Z", "20261101T173000Z")),
		found("none", "msg-2", "REQUEST", inviteICS("REQUEST", "u2", 0, "me@example.com", "20261110T170000Z", "20261110T173000Z")),
		found("none", "msg-3", "REQUEST", cancelled),
		found("none", "msg-4", "REQUEST", request("u4")),
	})
	broken := calcache.MailInviteRow{
		Account: "none", MessageRef: "msg-5", UID: "u5", Method: calendar.MethodRequest,
		DTStamp: inviteNow, Raw: []byte("not a calendar"), FoundAt: inviteNow,
	}
	if err := r.cal.PutMailInvite(t.Context(), broken); err != nil {
		t.Fatal(err)
	}
	r.h.announce(t.Context(), "none")
	for _, w := range []struct {
		msg, uid string
		settled  bool
	}{{"msg-1", "u1", true}, {"msg-2", "u2", true}, {"msg-3", "u3", true}, {"msg-4", "u4", false}, {"msg-5", "u5", true}} {
		if row := r.rowOf("none", w.msg, w.uid); row.Settled != w.settled {
			t.Errorf("%s settled = %t, want %t", w.uid, row.Settled, w.settled)
		}
	}
	pending, err := r.cal.PendingInvites(t.Context(), "none", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %d rows after u4 was announced, want none", len(pending))
	}
	if n := len(r.events()); n != 1 {
		t.Errorf("published %d invite events, want u4's", n)
	}
}

// An invite stays actionable while any of its events is still to come: an
// override after the master's end keeps it due.
func TestInviteDueWhileAnyEventIsToCome(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:u1\r\nDTSTAMP:20261101T120000Z\r\nDTSTART:20261101T170000Z\r\nDTEND:20261101T173000Z\r\n" +
		"ORGANIZER:mailto:ann@example.com\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:u1\r\nRECURRENCE-ID:20261101T170000Z\r\nDTSTAMP:20261101T120000Z\r\n" +
		"DTSTART:20261120T170000Z\r\nDTEND:20261120T173000Z\r\nORGANIZER:mailto:ann@example.com\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	inv, err := calendar.ParseInvite([]byte(ics), "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.h.due(inv, inviteNow) {
		t.Error("an invite with an override still to come is not due")
	}
	if r.h.due(inv, time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)) {
		t.Error("an invite whose every event has ended is due")
	}
}

// One announcing pass reads at most inviteAnnounceBatch pending invites,
// the ones first found longest ago first.
func TestInviteAnnouncePassIsBounded(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	for i := range inviteAnnounceBatch + 1 {
		uid := fmt.Sprintf("u%03d", i)
		row := calcache.MailInviteRow{
			Account: "none", MessageRef: "msg-" + uid, UID: uid, Method: calendar.MethodRequest,
			DTStamp: inviteNow, Raw: request(uid), FoundAt: inviteNow.Add(time.Duration(i) * time.Second),
		}
		if err := r.cal.PutMailInvite(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	r.failing.Store(1000)
	r.h.announce(t.Context(), "none")
	if n := r.publishes.Load(); n != inviteAnnounceBatch {
		t.Errorf("one pass tried %d publishes, want %d", n, inviteAnnounceBatch)
	}
}

// An announcing pass stops between rows once its context ends, so a
// shutdown does not wait for the rest of the batch.
func TestInviteAnnounceStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	r := newInviteRig(t)
	for _, uid := range []string{"u1", "u2", "u3"} {
		row := calcache.MailInviteRow{
			Account: "none", MessageRef: "msg-" + uid, UID: uid, Method: calendar.MethodRequest,
			DTStamp: inviteNow, Raw: request(uid), FoundAt: inviteNow,
		}
		if err := r.cal.PutMailInvite(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var tried atomic.Int32
	r.h.o.Publish = func(context.Context, watch.Event) error {
		tried.Add(1)
		cancel()
		return context.Canceled
	}
	r.h.announce(ctx, "none")
	if n := tried.Load(); n != 1 {
		t.Errorf("a cancelled pass tried %d publishes, want 1", n)
	}
}
