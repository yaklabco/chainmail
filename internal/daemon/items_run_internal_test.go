package daemon

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
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
// item_mail_seen every trimInterval with the watch retention: a row older
// than it goes and a newer one stays.
func TestStartBackgroundTrimsItemMailSeen(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	s.trimInterval = time.Millisecond
	retention := s.opts.Config.WatchSettings().Retention
	at := func(age time.Duration) string {
		return s.opts.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}
	for key, stamp := range map[string]string{"old": at(retention + time.Hour), "new": at(time.Hour)} {
		if _, err := stateDB.ExecContext(t.Context(), "INSERT INTO item_mail_seen (item_id, account, message_key, at) VALUES ('it_x', 'host', ?, ?)", key, stamp); err != nil {
			t.Fatal(err)
		}
	}
	count := func(key string) int {
		var n int
		if err := stateDB.QueryRowContext(t.Context(), "SELECT count(*) FROM item_mail_seen WHERE message_key = ?", key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	stop := s.startBackground(t.Context())
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for count("old") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the expired item_mail_seen row is still there after 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count("new") != 1 {
		t.Error("the trim removed a row inside the retention")
	}
}
