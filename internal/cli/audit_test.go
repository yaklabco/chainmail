package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// fakeAdmin records the ReadAudit requests it gets.
type fakeAdmin struct {
	chainmailv1connect.UnimplementedAdminServiceHandler

	mu   sync.Mutex
	reqs []*chainmailv1.ReadAuditRequest
}

func (f *fakeAdmin) ReadAudit(_ context.Context, req *connect.Request[chainmailv1.ReadAuditRequest]) (*connect.Response[chainmailv1.ReadAuditResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req.Msg)
	return connect.NewResponse(&chainmailv1.ReadAuditResponse{}), nil
}

func (f *fakeAdmin) last(t *testing.T) *chainmailv1.ReadAuditRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no ReadAudit request reached the server")
	}
	return f.reqs[len(f.reqs)-1]
}

// startFakeAdmin serves f on a Unix socket until the test ends and returns the socket.
func startFakeAdmin(t *testing.T, f *fakeAdmin) string {
	t.Helper()
	path, handler := chainmailv1connect.NewAdminServiceHandler(f)
	return startFakeServer(t, path, handler)
}

// startFakeServer serves one Connect service, as its New...ServiceHandler
// returns it, on a Unix socket until the test ends and returns the socket.
func startFakeServer(t *testing.T, path string, handler http.Handler) string {
	t.Helper()
	// Unix socket paths are limited to 108 bytes, so avoid t.TempDir's long paths.
	dir, err := os.MkdirTemp("", "cmd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fake server: %v", err)
		}
	})
	return socket
}

func TestAuditFlagsPassThrough(t *testing.T) {
	t.Parallel()
	f := &fakeAdmin{}
	socket := startFakeAdmin(t, f)
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	env := Env{Now: func() time.Time { return at }}

	got := runCLI(t, env, "--socket", socket, "audit",
		"--since", "2h", "--account", "host", "--action", "send", "--limit", "7", "--before-id", "42")
	if got.code != 0 {
		t.Fatalf("audit: %+v", got)
	}
	req := f.last(t)
	if req.GetAccount() != "host" || req.GetAction() != "send" || req.GetLimit() != 7 || req.GetBeforeId() != 42 {
		t.Errorf("request = %v", req)
	}
	if since, want := req.GetSince().AsTime(), at.Add(-2*time.Hour); !since.Equal(want) {
		t.Errorf("since = %v, want %v", since, want)
	}

	if got := runCLI(t, env, "--socket", socket, "audit"); got.code != 0 {
		t.Fatalf("audit: %+v", got)
	}
	req = f.last(t)
	if req.GetAccount() != "" || req.GetAction() != "" || req.GetLimit() != 0 || req.GetBeforeId() != 0 {
		t.Errorf("default request = %v, want the daemon to choose", req)
	}
	if since, want := req.GetSince().AsTime(), at.Add(-24*time.Hour); !since.Equal(want) {
		t.Errorf("default since = %v, want %v", since, want)
	}
}

func TestAuditFlagErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"audit", "--since", "yesterday"},
		{"audit", "--since", "-1h"},
		{"audit", "--limit", "x"},
		{"audit", "extra"},
	} {
		got := runCLI(t, Env{}, append([]string{"--socket", "/nonexistent.sock"}, args...)...)
		if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
			t.Errorf("%v: %+v, want a usage error", args, got)
		}
	}
}

func seedCLIAudit(t *testing.T, h *daemontest.Harness, entries ...audit.Entry) {
	t.Helper()
	w := audit.NewWriter(h.DB, nil)
	for _, e := range entries {
		if err := w.Record(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditJSON(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{NewStore: offlineStores})
	now := time.Now()
	seedCLIAudit(t, h,
		audit.Entry{At: now.Add(-3 * time.Hour), Client: "cli", Account: "host", Action: "send", Result: audit.ResultOK},
		audit.Entry{
			At: now.Add(-time.Hour), Client: "mcp", Account: "host", Action: "send",
			Targets: []string{"blocked:1"}, Decision: "held", Result: audit.ResultOK,
		},
		audit.Entry{At: now.Add(-time.Hour), Client: "cli", Account: "icloud", Action: "send", Result: audit.ResultOK},
	)
	got := runCLI(t, Env{}, "--socket", h.Socket, "--output", "json", "audit", "--since", "2h", "--account", "host")
	if got.code != 0 {
		t.Fatalf("audit: %+v", got)
	}
	if strings.Count(got.stdout, "\n") != 1 {
		t.Errorf("stdout is not one line of JSON: %q", got.stdout)
	}
	var resp chainmailv1.ReadAuditResponse
	if err := protojson.Unmarshal([]byte(got.stdout), &resp); err != nil {
		t.Fatalf("stdout %q: %v", got.stdout, err)
	}
	if len(resp.GetEntries()) != 1 {
		t.Fatalf("entries = %v, want the one host row newer than 2h", resp.GetEntries())
	}
	e := resp.GetEntries()[0]
	if e.GetId() != 2 || e.GetClient() != "mcp" || e.GetDecision() != "held" || !slices.Equal(e.GetTargets(), []string{"blocked:1"}) {
		t.Errorf("entry = %v", e)
	}
	if !strings.Contains(got.stdout, `"next_before_id":"0"`) {
		t.Errorf("stdout %q does not print next_before_id", got.stdout)
	}
}

func TestAuditTable(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{NewStore: offlineStores})
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	seedCLIAudit(t, h,
		audit.Entry{
			At: at, Client: "cli", Account: "host", Action: "send",
			Targets: []string{"blocked:2", "too_many"}, Decision: "held", Result: audit.ResultOK,
		},
		audit.Entry{
			At: at.Add(time.Minute), Client: "cli", Account: "icloud", Action: "evil\x1b[31m",
			Result: audit.ResultError, Reason: "NOT_FOUND",
		},
	)
	got := runCLI(t, Env{}, "--socket", h.Socket, "--output", "table", "audit")
	if got.code != 0 {
		t.Fatalf("audit: %+v", got)
	}
	if strings.ContainsRune(got.stdout, '\x1b') {
		t.Errorf("table holds a terminal escape: %q", got.stdout)
	}
	lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table = %q, want a header and 2 rows", got.stdout)
	}
	for i, want := range [][]string{
		{"AT", "ACTION", "ACCOUNT", "DECISION", "RESULT", "REASON", "TARGETS"},
		{at.Add(time.Minute).Format("2006-01-02 15:04"), "icloud", "error", "NOT_FOUND"},
		{at.Format("2006-01-02 15:04"), "send", "host", "held", "ok", "blocked:2,too_many"},
	} {
		for _, field := range want {
			if !strings.Contains(lines[i], field) {
				t.Errorf("line %d = %q, want it to hold %q", i, lines[i], field)
			}
		}
	}
	if !strings.HasPrefix(lines[0], "AT") {
		t.Errorf("header = %q", lines[0])
	}
}

func TestAuditTableNamesNextPage(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := auditTable(&b, time.UTC, &chainmailv1.ReadAuditResponse{NextBeforeId: 17}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "older rows: pass --before-id 17\n") {
		t.Errorf("table = %q, want the next page named", b.String())
	}
	b.Reset()
	if err := auditTable(&b, time.UTC, &chainmailv1.ReadAuditResponse{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "older rows") {
		t.Errorf("table = %q names a next page on the last page", b.String())
	}
}
