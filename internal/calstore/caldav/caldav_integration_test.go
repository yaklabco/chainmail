//go:build caldav

package caldav

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/testutil/mailwright"
)

func TestMain(m *testing.M) { os.Exit(mailwright.Main(m)) }

// deliveryWait bounds how long a scheduling message may take to reach an
// inbox. Mailwright delivers to local principals during the write, so this
// is slack, polled every 50ms.
const deliveryWait = 15 * time.Second

type principal struct {
	mailwright.Principal
	store *Store
}

func newPrincipal(t *testing.T, srv *mailwright.Server, name string) principal {
	t.Helper()
	p, err := srv.CreatePrincipal(name)
	if err != nil {
		t.Fatalf("%v\n%s", err, srv.Logs())
	}
	s, err := New(srv.HTTPClient(p), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	return principal{Principal: p, store: s}
}

// writableCalendar returns the principal's default calendar, or its first
// writable one.
func writableCalendar(t *testing.T, p principal) calendar.Calendar {
	t.Helper()
	cals, err := p.store.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars for %s: %v", p.Name, err)
	}
	var first *calendar.Calendar
	for i, c := range cals {
		if c.Access != calendar.AccessWrite {
			continue
		}
		if c.Primary {
			return c
		}
		if first == nil {
			first = &cals[i]
		}
	}
	if first == nil {
		t.Fatalf("%s has no writable calendar: %+v", p.Name, cals)
	}
	return *first
}

// inbox returns the iCalendar objects in a principal's schedule inbox.
func inbox(t *testing.T, p principal) []string {
	t.Helper()
	ctx := context.Background()
	d, err := p.store.discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.inbox == "" {
		t.Fatalf("%s has no schedule inbox", p.Name)
	}
	ms, err := p.store.propfind(ctx, d.inbox, "1", "d:resourcetype")
	if err != nil {
		t.Fatalf("listing the inbox: %v", err)
	}
	var items []string
	for _, r := range ms.Responses {
		href := r.href()
		if !childOf(d.inbox, href) {
			continue
		}
		obj, err := p.store.getObject(ctx, href)
		if err != nil {
			t.Fatalf("reading inbox item %s: %v", href, err)
		}
		items = append(items, unfoldICS(string(obj.raw)))
	}
	return items
}

