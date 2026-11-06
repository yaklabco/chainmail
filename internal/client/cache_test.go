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

// stubCache answers each CacheService call with a fixed response. The
// status names one folder, folderBytes long when that is set. It sends the
// client header of every call on headers.
type stubCache struct {
	chainmailv1connect.UnimplementedCacheServiceHandler
	headers     chan<- string
	folderBytes int
}

func (s stubCache) GetCacheStatus(_ context.Context, req *connect.Request[chainmailv1.GetCacheStatusRequest]) (*connect.Response[chainmailv1.GetCacheStatusResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	folder := "INBOX"
	if s.folderBytes > 0 {
		folder = strings.Repeat("x", s.folderBytes)
	}
	return connect.NewResponse(&chainmailv1.GetCacheStatusResponse{
		SizeBytes: 4096, MaxSizeBytes: 2 << 30,
		Accounts: []*chainmailv1.AccountCacheStatus{{
			Account: strings.Join(req.Msg.GetAccounts(), ","), Enabled: true, State: "ready",
			Folders: []*chainmailv1.FolderCacheStatus{{Folder: folder, State: "ready", Headers: 42, BackfillDone: true, BaselineUid: 43}},
		}},
	}), nil
}

func (s stubCache) RebuildCache(_ context.Context, req *connect.Request[chainmailv1.RebuildCacheRequest]) (*connect.Response[chainmailv1.RebuildCacheResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	return connect.NewResponse(&chainmailv1.RebuildCacheResponse{Folders: []string{req.Msg.GetAccount() + "/" + req.Msg.GetFolder()}}), nil
}

func (s stubCache) EvictCache(_ context.Context, req *connect.Request[chainmailv1.EvictCacheRequest]) (*connect.Response[chainmailv1.EvictCacheResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	return connect.NewResponse(&chainmailv1.EvictCacheResponse{BodiesEvicted: 3, FreedBytes: 12288}), nil
}

// serveCache serves h as the CacheService on a new Unix socket and returns
// the socket path.
func serveCache(t *testing.T, h chainmailv1connect.CacheServiceHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewCacheServiceHandler(h))
	return serveUnix(t, &http.Server{Handler: mux})
}

// TestCacheReachesTheDaemon checks that the Cache client calls each
// CacheService method over the socket, names the client kind, and decodes
// the answers.
func TestCacheReachesTheDaemon(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 3)
	c := New(serveCache(t, stubCache{headers: headers}), "test")
	defer c.Close()

	status, err := c.Cache.GetCacheStatus(t.Context(), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: []string{"work"}}))
	if err != nil {
		t.Fatalf("GetCacheStatus: %v", err)
	}
	accts := status.Msg.GetAccounts()
	if status.Msg.GetMaxSizeBytes() != 2<<30 || len(accts) != 1 || accts[0].GetAccount() != "work" || accts[0].GetState() != "ready" {
		t.Errorf("status = %v, want one ready account work under a 2 GiB cap", status.Msg)
	}
	if f := accts[0].GetFolders(); len(f) != 1 || f[0].GetHeaders() != 42 || !f[0].GetBackfillDone() || f[0].GetBaselineUid() != 43 {
		t.Errorf("folders = %v, want INBOX with 42 headers, backfill done from 43", f)
	}

	rebuilt, err := c.Cache.RebuildCache(t.Context(), connect.NewRequest(&chainmailv1.RebuildCacheRequest{Account: "work", Folder: "INBOX"}))
	if err != nil {
		t.Fatalf("RebuildCache: %v", err)
	}
	if got := rebuilt.Msg.GetFolders(); len(got) != 1 || got[0] != "work/INBOX" {
		t.Errorf("rebuilt folders = %q, want [work/INBOX]", got)
	}

	evicted, err := c.Cache.EvictCache(t.Context(), connect.NewRequest(&chainmailv1.EvictCacheRequest{}))
	if err != nil {
		t.Fatalf("EvictCache: %v", err)
	}
	if evicted.Msg.GetBodiesEvicted() != 3 || evicted.Msg.GetFreedBytes() != 12288 {
		t.Errorf("evicted = %v, want 3 bodies and 12288 bytes", evicted.Msg)
	}

	for range 3 {
		if got := <-headers; got != "test" {
			t.Errorf("%s = %q, want test", api.HeaderClient, got)
		}
	}
}

// TestCacheReadLimit checks that the Cache client reads a status as large
// as the daemon's 16 MiB cap and refuses one past maxMessageBytes.
func TestCacheReadLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		folder  int
		refused bool
	}{
		{name: "16 MiB is read", folder: 16 << 20},
		{name: "18 MiB is refused", folder: 18 << 20, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := make(chan string, 1)
			c := New(serveCache(t, stubCache{headers: headers, folderBytes: tt.folder}), "test")
			defer c.Close()
			_, err := c.Cache.GetCacheStatus(t.Context(), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{}))
			switch {
			case !tt.refused && err != nil:
				t.Errorf("GetCacheStatus: %v, want success", err)
			case tt.refused && connect.CodeOf(err) != connect.CodeResourceExhausted:
				t.Errorf("GetCacheStatus: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeResourceExhausted)
			}
		})
	}
}
