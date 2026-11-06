package items_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// mailSpec describes one new message for newMail.
type mailSpec struct {
	account, folder string
	uid             uint32
	thread, msgID   string
	from            []string
	// arrived is the server arrival time (InternalDate). Zero means a day
	// after linkT0, later than every link these tests make.
	arrived time.Time
}

// newMail is m as a sync batch hands it to the engine's OnNew hook: its
// summary and its new event. arrive sets the event's ID.
func newMail(m mailSpec) watch.NewMail {
	arrived := m.arrived
	if arrived.IsZero() {
		arrived = linkT0.Add(24 * time.Hour)
	}
	r := ref.Ref{Account: m.account, Folder: m.folder, UIDValidity: 7, UID: m.uid}
	var from []mailstore.Address
	for _, f := range m.from {
		from = append(from, mailstore.Address{Name: "Sender", Email: f})
	}
	subject := fmt.Sprintf("message %d in %s", m.uid, m.folder)
	return watch.NewMail{
		Summary: mailstore.Summary{
			Ref: r, Date: arrived, InternalDate: arrived, From: from, Subject: subject,
			Flags: []string{}, ThreadID: m.thread, MessageID: m.msgID,
		},
		Event: watch.Event{Kind: watch.KindNew, Account: m.account, Folder: m.folder, Ref: r.String(), Message: &watch.Message{
			Date: arrived, InternalDate: arrived, From: from, To: []mailstore.Address{}, Subject: subject,
			Flags: []string{}, ThreadID: m.thread,
		}},
	}
}

// arrive writes news as one sync batch does: their new events, then what
// MailArrived returns for them, in one transaction, committed. It returns
// MailArrived's events with their IDs.
func (r *linkRig) arrive(t *testing.T, acct items.MailAccount, news ...watch.NewMail) []watch.Event {
	t.Helper()
	return r.arriveAt(t, acct, r.clock.Now(), news...)
}

