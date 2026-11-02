package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
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
// the socket path. The directory comes from os.MkdirTemp rather than
// t.TempDir, since a socket path must fit in sun_path.
func serveWatch(t *testing.T, h chainmailv1connect.WatchServiceHandler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cmc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewWatchServiceHandler(h))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Protocols: protocols, Handler: mux}
	go func() { _ = srv.Serve(ln) }() // returns http.ErrServerClosed on Close
	t.Cleanup(func() { _ = srv.Close() })
	return socket
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
