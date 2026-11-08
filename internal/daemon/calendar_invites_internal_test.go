package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	r.hook = newInviteHook(inviteHookOptions{
		Cache:     cal,
		Log:       r.log,
		Poke:      r.poke,
		StoreNone: func(account string) bool { return account == "none" },
		Self:      func(addr string) bool { return strings.EqualFold(addr, "me@example.com") },
		Now:       func() time.Time { return inviteNow },
	}).onInvites
	return r
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
