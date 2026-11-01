package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
)

func TestErrorResultMapping(t *testing.T) {
	t.Parallel()
	s := New(Options{Socket: "/run/chainmail/chainmail.sock", Version: "test"})
	tests := []struct {
		name        string
		err         error
		wantReason  string
		wantMessage string // substring
		wantHint    string // substring
	}{
		{
			name:       "detail reason and hint pass through",
			err:        api.New(connect.CodeNotFound, api.ReasonNotFound, "run list_folders to see the folder names", errors.New(`no folder "Projekts"`)),
			wantReason: api.ReasonNotFound, wantMessage: `no folder "Projekts"`, wantHint: "list_folders",
		},
		{
			name:       "policy denied",
			err:        api.New(connect.CodePermissionDenied, api.ReasonPolicyDenied, "ask James to enable it", errors.New("expunge is disabled by policy")),
			wantReason: api.ReasonPolicyDenied, wantMessage: "expunge is disabled", wantHint: "James",
		},
		{
			name:       "unreachable names the socket and the group",
			err:        connect.NewError(connect.CodeUnavailable, errors.New("dial unix /run/chainmail/chainmail.sock: connect: no such file or directory")),
			wantReason: api.ReasonDaemonUnreachable, wantMessage: "/run/chainmail/chainmail.sock", wantHint: "systemctl status chainmail",
		},
		{
			name:       "a refused peer is told about the group",
			err:        connect.NewError(connect.CodePermissionDenied, errors.New("peer not allowed")),
			wantReason: api.ReasonForbidden, wantMessage: "peer not allowed", wantHint: "chainmail-clients",
		},
		{
			name:       "an error without a Connect code is internal",
			err:        errors.New("boom"),
			wantReason: api.ReasonInternal, wantMessage: "boom",
		},
		{
			name:       "a tool error passes through unchanged",
			err:        invalidArg("dir must be an absolute path, got %q", "tmp"),
			wantReason: api.ReasonInvalidArgument, wantMessage: `got "tmp"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var te *toolError
			if !errors.As(s.errorResult(tt.err), &te) {
				t.Fatal("errorResult did not return a *toolError")
			}
			b := te.body
			if b.Reason != tt.wantReason || !strings.Contains(b.Message, tt.wantMessage) || !strings.Contains(b.Hint, tt.wantHint) {
				t.Errorf("body = %+v, want reason %s, message ~%q, hint ~%q", b, tt.wantReason, tt.wantMessage, tt.wantHint)
			}
			if !strings.HasPrefix(te.Error(), tt.wantReason+": ") {
				t.Errorf("text = %q, want it to start with the reason", te.Error())
			}
		})
	}
}

func TestItemError(t *testing.T) {
	t.Parallel()
	if itemError(nil) != nil {
		t.Error("itemError(nil) is not nil")
	}
	got := itemError(&chainmailv1.ItemError{Reason: api.ReasonStaleRef, Message: "gone", Hint: "search again"})
	if *got != (errorBody{Reason: api.ReasonStaleRef, Message: "gone", Hint: "search again"}) {
		t.Errorf("itemError = %+v", got)
	}
}

func TestErrorResultsCarryStructuredContent(t *testing.T) {
	t.Parallel()
	type refIn struct {
		Ref string `json:"ref" jsonschema:"a ref"`
	}
	f := &fakeDaemon{}
	startFake(t, f)
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: t.TempDir()})
	addTool(s, "fails", "Fails", "Test only: always fails.", hints{readOnly: true, idempotent: true},
		func(context.Context, *mcp.CallToolRequest, refIn) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, s.errorResult(api.New(connect.CodeNotFound, api.ReasonNotFound, "search again", errors.New("no such message")))
		})
	cs := connectClient(t, s, latest)
	e := &testEnv{fake: f, srv: s, cs: cs}
	tests := []struct {
		name       string
		args       map[string]any
		wantReason string
		wantText   []string
	}{
		{name: "a daemon error", args: map[string]any{"ref": testRef}, wantReason: api.ReasonNotFound, wantText: []string{"NOT_FOUND: no such message", "hint: search again"}},
		{name: "arguments the schema rejects", args: map[string]any{}, wantReason: api.ReasonInvalidArgument, wantText: []string{"ref"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.call(t, "fails", tt.args)
			wantError(t, res, tt.wantReason)
			for _, w := range tt.wantText {
				if !strings.Contains(textOf(res), w) {
					t.Errorf("text %q lacks %q", textOf(res), w)
				}
			}
		})
	}
}
