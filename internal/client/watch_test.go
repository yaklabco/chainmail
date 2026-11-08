package client

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

// stubWatch answers ReadEvents with one event, whose detail is detailBytes
// long when that is set, and streams one event then one heartbeat from
// Subscribe. It sends the client header of every call on headers.
type stubWatch struct {
	chainmailv1connect.UnimplementedWatchServiceHandler
	headers     chan<- string
	detailBytes int
}

func (s stubWatch) ReadEvents(_ context.Context, req *connect.Request[chainmailv1.ReadEventsRequest]) (*connect.Response[chainmailv1.ReadEventsResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	ev := &chainmailv1.WatchEvent{Id: 7, Kind: "new", Account: "work", Folder: "INBOX", Ref: "r7"}
	if s.detailBytes > 0 {
		ev.Detail = strings.Repeat("x", s.detailBytes)
	}
	return connect.NewResponse(&chainmailv1.ReadEventsResponse{
		Events:     []*chainmailv1.WatchEvent{ev},
		NextCursor: 7,
	}), nil
}

func (s stubWatch) Subscribe(_ context.Context, req *connect.Request[chainmailv1.SubscribeRequest], stream *connect.ServerStream[chainmailv1.SubscribeResponse]) error {
	s.headers <- req.Header().Get(api.HeaderClient)
	if err := stream.Send(&chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Event{
		Event: &chainmailv1.WatchEvent{Id: 8, Kind: "flags", Account: "work", Folder: "INBOX", Ref: "r8", Flags: []string{`\Seen`}},
	}}); err != nil {
		return err
	}
	return stream.Send(&chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Heartbeat{
		Heartbeat: &chainmailv1.Heartbeat{Position: 8},
	}})
}

// serveWatch serves h as the WatchService on a new Unix socket and returns
// the socket path.
func serveWatch(t *testing.T, h chainmailv1connect.WatchServiceHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewWatchServiceHandler(h))
	return serveUnix(t, &http.Server{Handler: mux})
}

// TestWatchReadEventsReachesTheDaemon checks that the Watch client calls
// ReadEvents over the socket and names the client kind.
func TestWatchReadEventsReachesTheDaemon(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 1)
	c := New(serveWatch(t, stubWatch{headers: headers}), "test")
	defer c.Close()
	resp, err := c.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{Group: "bot", WaitSeconds: 1}))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if got := <-headers; got != "test" {
		t.Errorf("%s = %q, want test", api.HeaderClient, got)
	}
	events := resp.Msg.GetEvents()
	if len(events) != 1 || events[0].GetId() != 7 || events[0].GetKind() != "new" || events[0].GetRef() != "r7" {
		t.Errorf("events = %v, want one new event 7 for r7", events)
	}
	if got := resp.Msg.GetNextCursor(); got != 7 {
		t.Errorf("next_cursor = %d, want 7", got)
	}
}

// TestWatchSubscribeDecodesEventsAndHeartbeats checks that the Watch
// client streams Subscribe with the client header, and that both arms of
// the item oneof decode.
func TestWatchSubscribeDecodesEventsAndHeartbeats(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 1)
	c := New(serveWatch(t, stubWatch{headers: headers}), "test")
	defer c.Close()
	stream, err := c.Watch.Subscribe(t.Context(), connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var got []*chainmailv1.SubscribeResponse
	for stream.Receive() {
		got = append(got, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if h := <-headers; h != "test" {
		t.Errorf("%s on Subscribe = %q, want test", api.HeaderClient, h)
	}
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2: %v", len(got), got)
	}
	ev, ok := got[0].GetItem().(*chainmailv1.SubscribeResponse_Event)
	if !ok {
		t.Fatalf("first item = %T, want an event", got[0].GetItem())
	}
	if e := ev.Event; e.GetId() != 8 || e.GetKind() != "flags" || len(e.GetFlags()) != 1 || e.GetFlags()[0] != `\Seen` {
		t.Errorf("event = %v, want flags event 8 with \\Seen", e)
	}
	hb, ok := got[1].GetItem().(*chainmailv1.SubscribeResponse_Heartbeat)
	if !ok {
		t.Fatalf("second item = %T, want a heartbeat", got[1].GetItem())
	}
	if p := hb.Heartbeat.GetPosition(); p != 8 {
		t.Errorf("heartbeat position = %d, want 8", p)
	}
}

// TestWatchReadLimit checks that the Watch client reads a response as large
// as the daemon's 16 MiB cap and refuses one past maxMessageBytes.
func TestWatchReadLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		detail  int
		refused bool
	}{
		{name: "16 MiB is read", detail: 16 << 20},
		{name: "18 MiB is refused", detail: 18 << 20, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := make(chan string, 1)
			c := New(serveWatch(t, stubWatch{headers: headers, detailBytes: tt.detail}), "test")
			defer c.Close()
			_, err := c.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{}))
			switch {
			case !tt.refused && err != nil:
				t.Errorf("ReadEvents: %v, want success", err)
			case tt.refused && connect.CodeOf(err) != connect.CodeResourceExhausted:
				t.Errorf("ReadEvents: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeResourceExhausted)
			}
		})
	}
}

