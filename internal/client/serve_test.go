package client

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// serveUnix serves srv over unencrypted HTTP/2 on a new Unix socket and
// returns the socket path. It sets srv.Protocols. The directory comes from
// os.MkdirTemp rather than t.TempDir, since a socket path must fit in
// sun_path. Cleanup closes the server, waits for Serve to return and fails
// the test if Serve ended with anything but http.ErrServerClosed.
func serveUnix(t *testing.T, srv *http.Server) string {
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
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv.Protocols = protocols
	var (
		wg       sync.WaitGroup
		serveErr error
	)
	wg.Go(func() { serveErr = srv.Serve(ln) })
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
		wg.Wait()
		if !errors.Is(serveErr, http.ErrServerClosed) {
			t.Errorf("serve: %v, want %v", serveErr, http.ErrServerClosed)
		}
	})
	return socket
}
