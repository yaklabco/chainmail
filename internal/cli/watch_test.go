package cli

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// watchAt is when the scripted events happen.
var watchAt = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// watchScript plays one Subscribe stream on the fake daemon.
type watchScript func(ctx context.Context, send func(*chainmailv1.SubscribeResponse) error) error

// fakeWatch is a WatchService for CLI tests. The nth Subscribe call plays
// streams[n]. A call past the end fails with INVALID_ARGUMENT, so a watch
// that reconnects more often than a test expects exits instead of hanging.
type fakeWatch struct {
	chainmailv1connect.UnimplementedWatchServiceHandler

	streams     []watchScript
	status      func() (*chainmailv1.GetWatchStatusResponse, error)
	groups      func() (*chainmailv1.ListGroupsResponse, error)
	deleteGroup func(name string) (*chainmailv1.DeleteGroupResponse, error)

	acked chan struct{} // signalled after each Ack

	mu   sync.Mutex
	subs []*chainmailv1.SubscribeRequest
	acks []*chainmailv1.AckRequest
}

func newFakeWatch(streams ...watchScript) *fakeWatch {
	return &fakeWatch{streams: streams, acked: make(chan struct{}, 1)}
}

func (f *fakeWatch) Subscribe(ctx context.Context, req *connect.Request[chainmailv1.SubscribeRequest], stream *connect.ServerStream[chainmailv1.SubscribeResponse]) error {
	f.mu.Lock()
	n := len(f.subs)
	f.subs = append(f.subs, req.Msg)
	f.mu.Unlock()
	if n >= len(f.streams) {
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "", fmt.Errorf("unexpected Subscribe call %d", n+1))
	}
	return f.streams[n](ctx, stream.Send)
}

func (f *fakeWatch) Ack(_ context.Context, req *connect.Request[chainmailv1.AckRequest]) (*connect.Response[chainmailv1.AckResponse], error) {
	f.mu.Lock()
	f.acks = append(f.acks, req.Msg)
	f.mu.Unlock()
	select {
	case f.acked <- struct{}{}:
	default:
	}
	return connect.NewResponse(&chainmailv1.AckResponse{}), nil
}

func (f *fakeWatch) GetWatchStatus(context.Context, *connect.Request[chainmailv1.GetWatchStatusRequest]) (*connect.Response[chainmailv1.GetWatchStatusResponse], error) {
	resp, err := f.status()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeWatch) ListGroups(context.Context, *connect.Request[chainmailv1.ListGroupsRequest]) (*connect.Response[chainmailv1.ListGroupsResponse], error) {
	resp, err := f.groups()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeWatch) DeleteGroup(_ context.Context, req *connect.Request[chainmailv1.DeleteGroupRequest]) (*connect.Response[chainmailv1.DeleteGroupResponse], error) {
	resp, err := f.deleteGroup(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// requests returns the Subscribe requests so far.
func (f *fakeWatch) requests() []*chainmailv1.SubscribeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subs)
}

// ackIDs returns the acknowledged IDs so far, and fails the test on an
// Ack for any group but group.
func (f *fakeWatch) ackIDs(t *testing.T, group string) []int64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.acks))
	for _, a := range f.acks {
		if a.GetGroup() != group {
			t.Errorf("Ack for group %q, want %q", a.GetGroup(), group)
		}
		ids = append(ids, a.GetId())
	}
	return ids
}

// waitAcks blocks until the fake has had at least n Ack calls.
func (f *fakeWatch) waitAcks(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.acks)
		f.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-f.acked:
		case <-deadline:
			t.Fatalf("%d Ack calls, want %d", got, n)
		}
	}
}

// startFakeWatch serves f on a Unix socket until the test ends and returns the socket.
func startFakeWatch(t *testing.T, f *fakeWatch) string {
	t.Helper()
	path, handler := chainmailv1connect.NewWatchServiceHandler(f)
	return startFakeServer(t, path, handler)
}

