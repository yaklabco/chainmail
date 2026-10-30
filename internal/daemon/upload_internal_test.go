package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"filippo.io/age"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/vault"
)

// uploadDaemon is a served daemon with a state directory for uploads.
type uploadDaemon struct {
	db      *sql.DB
	socket  string
	uploads string
}

// serveUploads serves a daemon whose uploads end after idle without a
// message, or after total in all, until the test ends.
func serveUploads(t *testing.T, idle, total time.Duration) uploadDaemon {
	t.Helper()
	cfg, err := config.Parse([]byte(passwordConfig))
	if err != nil {
		t.Fatal(err)
	}
	dir := socketDir(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(filepath.Join(dir, "vault.age"), id)
	if err != nil {
		t.Fatal(err)
	}
	stateDB, err := db.OpenState(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() })
	socket := filepath.Join(dir, "d.sock")
	ln, err := Listen(t.Context(), socket, -1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Config: cfg, Vault: v, StateDB: stateDB, Listener: ln, StateDir: dir,
		Authorizer: NewAuthorizer(uint32(os.Getuid()), -1),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.compose.uploadIdle, s.compose.uploadTimeout = idle, total
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return uploadDaemon{db: stateDB, socket: socket, uploads: filepath.Join(dir, "uploads")}
}

// composeClient is a Compose client over HTTP/2 or, with http1, HTTP/1.1,
// and its transport, whose connections the caller closes.
func (d uploadDaemon) composeClient(http1 bool) (chainmailv1connect.ComposeServiceClient, *http.Transport) {
	protocols := new(http.Protocols)
	if http1 {
		protocols.SetHTTP1(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}
	tr := &http.Transport{Protocols: protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", d.socket)
	}}
	return chainmailv1connect.NewComposeServiceClient(&http.Client{Transport: tr}, "http://chainmail.local"), tr
}

// waitUploadAudit waits up to 5 seconds for the n-th upload audit row and
// returns its reason, or fails.
func (d uploadDaemon) waitUploadAudit(t *testing.T, n int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var reasons []string
		rows, err := d.db.QueryContext(t.Context(), "SELECT reason FROM audit WHERE action = 'upload' ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			reasons = append(reasons, r)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if len(reasons) >= n {
			return reasons[n-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("upload %d still running after 5 seconds: its handler did not time out", n)
	return ""
}

// TestUploadTimesOutOnTheServer stalls uploads before their header, after a
// data chunk, and trickles one past its total deadline, over HTTP/2 and
// HTTP/1.1, never cancelling on the client side. Each handler ends with
// TIMEOUT once the daemon's own deadline passes, leaves no partial file, and
// the client then reads DEADLINE_EXCEEDED.
func TestUploadTimesOutOnTheServer(t *testing.T) {
	t.Parallel()
	header := &chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Header{
		Header: &chainmailv1.UploadHeader{Account: "host", Filename: "a.bin"},
	}}
	chunk := &chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Data{Data: []byte("partial data")}}
	tests := []struct {
		name        string
		idle, total time.Duration
		// send runs on the client until the stream fails.
		send func(stream *connect.ClientStreamForClient[chainmailv1.UploadAttachmentRequest, chainmailv1.UploadAttachmentResponse]) error
	}{
		{"stalled before its header", 200 * time.Millisecond, time.Minute, func(s *connect.ClientStreamForClient[chainmailv1.UploadAttachmentRequest, chainmailv1.UploadAttachmentResponse]) error {
			return s.Send(nil)
		}},
		{"stalled after a chunk", 200 * time.Millisecond, time.Minute, func(s *connect.ClientStreamForClient[chainmailv1.UploadAttachmentRequest, chainmailv1.UploadAttachmentResponse]) error {
			if err := s.Send(header); err != nil {
				return err
			}
			return s.Send(chunk)
		}},
		{"trickling past its total", time.Minute, 400 * time.Millisecond, func(s *connect.ClientStreamForClient[chainmailv1.UploadAttachmentRequest, chainmailv1.UploadAttachmentResponse]) error {
			if err := s.Send(header); err != nil {
				return err
			}
			for range 200 {
				if err := s.Send(chunk); err != nil {
					return err
				}
				time.Sleep(20 * time.Millisecond)
			}
			return nil
		}},
	}
	for _, tt := range tests {
		for _, http1 := range []bool{false, true} {
			name := tt.name + "/http2"
			if http1 {
				name = tt.name + "/http1"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				d := serveUploads(t, tt.idle, tt.total)
				client, tr := d.composeClient(http1)
				defer tr.CloseIdleConnections()
				stream := client.UploadAttachment(t.Context())
				stream.RequestHeader().Set(api.HeaderClient, "test")
				var wg sync.WaitGroup
				wg.Go(func() { _ = tt.send(stream) })
				if reason := d.waitUploadAudit(t, 1); reason != api.ReasonTimeout {
					t.Errorf("audit reason = %q, want %s", reason, api.ReasonTimeout)
				}
				wg.Wait()
				if _, err := stream.CloseAndReceive(); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
					t.Errorf("client err = %v, want DEADLINE_EXCEEDED", err)
				}
				entries, err := os.ReadDir(d.uploads)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), "up_") {
						t.Errorf("upload file %s left after the timeout", e.Name())
					}
				}
			})
		}
	}
}
