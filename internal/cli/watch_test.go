package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// fakeClock is Env.After for watch tests. A wait whose duration instant
// accepts fires at once; any other wait fires only when the test calls fire.
type fakeClock struct {
	instant func(time.Duration) bool
	changed chan struct{} // signalled after each After

	mu      sync.Mutex
	asked   []time.Duration
	pending map[time.Duration][]chan time.Time
}

func newFakeClock(instant func(time.Duration) bool) *fakeClock {
	return &fakeClock{instant: instant, changed: make(chan struct{}, 1), pending: map[time.Duration][]chan time.Time{}}
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.asked = append(c.asked, d)
	if c.instant != nil && c.instant(d) {
		ch <- time.Time{}
	} else {
		c.pending[d] = append(c.pending[d], ch)
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return ch
}

// fire ends every wait for d asked for so far.
func (c *fakeClock) fire(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.pending[d] {
		ch <- time.Time{}
	}
	delete(c.pending, d)
}

// count is how many waits for d the watch has asked for.
func (c *fakeClock) count(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, a := range c.asked {
		if a == d {
			n++
		}
	}
	return n
}

// waitCount blocks until the watch has asked for at least n waits for d.
func (c *fakeClock) waitCount(t *testing.T, d time.Duration, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for c.count(d) < n {
		select {
		case <-c.changed:
		case <-deadline:
			t.Fatalf("the watch asked for %d waits of %v, want %d", c.count(d), d, n)
		}
	}
}

func eventItem(e *chainmailv1.WatchEvent) *chainmailv1.SubscribeResponse {
	return &chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Event{Event: e}}
}

func heartbeatItem(position int64) *chainmailv1.SubscribeResponse {
	return &chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Heartbeat{
		Heartbeat: &chainmailv1.Heartbeat{At: timestamppb.New(watchAt.Add(30 * time.Second)), Position: position},
	}}
}

func newEvent(id int64, subject string) *chainmailv1.WatchEvent {
	ref := fmt.Sprintf("cm1:host:INBOX:1:%d", id)
	return &chainmailv1.WatchEvent{
		Id: id, At: timestamppb.New(watchAt.Add(time.Duration(id) * time.Second)), Kind: "new",
		Account: "host", Folder: "INBOX", Ref: ref,
		Message: &chainmailv1.MessageRow{
			Ref: ref, Account: "host", Folder: "INBOX",
			Date: timestamppb.New(watchAt), InternalDate: timestamppb.New(watchAt),
			From: []*chainmailv1.Address{{
				Name:  &chainmailv1.Untrusted{Untrusted: "Sam"},
				Email: &chainmailv1.Untrusted{Untrusted: "sam@example.org"},
			}},
			Subject: &chainmailv1.Untrusted{Untrusted: subject},
			Size:    300,
		},
	}
}

func flagsEvent(id int64) *chainmailv1.WatchEvent {
	return &chainmailv1.WatchEvent{
		Id: id, At: timestamppb.New(watchAt.Add(time.Duration(id) * time.Second)), Kind: "flags",
		Account: "host", Folder: "INBOX", Ref: "cm1:host:INBOX:1:1",
		Flags: []string{`\Seen`}, Labels: []string{"Work"},
	}
}

func expungedEvent(id int64, ref string) *chainmailv1.WatchEvent {
	return &chainmailv1.WatchEvent{
		Id: id, At: timestamppb.New(watchAt.Add(time.Duration(id) * time.Second)), Kind: "expunged",
		Account: "host", Folder: "INBOX", Ref: ref,
	}
}

func statusEvent(id int64, folder, state, detail string) *chainmailv1.WatchEvent {
	return &chainmailv1.WatchEvent{
		Id: id, At: timestamppb.New(watchAt.Add(time.Duration(id) * time.Second)), Kind: "status",
		Account: "host", Folder: folder, State: state, Detail: detail,
	}
}

