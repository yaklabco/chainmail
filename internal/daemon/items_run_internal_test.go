package daemon

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/watch"
)

// TestEventToProtoItemKinds checks both item kinds: an item event carries
// its change and no message, an item_mail event its item, from_self and
// the message row as a new event's.
func TestEventToProtoItemKinds(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	item := eventToProto(watch.Event{
		ID: 4, At: at, Kind: watch.KindItem,
		Item: &watch.ItemChange{ItemID: "it_a", Type: "task", Revision: 9, Op: "updated", Fields: []string{"title", "priority"}},
	})
	wantItem := &chainmailv1.WatchEvent{
		Id: 4, At: timestamppb.New(at), Kind: "item",
		Item: &chainmailv1.WorkItemChange{ItemId: "it_a", Type: "task", Revision: 9, Op: "updated", Fields: []string{"title", "priority"}},
	}
	if !proto.Equal(item, wantItem) {
		t.Errorf("item event = %v, want %v", item, wantItem)
	}
	mail := eventToProto(watch.Event{
		ID: 5, At: at, Kind: watch.KindItemMail, Account: "host", Folder: "INBOX", Ref: "cm1:host:INBOX:7:3",
		ItemID: "it_a", FromSelf: true, Message: &watch.Message{Date: at, Subject: "re:\tplan", ThreadID: "00aa11bb22cc33dd"},
	})
	wantMail := &chainmailv1.WatchEvent{
		Id: 5, At: timestamppb.New(at), Kind: "item_mail", Account: "host", Folder: "INBOX", Ref: "cm1:host:INBOX:7:3",
		ItemId: "it_a", FromSelf: true,
		Message: &chainmailv1.MessageRow{
			Ref: "cm1:host:INBOX:7:3", Account: "host", Folder: "INBOX", Date: timestamppb.New(at),
			Subject: untrusted("re:\tplan"), ThreadId: "00aa11bb22cc33dd",
		},
	}
	if !proto.Equal(mail, wantMail) {
		t.Errorf("item_mail event = %v, want %v", mail, wantMail)
	}
}

// TestItemMailHook checks the engine hook: an account missing from the
// config is an error, which rolls the batch back, and new mail on a thread
// no item links gives no events. It uses only the transaction it is given.
func TestItemMailHook(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	tx, err := stateDB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }() // nothing here is kept
	if _, err := s.itemMail(t.Context(), tx, "nope", nil); !errors.Is(err, config.ErrUnknownAccount) {
		t.Errorf("unknown account: %v, want ErrUnknownAccount", err)
	}
	news := []watch.NewMail{{
		Summary: mailstore.Summary{Ref: ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 1, UID: 7}, ThreadID: "00aa11bb22cc33dd", MessageID: "m@example.com"},
		Event:   watch.Event{ID: 1, Kind: watch.KindNew, Account: "host", Folder: "INBOX", Ref: "cm1:host:INBOX:1:7"},
	}}
	if events, err := s.itemMail(t.Context(), tx, "host", news); err != nil || len(events) != 0 {
		t.Errorf("unlinked mail = %v, %v; want no events", events, err)
	}
}

// TestStartBackgroundTrimsItemMailSeen checks that startBackground trims
// item_mail_seen with the watch retention once on start and again on each
// tick: a row older than it goes and a newer one stays, and a row that
// expired after the first trim goes at the next tick.
func TestStartBackgroundTrimsItemMailSeen(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	tick := make(chan time.Time)
	s.itemsTrimTick = tick
	retention := s.opts.Config.WatchSettings().Retention
	insert := func(key string, age time.Duration) {
		t.Helper()
		stamp := s.opts.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
		if _, err := stateDB.ExecContext(t.Context(), "INSERT INTO item_mail_seen (item_id, account, message_key, at) VALUES ('it_x', 'host', ?, ?)", key, stamp); err != nil {
			t.Fatal(err)
		}
	}
	count := func(key string) int {
		t.Helper()
		var n int
		if err := stateDB.QueryRowContext(t.Context(), "SELECT count(*) FROM item_mail_seen WHERE message_key = ?", key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitGone := func(key, when string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for count(key) != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the expired item_mail_seen row %q is still there 5s %s", key, when)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	insert("old", retention+time.Hour)
	insert("new", time.Hour)
	stop := s.startBackground(t.Context())
	defer stop()
	waitGone("old", "after the start")

	// The first trim's transaction has committed, so a row added now
	// stays until the loop trims again. The tick channel is unbuffered: the
	// send returns once the loop is waiting for it.
	insert("later", retention+time.Hour)
	if count("later") != 1 {
		t.Fatal("a row added after the first trim went before any tick")
	}
	select {
	case tick <- s.opts.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("the trim loop did not wait for a tick within 5s")
	}
	waitGone("later", "after a tick")
	if count("new") != 1 {
		t.Error("the trim removed a row inside the retention")
	}
}

// TestItemSearchUsesTheItemsStore checks that New gives item: searches the
// daemon's own items store as their resolver.
func TestItemSearchUsesTheItemsStore(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	if s.itemThreads == nil || s.itemThreads != itemResolver(s.items) {
		t.Errorf("itemThreads = %v, want the items store", s.itemThreads)
	}
}
