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
	"github.com/jamesainslie/chainmail/internal/testutil/ctxtest"
	"github.com/jamesainslie/chainmail/internal/vault"
)

// uploadDaemon is a served daemon with a state directory for uploads.
type uploadDaemon struct {
	db      *sql.DB
	socket  string
	uploads string
}

// uploadTimers stands in for an upload's timers, so a test ends the wait
// for a message, or the whole upload, when it picks. Each wait the server
// starts arrives on waits, in order: the n-th is the wait for message n.
type uploadTimers struct {
	waits  chan chan time.Time
	expire chan struct{}
	once   sync.Once
}

func newUploadTimers() *uploadTimers {
	return &uploadTimers{waits: make(chan chan time.Time, 16), expire: make(chan struct{})}
}

func (u *uploadTimers) timer(time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	u.waits <- ch
	return ch, func() bool { return true }
}

func (u *uploadTimers) deadline(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return ctxtest.Expiring(ctx, u.expire), func() {}
}

// expireUpload ends the whole upload's deadline.
func (u *uploadTimers) expireUpload() { u.once.Do(func() { close(u.expire) }) }

// wait returns the server's wait for message n, counting from 1. When it
// returns, the server has received n-1 messages and reads nothing more.
func (u *uploadTimers) wait(t *testing.T, n int) chan time.Time {
	t.Helper()
	var ch chan time.Time
	for i := range n {
		select {
		case ch = <-u.waits:
		case <-time.After(time.Minute):
			t.Fatalf("the server started %d waits for a message, want %d", i, n)
		}
	}
	return ch
}

// serveUploads serves a daemon whose upload timers are timers', until the
// test ends. The real idle and total stay a minute, unused.
func serveUploads(t *testing.T, timers *uploadTimers) uploadDaemon {
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
		// Uploads are the subject: no supervisor runs beside them.
		DisableWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.compose.uploadIdle, s.compose.uploadTimeout = time.Minute, time.Minute
	s.compose.uploadTimer, s.compose.uploadDeadline = timers.timer, timers.deadline
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	t.Cleanup(timers.expireUpload) // a failed test still ends a hooked upload before Serve stops
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

// TestUploadTimesOutOnTheServer stalls uploads before their header and
// after a data chunk, and runs one past its whole deadline while it still
// sends, over HTTP/2 and HTTP/1.1, never cancelling on the client side.
// The test ends the server's wait, or the whole upload, only once the
// server has read every part sent, and the client reads the answer before
// it closes its side, so over HTTP/1.1 it never writes to a connection the
// server has answered on. Each handler ends with TIMEOUT, leaves no partial
// file, and the client reads DEADLINE_EXCEEDED.
func TestUploadTimesOutOnTheServer(t *testing.T) {
	t.Parallel()
	header := &chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Header{
		Header: &chainmailv1.UploadHeader{Account: "host", Filename: "a.bin"},
	}}
	chunk := &chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Data{Data: []byte("partial data")}}
	tests := []struct {
		name string
		// parts are what the client sends before it stops. With none it
		// sends only the request headers.
		parts []*chainmailv1.UploadAttachmentRequest
		// total ends the whole upload, rather than the wait for the next part.
		total bool
	}{
		{name: "stalled before its header"},
		{name: "stalled after a chunk", parts: []*chainmailv1.UploadAttachmentRequest{header, chunk}},
		{name: "trickling past its total", total: true, parts: []*chainmailv1.UploadAttachmentRequest{header, chunk, chunk, chunk, chunk, chunk}},
	}
	for _, tt := range tests {
		for _, http1 := range []bool{false, true} {
			name := tt.name + "/http2"
			if http1 {
				name = tt.name + "/http1"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				timers := newUploadTimers()
				d := serveUploads(t, timers)
				client, tr := d.composeClient(http1)
				defer tr.CloseIdleConnections()
				stream := client.UploadAttachment(t.Context())
				stream.RequestHeader().Set(api.HeaderClient, "test")
				if len(tt.parts) == 0 {
					if err := stream.Send(nil); err != nil {
						t.Fatalf("sending the request headers: %v", err)
					}
				}
				for i, p := range tt.parts {
					if err := stream.Send(p); err != nil {
						t.Fatalf("sending part %d: %v", i+1, err)
					}
				}
				waiting := timers.wait(t, len(tt.parts)+1) // the server has read every part
				if tt.total {
					timers.expireUpload()
				} else {
					waiting <- time.Now()
				}
				conn, err := stream.Conn()
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.Receive(new(chainmailv1.UploadAttachmentResponse)); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
					t.Errorf("client err = %v, want DEADLINE_EXCEEDED", err)
				}
				// The answer is in. Closing may now meet a connection the
				// server closed, which no longer matters.
				_ = conn.CloseRequest()
				_ = conn.CloseResponse()
				if reason := d.waitUploadAudit(t, 1); reason != api.ReasonTimeout {
					t.Errorf("audit reason = %q, want %s", reason, api.ReasonTimeout)
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