// arriveAt is arrive with at passed to MailArrived as given.
func (r *linkRig) arriveAt(t *testing.T, acct items.MailAccount, at time.Time, news ...watch.NewMail) []watch.Event {
	t.Helper()
	ctx := t.Context()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	events := make([]watch.Event, len(news))
	for i, n := range news {
		events[i] = n.Event
	}
	if err := r.log.Append(ctx, tx, events); err != nil {
		t.Fatalf("Append(new): %v", err)
	}
	for i := range news {
		news[i].Event = events[i]
	}
	extra, err := items.MailArrived(ctx, tx, acct, news, at)
	if err != nil {
		t.Fatalf("MailArrived: %v", err)
	}
	if err := r.log.Append(ctx, tx, extra); err != nil {
		t.Fatalf("Append(item_mail): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r.log.Notify()
	return extra
}

func TestMailArrivedOneEventPerLinkedItem(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	a, b, gone, other := r.task(t), r.task(t), r.task(t), r.task(t)
	for _, id := range []string{a, b, gone} {
		r.link(t, id, "work", "t1")
	}
	r.link(t, other, "work", "t2")
	r.link(t, other, "home", "t1") // the same key on another account
	r.remove(t, gone)
	acct := items.MailAccount{Name: "work", Address: "me@example.net"}
	at := r.clock.Now()
	news := []watch.NewMail{
		newMail(mailSpec{account: "work", folder: "INBOX", uid: 10, thread: "t1", msgID: "m10@example.com", from: []string{"ann@example.com"}}),
		newMail(mailSpec{account: "work", folder: "INBOX", uid: 11, thread: "t3", msgID: "m11@example.com"}),
		newMail(mailSpec{account: "work", folder: "INBOX", uid: 12, msgID: "m12@example.com"}),
	}
	got := r.arrive(t, acct, news...)
	want := []string{a, b}
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("MailArrived gave %d events, want one per live linked item: %+v", len(got), got)
	}
	n := news[0]
	for i, e := range got {
		if e.Kind != watch.KindItemMail || e.ItemID != want[i] || e.ID == 0 || !e.At.Equal(at) || e.FromSelf {
			t.Errorf("event %d = %+v, want item_mail for %s at %v", i, e, want[i], at)
		}
		if e.Account != "work" || e.Folder != "INBOX" || e.Ref != n.Event.Ref {
			t.Errorf("event %d is on %s %s %s, want the new event's work INBOX %s", i, e.Account, e.Folder, e.Ref, n.Event.Ref)
		}
		if !reflect.DeepEqual(e.Message, n.Event.Message) || e.Message == n.Event.Message {
			t.Errorf("event %d message = %+v, want a copy of the new event's", i, e.Message)
		}
	}
	stored := r.events(t, watch.KindItemMail)
	if len(stored) != len(want) {
		t.Fatalf("the log holds %d item_mail events, want %d", len(stored), len(want))
	}
	for i, e := range stored {
		if e.ID != got[i].ID || e.ItemID != want[i] || e.Ref != n.Event.Ref || e.Message == nil || e.Message.Subject != n.Summary.Subject {
			t.Errorf("stored event %d = %+v, want %+v", i, e, got[i])
		}
	}
	if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != len(want) {
		t.Errorf("item_mail_seen holds %d rows, want %d", c, len(want))
	}
	spare := newMail(mailSpec{account: "spare", folder: "INBOX", uid: 1, thread: "t1", msgID: "s1@example.com"})
	if got := r.arrive(t, items.MailAccount{Name: "spare"}, spare); len(got) != 0 {
		t.Errorf("an account with no links got %+v", got)
	}
}

func TestMailArrivedOncePerMessage(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	r.link(t, r.task(t), "work", "t1")
	acct := items.MailAccount{Name: "work", Address: "me@example.net"}
	inbox := mailSpec{account: "work", folder: "INBOX", uid: 1, thread: "t1", msgID: "m1@example.com"}
	sent := inbox
	sent.folder, sent.uid = "Sent", 4
	noID := mailSpec{account: "work", folder: "INBOX", uid: 2, thread: "t1"}
	noIDCopy := noID
	noIDCopy.folder = "Archive"
	steps := []struct {
		name string
		m    mailSpec
		want int
	}{
		{"first sight", inbox, 1},
		{"the same Message-ID in another folder", sent, 0},
		{"the same Message-ID again", inbox, 0},
		{"no Message-ID: keyed by its ref", noID, 1},
		{"the same ref again", noID, 0},
		{"another ref with no Message-ID", noIDCopy, 1},
	}
	for _, s := range steps {
		if got := r.arrive(t, acct, newMail(s.m)); len(got) != s.want {
			t.Errorf("%s: %d events, want %d", s.name, len(got), s.want)
		}
	}
	twin := inbox
	twin.uid, twin.msgID = 9, "m9@example.com"
	again := twin
	again.uid = 10
	if got := r.arrive(t, acct, newMail(twin), newMail(again)); len(got) != 1 {
		t.Errorf("one Message-ID twice in a batch gave %d events, want 1", len(got))
	}
	if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != 4 {
		t.Errorf("item_mail_seen holds %d rows, want 4", c)
	}
}

func TestMailArrivedFromSelf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		address string
		from    []string
		want    bool
	}{
		{"from the account", "me@example.net", []string{"me@example.net"}, true},
		{"in another case", "me@example.net", []string{"Me@Example.NET"}, true},
		{"self among several senders", "me@example.net", []string{"ann@example.com", "me@example.net"}, true},
		{"from someone else", "me@example.net", []string{"ann@example.com"}, false},
		{"no address configured", "", []string{""}, false},
		{"no sender", "me@example.net", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLinkRig(t)
			r.link(t, r.task(t), "work", "t1")
			m := newMail(mailSpec{account: "work", folder: "INBOX", uid: 1, thread: "t1", msgID: "m1@example.com", from: tc.from})
			got := r.arrive(t, items.MailAccount{Name: "work", Address: tc.address}, m)
			if len(got) != 1 || got[0].FromSelf != tc.want {
				t.Fatalf("MailArrived = %+v, want one event with from_self %v", got, tc.want)
			}
			if stored := r.events(t, watch.KindItemMail); len(stored) != 1 || stored[0].FromSelf != tc.want {
				t.Errorf("stored = %+v, want from_self %v", stored, tc.want)
			}
		})
	}
}

func TestMailArrivedSkipsMailFromBeforeTheLink(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	r.clock.Advance(time.Hour)
	id := r.task(t)
	r.link(t, id, "work", "t1")
	linked := r.clock.Now()
	acct := items.MailAccount{Name: "work", Address: "me@example.net"}
	// A message that was on the server well before the link, filed or copied
	// into another synced folder now, is old mail: no event. One that arrived
	// at the link is news, even though the server's INTERNALDATE has only
	// whole seconds and so reads a little before the link's time. A message
	// with no arrival time counts as news.
	before := newMail(mailSpec{account: "work", folder: "Archive", uid: 1, thread: "t1", msgID: "old@example.com", arrived: linked.Add(-time.Hour).Truncate(time.Second)})
	at := newMail(mailSpec{account: "work", folder: "INBOX", uid: 2, thread: "t1", msgID: "now@example.com", arrived: linked.Truncate(time.Second)})
	undated := newMail(mailSpec{account: "work", folder: "INBOX", uid: 3, thread: "t1", msgID: "undated@example.com"})
	undated.Summary.InternalDate = time.Time{}
	got := r.arrive(t, acct, before, at, undated)
	if len(got) != 2 || got[0].Ref != at.Event.Ref || got[1].Ref != undated.Event.Ref {
		t.Fatalf("MailArrived gave %+v, want events for the message at the link and the undated one", got)
	}
	if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != 2 {
		t.Errorf("item_mail_seen holds %d rows, want 2: old mail is not recorded", c)
	}
}