// gapEvent is a trimmed gap as the daemon generates it: no ID and no time.
func gapEvent(next int64) *chainmailv1.WatchEvent {
	return &chainmailv1.WatchEvent{Kind: "gap", Reason: "trimmed", NextId: next}
}

// playThen sends items, then ends the stream with end, or holds it open
// until the client leaves when end is nil.
func playThen(end error, items ...*chainmailv1.SubscribeResponse) watchScript {
	return func(ctx context.Context, send func(*chainmailv1.SubscribeResponse) error) error {
		for _, it := range items {
			if err := send(it); err != nil {
				return err
			}
		}
		if end != nil {
			return end
		}
		<-ctx.Done()
		return nil
	}
}

// authRequired is the error a daemon would repeat: the watch exits 5 on it.
func authRequired() error {
	return api.New(connect.CodeUnauthenticated, api.ReasonAuthRequired, "run: chainmail auth login host", errors.New("account host needs a login"))
}

// position names where a SubscribeRequest starts.
func position(r *chainmailv1.SubscribeRequest) string {
	switch {
	case r.GetGroup() != "" && r.Since != nil:
		return "group+since"
	case r.GetGroup() != "":
		return "group=" + r.GetGroup()
	case r.Since != nil:
		return fmt.Sprintf("since=%d", r.GetSince())
	default:
		return "live"
	}
}

func TestWatchJSONLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		golden string
	}{
		{name: "events", args: []string{"watch", "--once"}, golden: "watch_json"},
		{name: "with heartbeats", args: []string{"watch", "--once", "--heartbeat"}, golden: "watch_heartbeat_json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWatch(playThen(nil,
				eventItem(newEvent(1, "Hello")), eventItem(flagsEvent(2)),
				eventItem(statusEvent(3, "INBOX", "watching", "")), heartbeatItem(3)))
			socket := startFakeWatch(t, f)
			got := runCLI(t, Env{After: newFakeClock(nil).After}, append([]string{"--socket", socket}, tt.args...)...)
			if got.code != 0 {
				t.Fatalf("watch: %+v", got)
			}
			golden(t, tt.golden, got.stdout)
			reqs := f.requests()
			if len(reqs) != 1 || position(reqs[0]) != "live" {
				t.Errorf("requests = %v, want one live Subscribe", reqs)
			}
			if acks := f.ackIDs(t, ""); len(acks) != 0 {
				t.Errorf("acks = %v, want none without --group", acks)
			}
		})
	}
}

func TestWatchTable(t *testing.T) {
	t.Parallel()
	f := newFakeWatch(playThen(nil,
		eventItem(newEvent(1, "Hello")),
		eventItem(flagsEvent(2)),
		eventItem(expungedEvent(3, "cm1:host:INBOX:1:2")),
		eventItem(statusEvent(4, "", "auth_required", "run: chainmail auth login host")),
		eventItem(gapEvent(9)),
		eventItem(newEvent(5, "Bad\x1b[31m news\nline")),
		heartbeatItem(5)))
	socket := startFakeWatch(t, f)
	env := Env{
		After: newFakeClock(nil).After,
		TZ:    time.FixedZone("CET", 3600),
		Now:   func() time.Time { return watchAt.Add(5 * time.Second) },
	}
	got := runCLI(t, env, "--socket", socket, "--output", "table", "watch", "--once", "--heartbeat")
	if got.code != 0 {
		t.Fatalf("watch: %+v", got)
	}
	golden(t, "watch_table", got.stdout)
}

func TestWatchOnceExitsAtFirstHeartbeat(t *testing.T) {
	t.Parallel()
	f := newFakeWatch(playThen(nil, heartbeatItem(4), eventItem(newEvent(5, "Late"))))
	socket := startFakeWatch(t, f)
	got := runCLI(t, Env{After: newFakeClock(nil).After}, "--socket", socket, "watch", "--once")
	if got.code != 0 || got.stdout != "" {
		t.Errorf("watch --once: %+v, want exit 0 and nothing printed", got)
	}
}

func TestWatchFlagsPassThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		position string
		filter   *chainmailv1.WatchFilter
	}{
		{name: "defaults", position: "live", filter: &chainmailv1.WatchFilter{}},
		{
			name:     "every filter",
			args:     []string{"--account", "host", "--account", "icloud", "--kind", "new, flags", "--in", "inbox", "--since", "5"},
			position: "since=5",
			filter:   &chainmailv1.WatchFilter{Accounts: []string{"host", "icloud"}, Kinds: []string{"new", "flags"}, In: "inbox"},
		},
		{name: "group", args: []string{"--group", "bot"}, position: "group=bot", filter: &chainmailv1.WatchFilter{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWatch(playThen(authRequired()))
			socket := startFakeWatch(t, f)
			args := append([]string{"--socket", socket, "watch"}, tt.args...)
			got := runCLI(t, Env{After: newFakeClock(nil).After}, args...)
			if got.code != api.ExitAuth {
				t.Fatalf("watch: %+v, want exit 5", got)
			}
			req := f.requests()[0]
			if p := position(req); p != tt.position {
				t.Errorf("position = %s, want %s", p, tt.position)
			}
			fl := req.GetFilter()
			if !slices.Equal(fl.GetAccounts(), tt.filter.GetAccounts()) || !slices.Equal(fl.GetKinds(), tt.filter.GetKinds()) || fl.GetIn() != tt.filter.GetIn() {
				t.Errorf("filter = %v, want %v", fl, tt.filter)
			}
		})
	}
}

// Errors the daemon would repeat end the watch with their exit code, at
// once and without a reconnect.
func TestWatchExitsOnRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   int
		reason string
	}{
		{name: "auth required", err: authRequired(), code: api.ExitAuth, reason: api.ReasonAuthRequired},
		{
			name:   "invalid argument",
			err:    api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "group names are lowercase letters, digits, dot, dash and underscore", errors.New("bad group")),
			code:   api.ExitUsage,
			reason: api.ReasonInvalidArgument,
		},
		{name: "forbidden", err: connect.NewError(connect.CodePermissionDenied, errors.New("peer refused")), code: api.ExitDenied, reason: api.ReasonForbidden},
		{name: "unknown account", err: api.New(connect.CodeNotFound, api.ReasonNotFound, "", errors.New("no account nope")), code: api.ExitNotFound, reason: api.ReasonNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWatch(playThen(tt.err))
			socket := startFakeWatch(t, f)
			got := runCLI(t, Env{After: newFakeClock(nil).After}, "--socket", socket, "watch")
			if got.code != tt.code || errorReason(t, got.stderr) != tt.reason {
				t.Errorf("watch: %+v, want exit %d %s", got, tt.code, tt.reason)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("%d Subscribe calls, want 1", n)
			}
		})
	}
}

func TestWatchUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "group and since", args: []string{"watch", "--group", "bot", "--since", "5"}, want: "--group and --since"},
		{name: "negative since", args: []string{"watch", "--since", "-1"}, want: "--since must not be negative"},
		{name: "unknown kind", args: []string{"watch", "--kind", "new,bogus"}, want: "unknown --kind"},
		{name: "bad bool", args: []string{"watch", "--once=maybe"}, want: "--once"},
		{name: "watch argument", args: []string{"watch", "extra"}, want: "accepts 0 arg(s)"},
		{name: "status argument", args: []string{"watch", "status", "extra"}, want: "accepts 0 arg(s)"},
		{name: "unknown groups subcommand", args: []string{"watch", "groups", "nope"}, want: "accepts 0 arg(s)"},
		{name: "delete without a name", args: []string{"watch", "groups", "delete"}, want: "accepts 1 arg(s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, append([]string{"--socket", "/nonexistent.sock"}, tt.args...)...)
			if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
				t.Fatalf("%v: %+v, want a usage error", tt.args, got)
			}
			if !strings.Contains(got.stderr, tt.want) {
				t.Errorf("stderr = %s, want it to mention %q", got.stderr, tt.want)
			}
		})
	}
}
