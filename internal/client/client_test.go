package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

func TestCloseDropsIdleConnections(t *testing.T) {
	t.Parallel()
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
	closed := make(chan struct{}, 1)
	headers := make(chan string, 1)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Protocols: protocols,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers <- r.Header.Get(api.HeaderClient)
			http.Error(w, "nope", http.StatusTeapot)
		}),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed {
				select {
				case closed <- struct{}{}:
				default:
				}
			}
		},
	}
	go func() { _ = srv.Serve(ln) }() // returns http.ErrServerClosed on Close
	t.Cleanup(func() { _ = srv.Close() })

	c := New(socket, "cli")
	_, err = c.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("GetStatus: %v", err)
	}
	if got := <-headers; got != "cli" {
		t.Errorf("%s = %q, want cli", api.HeaderClient, got)
	}
	c.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection still open after Close")
	}
}

func TestStreamingCallsCarryClientHeader(t *testing.T) {
	t.Parallel()
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
	headers := make(chan string, 1)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Protocols: protocols,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers <- r.Header.Get(api.HeaderClient)
			_, _ = io.Copy(io.Discard, r.Body)
			http.Error(w, "nope", http.StatusTeapot)
		}),
	}
	go func() { _ = srv.Serve(ln) }() // returns http.ErrServerClosed on Close
	t.Cleanup(func() { _ = srv.Close() })

	c := New(socket, "cli")
	defer c.Close()
	stream, err := c.Message.GetRaw(t.Context(), connect.NewRequest(&chainmailv1.GetRawRequest{Ref: "x"}))
	if err == nil {
		for stream.Receive() {
		}
		_ = stream.Close()
	}
	select {
	case got := <-headers:
		if got != "cli" {
			t.Errorf("%s on a stream = %q, want cli", api.HeaderClient, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the server")
	}
}

// bigDryRun answers Send with a dry run whose raw message is size bytes.
type bigDryRun struct {
	chainmailv1connect.UnimplementedComposeServiceHandler
	size int
}

func (b bigDryRun) Send(context.Context, *connect.Request[chainmailv1.SendRequest]) (*connect.Response[chainmailv1.SendResponse], error) {
	return connect.NewResponse(&chainmailv1.SendResponse{Result: &chainmailv1.ComposeResult{
		Result: &chainmailv1.ComposeResult_DryRun{DryRun: &chainmailv1.DryRun{Raw: make([]byte, b.size)}},
	}}), nil
}

// TestComposeAcceptsLargeResponses checks that the Compose client reads a
// dry run as large as the daemon's 40 MiB Compose response cap allows.
func TestComposeAcceptsLargeResponses(t *testing.T) {
	t.Parallel()
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
	const size = 39 << 20
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewComposeServiceHandler(bigDryRun{size: size}))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Protocols: protocols, Handler: mux}
	go func() { _ = srv.Serve(ln) }() // returns http.ErrServerClosed on Close
	t.Cleanup(func() { _ = srv.Close() })

	c := New(socket, "cli")
	defer c.Close()
	resp, err := c.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{DryRun: true}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(resp.Msg.GetResult().GetDryRun().GetRaw()); got != size {
		t.Errorf("raw = %d bytes, want %d", got, size)
	}
}