// stubRuleEvents answers ReadEvents with one event per kind the request's
// filter names: a rule event carries a rule change and every other kind a
// due payload, so a test can see both payloads cross the wire.
type stubRuleEvents struct {
	chainmailv1connect.UnimplementedWatchServiceHandler
	headers chan<- string
}

func (s stubRuleEvents) ReadEvents(_ context.Context, req *connect.Request[chainmailv1.ReadEventsRequest]) (*connect.Response[chainmailv1.ReadEventsResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	kinds := req.Msg.GetFilter().GetKinds()
	events := make([]*chainmailv1.WatchEvent, 0, len(kinds))
	for i, kind := range kinds {
		ev := &chainmailv1.WatchEvent{Id: int64(i + 1), Kind: kind}
		if kind == "rule" {
			ev.Rule = &chainmailv1.RuleChange{
				RuleId: "ru_receipts", Name: "receipts", Change: "disabled", Revision: 4,
				Actor: "1000/rule", Reason: "all_failed",
			}
		} else {
			ev.Due = &chainmailv1.WorkItemDue{
				ItemId: "it_report", Type: "task", DueDate: "2026-11-09", State: "in-progress", ParentId: "it_admin_work",
			}
		}
		events = append(events, ev)
	}
	return connect.NewResponse(&chainmailv1.ReadEventsResponse{Events: events, NextCursor: int64(len(events))}), nil
}

// TestWatchCarriesRuleAndDueEvents checks that the rule and due payloads
// of WatchEvent reach a client intact, and that each event carries only
// its own payload.
func TestWatchCarriesRuleAndDueEvents(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 1)
	c := New(serveWatch(t, stubRuleEvents{headers: headers}), "test")
	defer c.Close()
	kinds := []string{"rule", "item_due_soon", "item_due", "item_overdue"}
	resp, err := c.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{
		Filter: &chainmailv1.WatchFilter{Kinds: kinds},
	}))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if got := <-headers; got != "test" {
		t.Errorf("%s = %q, want test", api.HeaderClient, got)
	}
	events := resp.Msg.GetEvents()
	if len(events) != len(kinds) {
		t.Fatalf("got %d events, want %d: %v", len(events), len(kinds), events)
	}
	rule := events[0].GetRule()
	if rule.GetRuleId() != "ru_receipts" || rule.GetName() != "receipts" || rule.GetChange() != "disabled" ||
		rule.GetRevision() != 4 || rule.GetActor() != "1000/rule" || rule.GetReason() != "all_failed" {
		t.Errorf("rule payload = %v, want ru_receipts disabled at revision 4 by 1000/rule for all_failed", rule)
	}
	if events[0].GetDue() != nil {
		t.Errorf("rule event due = %v, want unset", events[0].GetDue())
	}
	for i, ev := range events[1:] {
		if ev.GetKind() != kinds[i+1] {
			t.Errorf("event %d kind = %q, want %q", i+1, ev.GetKind(), kinds[i+1])
		}
		due := ev.GetDue()
		if due.GetItemId() != "it_report" || due.GetType() != "task" || due.GetDueDate() != "2026-11-09" ||
			due.GetState() != "in-progress" || due.GetParentId() != "it_admin_work" {
			t.Errorf("%s payload = %v, want it_report, a task in progress due 2026-11-09 under it_admin_work", ev.GetKind(), due)
		}
		if ev.GetRule() != nil {
			t.Errorf("%s rule = %v, want unset", ev.GetKind(), ev.GetRule())
		}
	}
}
