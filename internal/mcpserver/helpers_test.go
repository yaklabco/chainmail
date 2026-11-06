package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yaklabco/chainmail/internal/ref"
)

const (
	latest = "2026-07-28"
	legacy = "2025-11-25"
)

var (
	testRef  = ref.Ref{Account: "work", Folder: "INBOX", UIDValidity: 9, UID: 42}.String()
	testRef2 = ref.Ref{Account: "work", Folder: "INBOX", UIDValidity: 9, UID: 43}.String()
)

// testEnv is a fake daemon, a Server dialing it and a connected client.
type testEnv struct {
	fake *fakeDaemon
	srv  *Server
	cs   *mcp.ClientSession
	dir  string // the Server's AttachmentsDir
}

// newTestEnv starts f and a Server on it. Each mod adjusts the Options
// before New, for example to set Now.
func newTestEnv(t *testing.T, f *fakeDaemon, version string, mods ...func(*Options)) *testEnv {
	t.Helper()
	startFake(t, f)
	dir := t.TempDir()
	opts := Options{Socket: f.Socket, Version: "test", AttachmentsDir: dir}
	for _, m := range mods {
		m(&opts)
	}
	s := New(opts)
	return &testEnv{fake: f, srv: s, cs: connectClient(t, s, version), dir: dir}
}

// connectClient runs s on an in-memory transport and connects a client that
// asks for version. Cleanup closes the client, then stops s.
func connectClient(t *testing.T, s *Server, version string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, serverT) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	})
	c := mcp.NewClient(&mcp.Implementation{Name: "chainmail-test", Version: "0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	cs, err := c.Connect(ctx, clientT, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("connecting at %s: %v", version, err)
	}
	t.Cleanup(func() { _ = cs.Close() }) // the test is over; Run's result is not checked
	return cs
}

func (e *testEnv) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := e.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return res
}

// decode re-encodes the structured content into out.
func decode(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("structured content %s: %v", b, err)
	}
}

func wantOK(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	if res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
}

// wantError checks res is an error result with reason in its text and its
// structured content, and returns the structured error.
func wantError(t *testing.T, res *mcp.CallToolResult, reason string) errorBody {
	t.Helper()
	if !res.IsError {
		t.Fatalf("want an error result with %s, got: %s", reason, textOf(res))
	}
	var out struct {
		Error errorBody `json:"error"`
	}
	decode(t, res, &out)
	if out.Error.Reason != reason {
		t.Errorf("reason = %q, want %q (text %q)", out.Error.Reason, reason, textOf(res))
	}
	if !strings.Contains(textOf(res), reason) {
		t.Errorf("text %q does not name %s", textOf(res), reason)
	}
	return out.Error
}

func listToolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}
