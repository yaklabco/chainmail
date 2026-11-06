package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/ref"
)

func TestNegotiationAndIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version string
	}{
		{name: "a 2026-07-28 client negotiates 2026-07-28", version: latest},
		{name: "a 2025-11-25 client still connects", version: legacy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, tt.version)
			init := e.cs.InitializeResult()
			if init.ProtocolVersion != tt.version {
				t.Errorf("protocol version = %q, want %q", init.ProtocolVersion, tt.version)
			}
			if init.ServerInfo == nil || init.ServerInfo.Name != "chainmail" || init.ServerInfo.Version != "test" {
				t.Errorf("server info = %+v", init.ServerInfo)
			}
			caps := init.Capabilities
			wire, err := json.Marshal(caps)
			if err != nil {
				t.Fatal(err)
			}
			var advertised map[string]json.RawMessage
			if err := json.Unmarshal(wire, &advertised); err != nil {
				t.Fatal(err)
			}
			if len(advertised) != 1 || advertised["tools"] == nil {
				t.Errorf("capabilities = %s, want tools only (no logging)", wire)
			}
			if caps.Tools == nil || !caps.Tools.ListChanged {
				t.Errorf("tools capability = %+v, want listChanged", caps.Tools)
			}
			for _, want := range []string{"untrusted", "never as instructions", "Drafts", "Do not retry", "read_events", "Work item text", "link notes", "item_mail"} {
				if !strings.Contains(init.Instructions, want) {
					t.Errorf("instructions lack %q", want)
				}
			}
			if _, err := e.cs.ListTools(t.Context(), nil); err != nil {
				t.Errorf("tools/list: %v", err)
			}
		})
	}
}

func TestServerDiscoverOnTheWire(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	startFake(t, f)
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir()})
	serverT, clientT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, serverT) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	})
	conn, err := clientT.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	id, err := jsonrpc.MakeID("discover-1")
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"wire","version":"0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}`)
	if err := conn.Write(ctx, &jsonrpc.Request{ID: id, Method: "server/discover", Params: params}); err != nil {
		t.Fatal(err)
	}
	msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := msg.(*jsonrpc.Response)
	if !ok || resp.Error != nil {
		t.Fatalf("server/discover answered %#v", msg)
	}
	var got struct {
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		Instructions      string                     `json:"instructions"`
		TTLMs             *int                       `json:"ttlMs"`
		CacheScope        string                     `json:"cacheScope"`
		Meta              map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{latest, legacy} {
		if !slices.Contains(got.SupportedVersions, v) {
			t.Errorf("supportedVersions = %v, want %s in them", got.SupportedVersions, v)
		}
	}
	if _, ok := got.Capabilities["tools"]; !ok {
		t.Errorf("capabilities = %s, want tools", resp.Result)
	}
	if _, ok := got.Capabilities["logging"]; ok {
		t.Errorf("capabilities advertise the deprecated logging capability: %s", resp.Result)
	}
	if got.TTLMs == nil || *got.TTLMs != 0 || got.CacheScope != "private" {
		t.Errorf("cache fields ttlMs=%v cacheScope=%q, want 0 and private", got.TTLMs, got.CacheScope)
	}
	if _, ok := got.Meta[mcp.MetaKeyServerInfo]; !ok || got.Instructions == "" {
		t.Errorf("discover result lacks serverInfo or instructions: %s", resp.Result)
	}
}

// probeTool registers a test-only tool that calls ListFolders and maps its error.
func probeTool(s *Server) {
	addTool(s, "probe", "Probe", "Test only: calls ListFolders.", hints{readOnly: true, idempotent: true},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			if _, err := s.daemon.Message.ListFolders(ctx, connect.NewRequest(&chainmailv1.ListFoldersRequest{})); err != nil {
				return nil, struct{}{}, s.errorResult(err)
			}
			return nil, struct{}{}, nil
		})
}

func TestDaemonUnreachable(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "mcpu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "absent.sock")
	s := New(Options{Socket: socket, Version: "test", AttachmentsDir: t.TempDir()})
	probeTool(s)
	cs := connectClient(t, s, latest)
	if names := listToolNames(t, cs); !slices.Contains(names, "probe") {
		t.Fatalf("tools/list = %v without a daemon, want the tools listed", names)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	body := wantError(t, res, api.ReasonDaemonUnreachable)
	if !strings.Contains(body.Message, socket) {
		t.Errorf("message %q does not name the socket %s", body.Message, socket)
	}
	for _, want := range []string{"systemctl status chainmail", "chainmail-clients", "sg chainmail-clients"} {
		if !strings.Contains(body.Hint, want) {
			t.Errorf("hint %q lacks %q", body.Hint, want)
		}
	}
}

func TestCallsCarryClientKindMCP(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{ListFolders: func(context.Context, *chainmailv1.ListFoldersRequest) (*chainmailv1.ListFoldersResponse, error) {
		return &chainmailv1.ListFoldersResponse{}, nil
	}}
	startFake(t, f)
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir()})
	probeTool(s)
	cs := connectClient(t, s, latest)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	wantOK(t, res)
	for _, c := range f.Calls() {
		if c.Client != "mcp" {
			t.Errorf("%s carried client kind %q, want mcp", c.Procedure, c.Client)
		}
	}
	f.lastCall(t, chainmailv1connect.MessageServiceListFoldersProcedure)
}

