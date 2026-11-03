package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

// watchAt is the time every faked event carries.
var watchAt = time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)

// watchNoRead fails the test if the fake daemon received a ReadEvents call.
func watchNoRead(t *testing.T, f *fakeDaemon) {
	t.Helper()
	for _, c := range f.Calls() {
		if c.Procedure == chainmailv1connect.WatchServiceReadEventsProcedure {
			t.Errorf("the request reached the daemon: %v", c.Msg)
		}
	}
}

func TestReadEventsRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args map[string]any
		want *chainmailv1.ReadEventsRequest
	}{
		{
			name: "group with every filter",
			args: map[string]any{"group": "triage", "kinds": []string{"new", "flags"}, "accounts": []string{"work", "home"}, "in": "inbox", "wait_seconds": 60, "limit": 200},
			want: &chainmailv1.ReadEventsRequest{
				Group:       "triage",
				Filter:      &chainmailv1.WatchFilter{Accounts: []string{"work", "home"}, Kinds: []string{"new", "flags"}, In: "inbox"},
				WaitSeconds: 60, Limit: 200,
			},
		},
		{name: "group alone", args: map[string]any{"group": "triage"}, want: &chainmailv1.ReadEventsRequest{Group: "triage", Limit: 50}},
		{name: "defaults: no wait, limit 50, no filter", args: map[string]any{}, want: &chainmailv1.ReadEventsRequest{Limit: 50}},
		{name: "cursor", args: map[string]any{"cursor": 41}, want: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(41), Limit: 50}},
		{name: "cursor 0 reads from the start of the log", args: map[string]any{"cursor": 0}, want: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Limit: 50}},
		{name: "one filter field", args: map[string]any{"in": "Clients"}, want: &chainmailv1.ReadEventsRequest{Filter: &chainmailv1.WatchFilter{In: "Clients"}, Limit: 50}},
		{name: "every kind", args: map[string]any{"kinds": []string{"new", "flags", "expunged", "status", "gap"}}, want: &chainmailv1.ReadEventsRequest{
			Filter: &chainmailv1.WatchFilter{Kinds: []string{"new", "flags", "expunged", "status", "gap"}}, Limit: 50,
		}},
		{name: "limit 1", args: map[string]any{"limit": 1}, want: &chainmailv1.ReadEventsRequest{Limit: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest) // unfaked RPCs answer Unimplemented; the request is still recorded
			e.call(t, "read_events", tt.args)
			if got := e.fake.lastCall(t, chainmailv1connect.WatchServiceReadEventsProcedure).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadEventsRejectsBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      map[string]any
		inMessage string
	}{
		{name: "group with cursor", args: map[string]any{"group": "triage", "cursor": 5}, inMessage: "group or cursor"},
		{name: "negative cursor", args: map[string]any{"cursor": -1}, inMessage: "cursor must be 0 or more"},
		{name: "negative wait", args: map[string]any{"wait_seconds": -1}, inMessage: "wait_seconds must be 0 to 60"},
		{name: "wait above 60", args: map[string]any{"wait_seconds": 61}, inMessage: "wait_seconds must be 0 to 60"},
		{name: "negative limit", args: map[string]any{"limit": -1}, inMessage: "limit must be 1 to 200"},
		{name: "limit above 200", args: map[string]any{"limit": 201}, inMessage: "limit must be 1 to 200"},
		{name: "unknown kind", args: map[string]any{"kinds": []string{"new", "bounced"}}, inMessage: `unknown kind "bounced"`},
		{name: "kinds are lowercase", args: map[string]any{"kinds": []string{"New"}}, inMessage: `unknown kind "New"`},
		{name: "cache group", args: map[string]any{"group": "cache.work"}, inMessage: `"cache." are reserved`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			body := wantError(t, e.call(t, "read_events", tt.args), api.ReasonInvalidArgument)
			if !strings.Contains(body.Message, tt.inMessage) {
				t.Errorf("message %q lacks %q", body.Message, tt.inMessage)
			}
			watchNoRead(t, e.fake)
		})
	}
}