// awaitInbox polls an inbox until an item satisfies match.
func awaitInbox(t *testing.T, p principal, what string, match func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(deliveryWait)
	for {
		for _, item := range inbox(t, p) {
			if match(item) {
				return item
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s in %s's inbox after %v: %q", what, p.Name, deliveryWait, inbox(t, p))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func countInbox(t *testing.T, p principal, uid, method string) int {
	t.Helper()
	n := 0
	for _, item := range inbox(t, p) {
		if strings.Contains(item, uid) && strings.Contains(item, "METHOD:"+method) {
			n++
		}
	}
	return n
}

func meeting(uid string, organizer principal, attendees ...principal) calendar.Event {
	ny, _ := time.LoadLocation("America/New_York")
	day := time.Now().AddDate(0, 0, 7)
	start := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, ny)
	ev := calendar.Event{
		UID:       uid,
		Summary:   "Integration meeting",
		Start:     start.UTC(),
		End:       start.Add(time.Hour).UTC(),
		TZID:      "America/New_York",
		Status:    calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: organizer.Email},
		Attendees: []calendar.Attendee{{Person: calendar.Person{Email: organizer.Email}, PartStat: calendar.PartStatAccepted}},
	}
	for _, a := range attendees {
		ev.Attendees = append(ev.Attendees, calendar.Attendee{
			Person: calendar.Person{Email: a.Email}, PartStat: calendar.PartStatNeedsAction, RSVP: true, Role: "REQ-PARTICIPANT",
		})
	}
	return ev
}

func TestMailwrightCalDAV(t *testing.T) {
	srv := mailwright.Require(t)
	ctx := context.Background()
	jane := newPrincipal(t, srv, "jane")
	john := newPrincipal(t, srv, "john")
	janeCal := writableCalendar(t, jane)
	johnCal := writableCalendar(t, john)

	// Discovery found the scheduling collections and the address.
	d, err := jane.store.discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.outbox == "" || d.inbox == "" || len(d.addresses) == 0 {
		t.Fatalf("discovery = %+v, want inbox, outbox and an address", d)
	}

	// The initial sync is a full one by sync-collection.
	initial, err := jane.store.Sync(ctx, janeCal.ID, "")
	if err != nil {
		t.Fatalf("initial Sync: %v", err)
	}
	if !initial.Full || !strings.HasPrefix(initial.Token, tokenSync) {
		t.Errorf("initial sync: Full %v token %q, want a sync-collection token", initial.Full, initial.Token)
	}

	// Create an event inviting john.
	uid := "integration-" + strings.ToLower(time.Now().Format("20060102150405.000")) + "@chainmail"
	created, err := jane.store.Put(ctx, janeCal.ID, meeting(uid, jane, john), "", calendar.NotifySend)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ETag == "" || !strings.HasPrefix(created.ProviderID, janeCal.ID) {
		t.Errorf("created = ProviderID %q ETag %q", created.ProviderID, created.ETag)
	}
	if _, err := jane.store.Put(ctx, janeCal.ID, meeting(uid, jane, john), "", calendar.NotifySend); !errors.Is(err, calstore.ErrExists) {
		t.Errorf("repeated create: err = %v, want ErrExists", err)
	}
	stored := string(created.Raw)
	if !strings.Contains(stored, "BEGIN:VTIMEZONE") {
		t.Errorf("stored object has no VTIMEZONE:\n%s", stored)
	}

	// The invitation reaches john.
	awaitInbox(t, john, "REQUEST", func(item string) bool {
		return strings.Contains(item, uid) && strings.Contains(item, "METHOD:REQUEST")
	})

	// Incremental sync sees the new object.
	inc, err := jane.store.Sync(ctx, janeCal.ID, initial.Token)
	if err != nil {
		t.Fatalf("incremental Sync: %v", err)
	}
	found := false
	for _, e := range inc.Upserts {
		found = found || e.ProviderID == created.ProviderID
	}
	if inc.Full || !found {
		t.Errorf("incremental sync: Full %v, created object found %v", inc.Full, found)
	}

	// An update with the current etag succeeds; the old etag then conflicts.
	upd := created
	upd.Location = "Room 7"
	updated, err := jane.store.Put(ctx, janeCal.ID, upd, created.ETag, calendar.NotifySend)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Location != "Room 7" || updated.ETag == created.ETag {
		t.Errorf("updated = location %q etag %q (was %q)", updated.Location, updated.ETag, created.ETag)
	}
	if _, err := jane.store.Put(ctx, janeCal.ID, upd, created.ETag, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("stale etag: err = %v, want ErrConflict", err)
	}

	// John files the invitation in his calendar and accepts it, and the
	// server sends jane a REPLY.
	req := awaitInbox(t, john, "REQUEST", func(item string) bool {
		return strings.Contains(item, uid) && strings.Contains(item, "METHOD:REQUEST")
	})
	_, evs, err := calendar.ParseICS([]byte(req))
	if err != nil || len(evs) == 0 {
		t.Fatalf("parsing the invitation: %v", err)
	}
	copyEv := evs[0]
	copyEv.ProviderID, copyEv.ETag, copyEv.Raw = "", "", nil
	filed, err := john.store.Put(ctx, johnCal.ID, copyEv, "", calendar.NotifySend)
	if err != nil {
		t.Fatalf("filing the invitation: %v", err)
	}
	accepted, err := john.store.Respond(ctx, johnCal.ID, filed.ProviderID, calendar.Response{
		Self: john.Email, PartStat: calendar.PartStatAccepted, Comment: "Works for me",
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	for _, a := range accepted.Attendees {
		if strings.EqualFold(a.Email, john.Email) && a.PartStat != calendar.PartStatAccepted {
			t.Errorf("john's PARTSTAT after Respond = %s", a.PartStat)
		}
	}
	reply := awaitInbox(t, jane, "REPLY", func(item string) bool {
		return strings.Contains(item, uid) && strings.Contains(item, "METHOD:REPLY")
	})
	if !strings.Contains(line(reply, "ATTENDEE"), "PARTSTAT=ACCEPTED") {
		t.Errorf("REPLY does not carry the acceptance:\n%s", reply)
	}

	// Free/busy: john is busy at the meeting; a stranger is unknown.
	fb, err := jane.store.FreeBusy(ctx, []string{john.Email, "stranger@elsewhere.invalid"},
		created.Start.Add(-time.Hour), created.End.Add(time.Hour))
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	jf := fb[john.Email]
	busy := false
	for _, b := range jf.Busy {
		busy = busy || (b.Start.Before(created.End) && b.End.After(created.Start))
	}
	if !jf.Known || !busy {
		t.Errorf("john's free/busy = %+v, want known and busy during the meeting", jf)
	}
	if fb["stranger@elsewhere.invalid"].Known {
		t.Error("a stranger's free/busy is known")
	}

	// A held create notifies nobody.
	heldUID := "held-" + uid
	held, err := jane.store.Put(ctx, janeCal.ID, meeting(heldUID, jane, john), "", calendar.NotifyNone)
	if err != nil {
		t.Fatalf("held create: %v", err)
	}
	for _, a := range lines(string(held.Raw), "ATTENDEE") {
		if !strings.Contains(a, "SCHEDULE-AGENT=CLIENT") {
			t.Errorf("held attendee without SCHEDULE-AGENT=CLIENT: %s", a)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := countInbox(t, john, heldUID, "REQUEST"); n != 0 {
		t.Errorf("a held create delivered %d invitations", n)
	}

	// Deleting the organizer's event cancels it for john.
	if err := jane.store.Delete(ctx, janeCal.ID, updated.ProviderID, updated.ETag, calendar.NotifySend); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	awaitInbox(t, john, "CANCEL", func(item string) bool {
		return strings.Contains(item, uid) && strings.Contains(item, "METHOD:CANCEL")
	})
	if _, err := jane.store.GetEvent(ctx, janeCal.ID, updated.ProviderID); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("deleted event: err = %v, want ErrNotFound", err)
	}

	// A silent delete of the held event sends nothing.
	if err := jane.store.Delete(ctx, janeCal.ID, held.ProviderID, held.ETag, calendar.NotifyNone); err != nil {
		t.Fatalf("silent Delete: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countInbox(t, john, heldUID, "CANCEL"); n != 0 {
		t.Errorf("a silent delete delivered %d cancellations", n)
	}
}

func TestMailwrightRefusesWrongPassword(t *testing.T) {
	srv := mailwright.Require(t)
	p, err := srv.CreatePrincipal("mallory")
	if err != nil {
		t.Fatal(err)
	}
	p.Password = "wrong-" + p.Password
	s, err := New(srv.HTTPClient(p), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListCalendars(context.Background()); !errors.Is(err, calstore.ErrAuth) {
		t.Errorf("wrong password: err = %v, want ErrAuth", err)
	}
}