func watchStatusFixture() *chainmailv1.GetWatchStatusResponse {
	return &chainmailv1.GetWatchStatusResponse{Folders: []*chainmailv1.FolderSyncStatus{
		{
			Account: "host", Folder: "INBOX", Mode: "idle", State: "watching", Condstore: true,
			LastSyncAt:  timestamppb.New(watchAt),
			LastSweepAt: timestamppb.New(watchAt.Add(-30 * time.Minute)),
		},
		{
			Account: "host", Folder: "Sent", Mode: "poll", State: "error",
			LastSyncAt: timestamppb.New(watchAt.Add(-5 * time.Minute)),
			LastError:  "FOLDER_NOT_FOUND",
		},
	}}
}

func watchGroupsFixture() *chainmailv1.ListGroupsResponse {
	return &chainmailv1.ListGroupsResponse{Groups: []*chainmailv1.GroupStatus{
		{
			Name: "nightly", AckedId: 120, Lag: 3,
			CreatedAt:  timestamppb.New(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)),
			LastSeenAt: timestamppb.New(watchAt.Add(-time.Minute)),
		},
		{
			Name: "bot", AckedId: 0, Lag: 123,
			CreatedAt:  timestamppb.New(watchAt.Add(-3 * time.Hour)),
			LastSeenAt: timestamppb.New(watchAt),
		},
	}}
}

func TestWatchStatusAndGroups(t *testing.T) {
	t.Parallel()
	f := newFakeWatch()
	f.status = func() (*chainmailv1.GetWatchStatusResponse, error) { return watchStatusFixture(), nil }
	f.groups = func() (*chainmailv1.ListGroupsResponse, error) { return watchGroupsFixture(), nil }
	socket := startFakeWatch(t, f)
	tests := []struct {
		command, output, golden string
	}{
		{command: "status", output: "json", golden: "watch_status_json"},
		{command: "status", output: "table", golden: "watch_status_table"},
		{command: "groups", output: "json", golden: "watch_groups_json"},
		{command: "groups", output: "table", golden: "watch_groups_table"},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "watch", tt.command)
			if got.code != 0 {
				t.Fatalf("watch %s: %+v", tt.command, got)
			}
			golden(t, tt.golden, got.stdout)
		})
	}
}

func TestWatchGroupsDelete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		deleted bool
		want    string
	}{
		{name: "json deleted", output: "json", deleted: true, want: `{"deleted":true}` + "\n"},
		{name: "json missing", output: "json", deleted: false, want: `{"deleted":false}` + "\n"},
		{name: "table deleted", output: "table", deleted: true, want: "deleted group nightly\n"},
		{name: "table missing", output: "table", deleted: false, want: "no group named nightly\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var names []string
			f := newFakeWatch()
			f.deleteGroup = func(name string) (*chainmailv1.DeleteGroupResponse, error) {
				mu.Lock()
				defer mu.Unlock()
				names = append(names, name)
				return &chainmailv1.DeleteGroupResponse{Deleted: tt.deleted}, nil
			}
			socket := startFakeWatch(t, f)
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "watch", "groups", "delete", "nightly")
			if got.code != 0 {
				t.Fatalf("watch groups delete: %+v", got)
			}
			if got.stdout != tt.want {
				t.Errorf("stdout = %q, want %q", got.stdout, tt.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(names, []string{"nightly"}) {
				t.Errorf("DeleteGroup names = %q, want [nightly]", names)
			}
		})
	}
}

func TestWatchStatusReportsDaemonErrors(t *testing.T) {
	t.Parallel()
	f := newFakeWatch()
	f.status = func() (*chainmailv1.GetWatchStatusResponse, error) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("peer refused"))
	}
	socket := startFakeWatch(t, f)
	got := runCLI(t, Env{}, "--socket", socket, "watch", "status")
	if got.code != api.ExitDenied || errorReason(t, got.stderr) != api.ReasonForbidden {
		t.Errorf("watch status: %+v, want exit 3 FORBIDDEN", got)
	}
}
