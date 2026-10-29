package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerCredentials(t *testing.T) {
	t.Parallel()
	path := filepath.Join(socketDir(t), "s.sock")
	ln, err := Listen(t.Context(), path, -1)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Peer, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PeerFromContext(r.Context())
			if !ok {
				http.Error(w, "no peer", http.StatusInternalServerError)
				return
			}
			got <- p
		}),
		ConnContext: connContext,
	}
	go func() { _ = srv.Serve(ln) }() // returns http.ErrServerClosed on Close
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://daemon/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	p := <-got
	if int(p.UID) != os.Getuid() || int(p.PID) != os.Getpid() {
		t.Errorf("peer = %+v, want uid %d pid %d", p, os.Getuid(), os.Getpid())
	}
}

func TestAuthorizerAllowed(t *testing.T) {
	t.Parallel()
	groups := map[uint32][]string{
		1001: {"100", "990"},
		1002: {"100"},
	}
	lookup := func(uid uint32) ([]string, error) {
		g, ok := groups[uid]
		if !ok {
			return nil, errors.New("no such user")
		}
		return g, nil
	}
	tests := []struct {
		name string
		gid  int
		peer Peer
		want bool
	}{
		{name: "own uid", gid: -1, peer: Peer{UID: 500}, want: true},
		{name: "other uid without group", gid: -1, peer: Peer{UID: 1001}, want: false},
		{name: "primary group matches", gid: 990, peer: Peer{UID: 1003, GID: 990}, want: true},
		{name: "supplementary group matches", gid: 990, peer: Peer{UID: 1001, GID: 100}, want: true},
		{name: "not a member", gid: 990, peer: Peer{UID: 1002, GID: 100}, want: false},
		{name: "lookup fails", gid: 990, peer: Peer{UID: 4242, GID: 100}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := NewAuthorizer(500, tt.gid)
			a.groupsOf = lookup
			if got := a.Allowed(tt.peer); got != tt.want {
				t.Errorf("Allowed(%+v) = %v, want %v", tt.peer, got, tt.want)
			}
		})
	}
}

func TestMiddlewareRejectsMissingPeer(t *testing.T) {
	t.Parallel()
	called := false
	h := NewAuthorizer(500, -1).Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil))
	if rec.Code != http.StatusForbidden || called {
		t.Errorf("status = %d, called = %v; want 403 and not called", rec.Code, called)
	}
}