func TestMailArrivedClockSkewAllowance(t *testing.T) {
	t.Parallel()
	// The link is made at 12:00:00.200. The server stamps INTERNALDATE in
	// whole seconds and its clock may lag the daemon's, so mail up to five
	// minutes before the link, counted from the link's whole second, is news.
	linked := time.Date(2026, 11, 6, 12, 0, 0, 200*int(time.Millisecond), time.UTC)
	whole := linked.Truncate(time.Second)
	tests := []struct {
		name    string
		arrived time.Time
		want    int
	}{
		{"in the link's second", whole, 1},
		{"after the link", whole.Add(time.Second), 1},
		{"4m59s before the link", whole.Add(-4*time.Minute - 59*time.Second), 1},
		{"exactly the allowance before the link", whole.Add(-5 * time.Minute), 1},
		{"a second past the allowance", whole.Add(-5*time.Minute - time.Second), 0},
		{"an hour before the link", whole.Add(-time.Hour), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLinkRig(t)
			id := r.task(t)
			r.clock.Advance(linked.Sub(r.clock.Now()))
			r.link(t, id, "work", "t1")
			m := newMail(mailSpec{account: "work", folder: "INBOX", uid: 1, thread: "t1", msgID: "m1@example.com", arrived: tc.arrived})
			if got := r.arrive(t, items.MailAccount{Name: "work"}, m); len(got) != tc.want {
				t.Errorf("MailArrived gave %d events, want %d: %+v", len(got), tc.want, got)
			}
			if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != tc.want {
				t.Errorf("item_mail_seen holds %d rows, want %d", c, tc.want)
			}
		})
	}
}

func TestMailArrivedZeroAtUsesTheClock(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	r.link(t, r.task(t), "work", "t1")
	acct := items.MailAccount{Name: "work"}
	inbox := mailSpec{account: "work", folder: "INBOX", uid: 1, thread: "t1", msgID: "m1@example.com"}
	got := r.arriveAt(t, acct, time.Time{}, newMail(inbox))
	if len(got) != 1 || got[0].At.IsZero() {
		t.Fatalf("MailArrived with a zero at = %+v, want one event with a time", got)
	}
	// The seen row was stamped now, so a trim of rows older than a minute
	// ago keeps it, and a copy of the message in another folder is not news.
	if _, err := r.store.Trim(t.Context(), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != 1 {
		t.Errorf("item_mail_seen holds %d rows after the trim, want 1", c)
	}
	sent := inbox
	sent.folder, sent.uid = "Sent", 4
	if again := r.arrive(t, acct, newMail(sent)); len(again) != 0 {
		t.Errorf("the same Message-ID in another folder gave %+v, want no event", again)
	}
}

func TestMailArrivedRollsBackWithTheBatch(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	r.link(t, r.task(t), "work", "t1")
	acct := items.MailAccount{Name: "work", Address: "me@example.net"}
	spec := mailSpec{account: "work", folder: "INBOX", uid: 1, thread: "t1", msgID: "m1@example.com"}
	ctx := t.Context()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := newMail(spec)
	events := []watch.Event{m.Event}
	if err := r.log.Append(ctx, tx, events); err != nil {
		t.Fatal(err)
	}
	m.Event = events[0]
	got, err := items.MailArrived(ctx, tx, acct, []watch.NewMail{m}, r.clock.Now())
	if err != nil || len(got) != 1 {
		t.Fatalf("MailArrived = %+v, %v; want one event", got, err)
	}
	// The batch fails after the hook, as a checkpoint write would.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != 0 {
		t.Errorf("item_mail_seen holds %d rows after the rollback, want 0", c)
	}
	// The next pass fetches the batch again and gets the event.
	if again := r.arrive(t, acct, newMail(spec)); len(again) != 1 {
		t.Errorf("the repeated batch gave %d events, want 1", len(again))
	}
}
