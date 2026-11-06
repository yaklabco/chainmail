package daemon_test

import (
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// itemOp matches the item event of op on item id.
func itemOp(id, op string) func(*chainmailv1.WatchEvent) bool {
	return func(ev *chainmailv1.WatchEvent) bool {
		return ev.GetKind() == "item" && ev.GetItem().GetItemId() == id && ev.GetItem().GetOp() == op
	}
}

// TestWatchItemFilter checks that the item filter passes the item and the
// items below it and nothing else, that kinds can name item, that a reader
// with no kinds sees no item events, the filter's refusals, and its audit
// target.
func TestWatchItemFilter(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	epic := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "epic", Title: "Launch", Area: "work"}).GetItem().GetId()
	child := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Form", ParentId: epic}).GetItem().GetId()
	other := itemTask(t, h, "Elsewhere")
	tail := readEvents(t, h, &chainmailv1.ReadEventsRequest{}).GetNextCursor()
	for _, id := range []string{child, other, epic} {
		if _, err := itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id, Title: "Renamed"}, "title"); err != nil {
			t.Fatal(err)
		}
	}
	events := readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(tail), Filter: &chainmailv1.WatchFilter{Item: epic}}).GetEvents()
	var got []string
	for _, e := range events {
		got = append(got, e.GetKind()+" "+e.GetItem().GetItemId()+" "+e.GetItem().GetOp()+" "+strings.Join(e.GetItem().GetFields(), ","))
	}
	if want := []string{"item " + child + " updated title", "item " + epic + " updated title"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("filtered events = %q, want %q", got, want)
	}
	if len(events) > 0 && (events[0].GetAccount() != "" || events[0].GetMessage() != nil || events[0].GetItem().GetType() != "task") {
		t.Errorf("item event = %v, want no account and no message", events[0])
	}
	if n := len(readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(tail), Filter: &chainmailv1.WatchFilter{Kinds: []string{"item", "item_mail"}}}).GetEvents()); n != 3 {
		t.Errorf("kinds item,item_mail gave %d events, want the 3 updates", n)
	}
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0)}).GetEvents() {
		if e.GetKind() == "item" || e.GetKind() == "item_mail" {
			t.Errorf("a reader with no kinds got %v; item kinds are opt-in", e)
		}
	}
	if _, err := h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: other})); err != nil {
		t.Fatal(err)
	}
	refusals := []struct {
		name, item string
		code       connect.Code
		reason     string
		hint       string
	}{
		{name: "malformed", item: "Launch", code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "item IDs start with it_; run: chainmail item list"},
		{name: "unknown", item: "it_nope", code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list"},
		{name: "deleted", item: other, code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list"},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.Client.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{Filter: &chainmailv1.WatchFilter{Item: tt.item}}))
			wantItemErr(t, "read "+tt.name, err, tt.code, tt.reason, tt.hint)
			wantItemErr(t, "subscribe "+tt.name, subscribeErr(t, h, &chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Item: tt.item}}), tt.code, tt.reason, tt.hint)
		})
	}
	var reads []string
	for _, r := range h.AuditRows(t) {
		if r.Action == "watch.read" || r.Action == "watch.subscribe" {
			reads = append(reads, r.Targets)
		}
	}
	if want := itemTargets(t, "since="+strconv.FormatInt(tail, 10), "accounts=0", "kinds=", "in=unset", "item="+epic, "events=2"); len(reads) < 2 || reads[1] != want {
		t.Errorf("filtered read audited as %v, want %s second", reads, want)
	}
	if !strings.Contains(strings.Join(reads, " "), `"item=(invalid)"`) {
		t.Errorf("reads audited as %v, want the malformed item as (invalid)", reads)
	}
}

// TestItemEventsStream checks that a Subscribe stream naming kind item
// receives the created and linked events of an item as they happen.
func TestItemEventsStream(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), From: itemAnn, Subject: "hi", MessageID: "hi@example.com"})
	stream := subscribe(t, m.Harness, &chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Kinds: []string{"item"}}})
	untilHeartbeat(t, stream)
	id := itemTask(t, m.Harness, "Reply to Ann")
	itemLink(t, m.Harness, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: r.String()})
	created := awaitWatchEvent(t, stream, itemOp(id, "created"))
	linked := awaitWatchEvent(t, stream, itemOp(id, "linked"))
	if created.GetAccount() != "" || created.GetItem().GetType() != "task" || created.GetMessage() != nil ||
		linked.GetId() <= created.GetId() || linked.GetItem().GetRevision() <= created.GetItem().GetRevision() {
		t.Errorf("created %v then linked %v", created, linked)
	}
}