func TestReadEventsOutput(t *testing.T) {
	t.Parallel()
	ts := timestamppb.New(watchAt)
	f := &fakeDaemon{ReadEvents: func(context.Context, *chainmailv1.ReadEventsRequest) (*chainmailv1.ReadEventsResponse, error) {
		return &chainmailv1.ReadEventsResponse{NextCursor: 104, Events: []*chainmailv1.WatchEvent{
			{At: ts, Kind: "gap", Reason: "trimmed", NextId: 90},
			{Id: 101, At: ts, Kind: "new", Account: "work", Folder: "INBOX", Ref: testRef, Message: &chainmailv1.MessageRow{
				Ref: testRef, Account: "work", Folder: "INBOX", Date: ts,
				From:    []*chainmailv1.Address{{Name: readU("Ann"), Email: readU("ann@x.test")}},
				To:      []*chainmailv1.Address{{Email: readU("me@work.test")}},
				Subject: readU("Invoice 42"), Flags: []string{`\Seen`, "$Work"}, Labels: []string{`\Inbox`},
				Size: 2048, HasAttachment: true, ThreadId: "t1", Snippet: readU("pay by Friday"),
			}},
			{Id: 102, At: ts, Kind: "flags", Account: "work", Folder: "INBOX", Ref: testRef2, Flags: []string{`\Seen`, `\Flagged`, "$Work"}, Labels: []string{`\Inbox`, "Clients"}},
			{Id: 103, At: ts, Kind: "expunged", Account: "work", Folder: "INBOX", Ref: testRef2},
			{Id: 104, At: ts, Kind: "status", Account: "home", State: "auth_required", Detail: "run chainmail auth login home"},
		}}, nil
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "read_events", map[string]any{"group": "triage"})
	wantOK(t, res)

	var out readEventsOut
	decode(t, res, &out)
	const at = "2026-11-02T09:30:00Z"
	row := rowOut{
		Ref: testRef, Account: "work", Folder: serverName("INBOX"), Date: at,
		From: []untrusted{serverName("Ann <ann@x.test>")}, To: []untrusted{serverName("me@work.test")},
		Subject: serverName("Invoice 42"), Flags: []string{"seen"}, Keywords: []untrusted{serverName("$Work")},
		Labels: []untrusted{serverName(`\Inbox`)}, Size: 2048, HasAttachment: true, ThreadID: "t1", Snippet: serverNamePtr("pay by Friday"),
	}
	want := readEventsOut{NextCursor: 104, Events: []eventOut{
		{At: at, Kind: "gap", Reason: "trimmed", NextID: 90},
		{ID: 101, At: at, Kind: "new", Account: "work", Folder: serverNamePtr("INBOX"), Ref: testRef, Message: &row},
		{
			ID: 102, At: at, Kind: "flags", Account: "work", Folder: serverNamePtr("INBOX"), Ref: testRef2,
			Flags: []string{"seen", "flagged"}, Keywords: []untrusted{serverName("$Work")}, Labels: []untrusted{serverName(`\Inbox`), serverName("Clients")},
		},
		{ID: 103, At: at, Kind: "expunged", Account: "work", Folder: serverNamePtr("INBOX"), Ref: testRef2},
		{ID: 104, At: at, Kind: "status", Account: "home", State: "auth_required", Detail: "run chainmail auth login home"},
	}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("output =\n%+v\nwant\n%+v", out, want)
	}

	// Empty fields are omitted, so each kind carries only its own fields.
	var raw struct {
		Events []map[string]any `json:"events"`
	}
	decode(t, res, &raw)
	wantKeys := [][]string{
		{"at", "kind", "next_id", "reason"},
		{"account", "at", "folder", "id", "kind", "message", "ref"},
		{"account", "at", "flags", "folder", "id", "keywords", "kind", "labels", "ref"},
		{"account", "at", "folder", "id", "kind", "ref"},
		{"account", "at", "detail", "id", "kind", "state"},
	}
	if len(raw.Events) != len(wantKeys) {
		t.Fatalf("%d events, want %d", len(raw.Events), len(wantKeys))
	}
	for i, keys := range wantKeys {
		if got := slices.Sorted(maps.Keys(raw.Events[i])); !slices.Equal(got, keys) {
			t.Errorf("event %d keys = %v, want %v", i, got, keys)
		}
	}

	text := textOf(res)
	if !strings.HasPrefix(text, "5 events, next_cursor 104; group triage has acknowledged them") {
		t.Errorf("summary = %q", text)
	}
	if !strings.Contains(text, untrustedPreamble) || strings.Count(text, "<<<end chainmail-untrusted ") != 1 {
		t.Errorf("text is not wrapped once: %q", text)
	}
}

func TestReadEventsSummaryWithoutGroup(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{ReadEvents: func(context.Context, *chainmailv1.ReadEventsRequest) (*chainmailv1.ReadEventsResponse, error) {
		return &chainmailv1.ReadEventsResponse{NextCursor: 41}, nil
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "read_events", map[string]any{"cursor": 41})
	wantOK(t, res)
	var out readEventsOut
	decode(t, res, &out)
	if out.Events == nil || len(out.Events) != 0 || out.NextCursor != 41 {
		t.Errorf("output = %+v, want an empty events array and next_cursor 41", out)
	}
	if text := textOf(res); !strings.HasPrefix(text, "0 events, next_cursor 41; pass next_cursor as cursor to continue") {
		t.Errorf("summary = %q", text)
	}
}

func TestReadEventsOutputIsUntrusted(t *testing.T) {
	t.Parallel()
	ts := timestamppb.New(watchAt)
	f := &fakeDaemon{ReadEvents: func(context.Context, *chainmailv1.ReadEventsRequest) (*chainmailv1.ReadEventsResponse, error) {
		return &chainmailv1.ReadEventsResponse{NextCursor: 8, Events: []*chainmailv1.WatchEvent{
			{Id: 7, At: ts, Kind: "new", Account: "work", Folder: readHostile, Ref: testRef, Message: &chainmailv1.MessageRow{
				Ref: testRef, Account: "work", Folder: readHostile, Date: ts,
				From: []*chainmailv1.Address{{Email: readU("ann@x.test")}}, Subject: readU(readHostile),
			}},
			{Id: 8, At: ts, Kind: "flags", Account: "work", Folder: readHostile, Ref: testRef2, Flags: []string{readHostile}, Labels: []string{readHostile}},
		}}, nil
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "read_events", map[string]any{"group": "triage"})
	wantOK(t, res)
	// new: event folder, row folder, subject. flags: event folder, keyword, label.
	if n := readWrappedOnly(t, res.StructuredContent, readHostile); n != 6 {
		t.Errorf("%d enveloped hostile strings, want 6", n)
	}
	readInsideMarkers(t, textOf(res), readHostile)
}

func TestReadEventsDaemonErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                      string
		err                       error
		reason, inMessage, inHint string
	}{
		{
			name:   "forbidden with the daemon's detail",
			err:    api.New(connect.CodePermissionDenied, api.ReasonForbidden, "", errors.New("the socket peer is unknown")),
			reason: api.ReasonForbidden, inMessage: "the socket peer is unknown", inHint: "chainmail-clients",
		},
		{
			name:   "forbidden without a detail",
			err:    connect.NewError(connect.CodePermissionDenied, errors.New("peer not allowed")),
			reason: api.ReasonForbidden, inMessage: "refused this user", inHint: "chainmail-clients",
		},
		{
			name:   "a group name the daemon refuses",
			err:    api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "group names are lowercase letters, digits, dot, dash and underscore", errors.New(`invalid group name "Triage!"`)),
			reason: api.ReasonInvalidArgument, inMessage: "Triage!", inHint: "lowercase letters",
		},
		{
			name:   "an unknown account",
			err:    api.New(connect.CodeNotFound, api.ReasonNotFound, "run list_accounts", errors.New(`no account "nope"`)),
			reason: api.ReasonNotFound, inMessage: "nope", inHint: "list_accounts",
		},
		{
			name:   "a bare error gets fixed text",
			err:    errors.New("disk I/O error at /var/lib/chainmail/state.db"),
			reason: api.ReasonInternal, inMessage: "internal error", inHint: "tell James",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeDaemon{ReadEvents: func(context.Context, *chainmailv1.ReadEventsRequest) (*chainmailv1.ReadEventsResponse, error) {
				return nil, tt.err
			}}
			e := newTestEnv(t, f, latest)
			res := e.call(t, "read_events", map[string]any{"group": "triage"})
			body := wantError(t, res, tt.reason)
			if !strings.Contains(body.Message, tt.inMessage) || !strings.Contains(body.Hint, tt.inHint) {
				t.Errorf("error = %+v, want a message with %q and a hint with %q", body, tt.inMessage, tt.inHint)
			}
			if strings.Contains(textOf(res), "/var/lib/chainmail") {
				t.Errorf("the daemon's own error text leaked: %q", textOf(res))
			}
		})
	}
}

