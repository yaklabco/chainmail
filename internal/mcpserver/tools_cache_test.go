package mcpserver

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// cacheAt is when the faked cache last applied a change.
var cacheAt = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// cacheStatusAnswer is a daemon cache status: one account with a folder,
// one disabled with none.
func cacheStatusAnswer(context.Context, *chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
	return &chainmailv1.GetCacheStatusResponse{SizeBytes: 12 << 20, MaxSizeBytes: 2 << 30, Accounts: []*chainmailv1.AccountCacheStatus{
		{
			Account: "work", Enabled: true, State: "backfilling", BudgetUsedBytes: 4096, BudgetBytes: 1 << 30,
			Folders: []*chainmailv1.FolderCacheStatus{{
				Folder: "INBOX", State: "backfilling", Headers: 1200, BaselineUid: 9000, BackfillLowUid: 4100,
				Bodies: 300, BodyBytes: 1 << 20, WindowMessages: 310, WindowBodies: 300, BodyFailures: 2,
				LastAppliedAt: timestamppb.New(cacheAt),
			}},
		},
		{Account: "old", State: "disabled", BudgetBytes: 1 << 30, LastError: "AUTH_REQUIRED"},
	}}, nil
}

func TestCacheStatusRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args map[string]any
		want *chainmailv1.GetCacheStatusRequest
	}{
		{name: "every account", args: map[string]any{}, want: &chainmailv1.GetCacheStatusRequest{}},
		{name: "named", args: map[string]any{"accounts": []string{"work", "home"}}, want: &chainmailv1.GetCacheStatusRequest{Accounts: []string{"work", "home"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{GetCacheStatus: cacheStatusAnswer}, latest)
			wantOK(t, e.call(t, "cache_status", tt.args))
			if got := e.fake.lastCall(t, chainmailv1connect.CacheServiceGetCacheStatusProcedure).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCacheStatusOutput(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{GetCacheStatus: cacheStatusAnswer}, latest)
	res := e.call(t, "cache_status", nil)
	wantOK(t, res)
	var out cacheStatusOut
	decode(t, res, &out)
	want := cacheStatusOut{SizeBytes: 12 << 20, MaxSizeBytes: 2 << 30, Accounts: []cacheAccountOut{
		{
			Account: "work", Enabled: true, State: "backfilling", BudgetUsedBytes: 4096, BudgetBytes: 1 << 30,
			Folders: []cacheFolderOut{{
				Folder: untrusted{Untrusted: "INBOX"}, State: "backfilling", Headers: 1200, BaselineUID: 9000, BackfillLowUID: 4100,
				Bodies: 300, BodyBytes: 1 << 20, WindowMessages: 310, WindowBodies: 300, BodyFailures: 2,
				LastAppliedAt: "2026-11-02T09:00:00Z",
			}},
		},
		{Account: "old", State: "disabled", BudgetBytes: 1 << 30, LastError: "AUTH_REQUIRED", Folders: []cacheFolderOut{}},
	}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("cache_status =\n%+v\nwant\n%+v", out, want)
	}
	if text := textOf(res); !strings.HasPrefix(text, "cache of 2 accounts: 12582912 of 2147483648 bytes\n") {
		t.Errorf("text = %q", text)
	}
}

// TestCacheStatusWrapsFolderNames: a folder name comes from the mail
// server, which can name a folder like an instruction, so it reaches the
// client only as untrusted data in both representations.
func TestCacheStatusWrapsFolderNames(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{GetCacheStatus: func(context.Context, *chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
		return &chainmailv1.GetCacheStatusResponse{Accounts: []*chainmailv1.AccountCacheStatus{
			{Account: "work", Enabled: true, State: "ready", Folders: []*chainmailv1.FolderCacheStatus{{Folder: readHostile, State: "ready"}}},
		}}, nil
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "cache_status", nil)
	wantOK(t, res)
	if n := readWrappedOnly(t, res.StructuredContent, readHostile); n != 1 {
		t.Errorf("%d enveloped hostile strings, want 1", n)
	}
	readInsideMarkers(t, textOf(res), readHostile)
}

func TestCacheStatusPassesDaemonErrors(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{GetCacheStatus: func(context.Context, *chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
		return nil, api.New(connect.CodeNotFound, api.ReasonNotFound, "run list_accounts", errNoMessage)
	}}
	e := newTestEnv(t, f, latest)
	if b := wantError(t, e.call(t, "cache_status", map[string]any{"accounts": []string{"nope"}}), api.ReasonNotFound); b.Hint != "run list_accounts" {
		t.Errorf("hint = %q", b.Hint)
	}
}

// TestCacheStatusAnnotations pins the tool's title, hints and description,
// and that rebuild and evict have no tools.
func TestCacheStatusAnnotations(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == "cache_status" })
	if i < 0 {
		t.Fatal("cache_status is not listed")
	}
	tl := res.Tools[i]
	a := tl.Annotations
	switch {
	case tl.Title != "Show the mail cache" || a == nil || a.Title != tl.Title:
		t.Errorf("title %q, annotations %+v", tl.Title, a)
	case !a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint:
		t.Errorf("hints %+v, want read-only, idempotent, not destructive, closed world", a)
	}
	for _, want := range []string{"backfilling", "budget", "source server", "incomplete_reason", "untrusted"} {
		if !strings.Contains(tl.Description, want) {
			t.Errorf("description lacks %q: %q", want, tl.Description)
		}
	}
	for _, tool := range res.Tools {
		if strings.Contains(tool.Name, "rebuild") || strings.Contains(tool.Name, "evict") {
			t.Errorf("operator action %s is a tool", tool.Name)
		}
	}
}
