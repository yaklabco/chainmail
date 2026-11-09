package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

func TestCloseDropsIdleConnections(t *testing.T) {
	t.Parallel()
	closed := make(chan struct{}, 1)
	headers := make(chan string, 1)
	srv := &http.Server{
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
	socket := serveUnix(t, srv)

	c := New(socket, "cli")
	_, err := c.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
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
	headers := make(chan string, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers <- r.Header.Get(api.HeaderClient)
			_, _ = io.Copy(io.Discard, r.Body)
			http.Error(w, "nope", http.StatusTeapot)
		}),
	}
	socket := serveUnix(t, srv)

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
	const size = 39 << 20
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewComposeServiceHandler(bigDryRun{size: size}))
	socket := serveUnix(t, &http.Server{Handler: mux})

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

// stubAudit answers ReadAudit with one row that echoes the request's
// client, account and action filters, so a test can see them arrive.
type stubAudit struct {
	chainmailv1connect.UnimplementedAdminServiceHandler
	headers chan<- string
}

func (s stubAudit) ReadAudit(_ context.Context, req *connect.Request[chainmailv1.ReadAuditRequest]) (*connect.Response[chainmailv1.ReadAuditResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	m := req.Msg
	return connect.NewResponse(&chainmailv1.ReadAuditResponse{
		Entries: []*chainmailv1.AuditEntry{{Id: 9, Client: m.GetClient(), Account: m.GetAccount(), Action: m.GetAction()}},
	}), nil
}

// TestReadAuditCarriesTheClientFilter checks that ReadAuditRequest.client
// reaches the daemon beside the other filters.
func TestReadAuditCarriesTheClientFilter(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 1)
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewAdminServiceHandler(stubAudit{headers: headers}))
	c := New(serveUnix(t, &http.Server{Handler: mux}), "test")
	defer c.Close()
	resp, err := c.Admin.ReadAudit(t.Context(), connect.NewRequest(&chainmailv1.ReadAuditRequest{
		Client: "rule", Account: "work", Action: "move",
	}))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if got := <-headers; got != "test" {
		t.Errorf("%s = %q, want test", api.HeaderClient, got)
	}
	entries := resp.Msg.GetEntries()
	if len(entries) != 1 || entries[0].GetClient() != "rule" || entries[0].GetAccount() != "work" || entries[0].GetAction() != "move" {
		t.Errorf("entries = %v, want one row echoing client rule, account work, action move", entries)
	}
}