func TestReadEventsWithoutDaemon(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "mcpw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := New(Options{Socket: filepath.Join(dir, "none.sock"), Version: "test", AttachmentsDir: t.TempDir()})
	e := &testEnv{srv: s, cs: connectClient(t, s, latest)}
	wantError(t, e.call(t, "read_events", map[string]any{"group": "triage", "wait_seconds": 60}), api.ReasonDaemonUnreachable)
}

// TestReadEventsCancellationEndsTheWait cancels a read that waits 60
// seconds. The daemon's wait must end within the guard, long before the
// wait or the deadline would end it.
func TestReadEventsCancellationEndsTheWait(t *testing.T) {
	t.Parallel()
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := &fakeDaemon{ReadEvents: func(ctx context.Context, _ *chainmailv1.ReadEventsRequest) (*chainmailv1.ReadEventsResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	e := newTestEnv(t, f, latest)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	params := &mcp.CallToolParams{Name: "read_events", Arguments: map[string]any{"group": "triage", "wait_seconds": 60}}
	if _, err := e.cs.CallTool(ctx, params); err == nil {
		t.Error("a cancelled call returned no error")
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the request did not end the daemon's wait")
	}
}

func TestReadEventsAnnotations(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == "read_events" })
	if i < 0 {
		t.Fatal("read_events is not listed")
	}
	tl := res.Tools[i]
	a := tl.Annotations
	switch {
	case tl.Title != "Read mail events" || a == nil || a.Title != tl.Title:
		t.Errorf("title %q, annotations %+v", tl.Title, a)
	case a.ReadOnlyHint || a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || !*a.OpenWorldHint:
		t.Errorf("hints %+v, want not read-only, not destructive, not idempotent, open world: a group read moves the group", a)
	}
	for _, want := range []string{`read_events(group: "<name>", wait_seconds: 60)`, "untrusted", "acknowledges what it returns", "auth_required", "James"} {
		if !strings.Contains(tl.Description, want) {
			t.Errorf("description lacks %q: %q", want, tl.Description)
		}
	}
}