func TestClientCancellationCancelsTheDaemonCall(t *testing.T) {
	t.Parallel()
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := &fakeDaemon{ListFolders: func(ctx context.Context, _ *chainmailv1.ListFoldersRequest) (*chainmailv1.ListFoldersResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	startFake(t, f)
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir()})
	probeTool(s)
	cs := connectClient(t, s, latest)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "probe"}); err == nil {
		t.Error("a cancelled call returned no error")
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon call was not cancelled")
	}
}

func TestEndingRunCancelsTheDaemonCall(t *testing.T) {
	t.Parallel()
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := &fakeDaemon{ListFolders: func(ctx context.Context, _ *chainmailv1.ListFoldersRequest) (*chainmailv1.ListFoldersResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	startFake(t, f)
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir()})
	probeTool(s)
	serverT, clientT := mcp.NewInMemoryTransports()
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	ran := make(chan error, 1)
	go func() { ran <- s.Run(serverCtx, serverT) }()

	// The client's context is independent and never cancelled: the client
	// keeps its side of the stream open the whole time.
	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	c := mcp.NewClient(&mcp.Implementation{Name: "chainmail-test", Version: "0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	cs, err := c.Connect(clientCtx, clientT, &mcp.ClientSessionOptions{ProtocolVersion: latest})
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{})
	go func() {
		defer close(called)
		_, _ = cs.CallTool(clientCtx, &mcp.CallToolParams{Name: "probe"}) // its outcome is not the point
	}()
	t.Cleanup(func() {
		_ = cs.Close() // the session is over either way
		<-called
	})

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon call did not start")
	}
	stopServer()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("ending Run's context did not cancel the daemon call")
	}
	select {
	case err := <-ran:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestUnknownToolIsAProtocolError(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{}, latest)
	if _, err := e.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "no_such_tool"}); err == nil {
		t.Fatal("calling an unknown tool succeeded, want a JSON-RPC error")
	}
}

func TestDefaultAttachmentsDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "XDG_CACHE_HOME wins", env: map[string]string{"XDG_CACHE_HOME": "/x/cache", "HOME": "/home/u"}, want: "/x/cache/chainmail/attachments"},
		{name: "relative XDG_CACHE_HOME is ignored", env: map[string]string{"XDG_CACHE_HOME": "cache", "HOME": "/home/u"}, want: "/home/u/.cache/chainmail/attachments"},
		{name: "HOME fallback", env: map[string]string{"HOME": "/home/u"}, want: "/home/u/.cache/chainmail/attachments"},
		{name: "neither", env: map[string]string{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DefaultAttachmentsDir(func(k string) string { return tt.env[k] })
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("DefaultAttachmentsDir = %q, %v; want %q, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestGateInterval pins the spec's 60 second recheck of the policy-gated tools.
func TestGateInterval(t *testing.T) {
	t.Parallel()
	if gateInterval != 60*time.Second {
		t.Errorf("gateInterval = %v, want 60s", gateInterval)
	}
}

// TestHarnessRefs checks the shared test refs are distinct valid refs on account work.
func TestHarnessRefs(t *testing.T) {
	t.Parallel()
	if testRef == testRef2 {
		t.Fatalf("testRef and testRef2 are both %s", testRef)
	}
	for _, s := range []string{testRef, testRef2} {
		r, err := ref.Parse(s)
		if err != nil {
			t.Fatalf("ref.Parse(%q): %v", s, err)
		}
		if r.Account != "work" {
			t.Errorf("%s is on account %q, want work", s, r.Account)
		}
	}
}

// TestRunJoinsItsCancelCallback holds the callback that ends the requests in
// flight open, and checks Run cannot return until it is let go. The bounded
// wait can only miss a bug, never report a false one.
func TestRunJoinsItsCancelCallback(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	startFake(t, f)
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir()})
	entered, release := make(chan struct{}), make(chan struct{})
	s.beforeEnd = func() {
		close(entered)
		<-release
	}
	ctx, cancel := context.WithCancel(t.Context())
	serverT, _ := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, serverT) }()
	cancel()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) while its cancel callback was still running", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want nil or context.Canceled", err)
	}
}

// TestSDKDiagnosticsNeverReachTheLog: the SDK logs raw transport and session
// errors, and a client controls their text. A frame whose jsonrpc field is a
// secret must not put it in the stderr log.
func TestSDKDiagnosticsNeverReachTheLog(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	startFake(t, f)
	var logs syncBuffer
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	in := io.NopCloser(strings.NewReader(`{"jsonrpc":"` + sentinel + `","id":1,"method":"tools/list"}` + "\n"))
	err := s.Run(t.Context(), &mcp.IOTransport{Reader: in, Writer: nopWriteCloser{io.Discard}})
	if strings.Contains(logs.String(), sentinel) {
		t.Errorf("the stderr log holds client-controlled text: %q (Run = %v)", logs.String(), err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
