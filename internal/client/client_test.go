package client

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
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
				closed <- struct{}{}
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