// TestReadEventsDeadlineCoversTheWait checks the daemon call carries a
// deadline past wait_seconds, by at most readEventsMargin. Connect sends the
// deadline as a timeout header, and the fake's handler context has it.
func TestReadEventsDeadlineCoversTheWait(t *testing.T) {
	t.Parallel()
	for _, wait := range []int{0, 60} {
		t.Run(fmt.Sprintf("wait %ds", wait), func(t *testing.T) {
			t.Parallel()
			type seen struct {
				left time.Duration
				ok   bool
			}
			got := make(chan seen, 1)
			f := &fakeDaemon{ReadEvents: func(ctx context.Context, _ *chainmailv1.ReadEventsRequest) (*chainmailv1.ReadEventsResponse, error) {
				d, ok := ctx.Deadline()
				got <- seen{left: time.Until(d), ok: ok}
				return &chainmailv1.ReadEventsResponse{}, nil
			}}
			e := newTestEnv(t, f, latest)
			wantOK(t, e.call(t, "read_events", map[string]any{"group": "triage", "wait_seconds": wait}))
			s := <-got
			w := time.Duration(wait) * time.Second
			switch {
			case !s.ok:
				t.Error("the daemon call has no deadline")
			case s.left <= w || s.left > w+readEventsMargin:
				t.Errorf("deadline in %v, want more than %v and at most %v", s.left, w, w+readEventsMargin)
			}
		})
	}
}
