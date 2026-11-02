package daemon_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/daemon"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/watch"
)

const watchConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.work]
provider = "hostinger"
address = "me@work.example"
`

// groupHint is the daemon's hint for an invalid group name (contract C8).
const groupHint = "group names are lowercase letters, digits, dot, dash and underscore"

// startWatch runs a daemon with watchConfig and the given heartbeat, where
// 0 means the 30 second default. Supervisors stay off.
func startWatch(t *testing.T, heartbeat time.Duration) *daemontest.Harness {
	t.Helper()
	return daemontest.Start(t, watchConfig, daemontest.Options{WatchHeartbeat: heartbeat})
}

// watchLog is a Log on the harness's state database, as a sync pass would
// write through. Its notify does not reach the daemon's own Log, so a live
// stream finds what it publishes at the stream's next heartbeat.
func watchLog(h *daemontest.Harness) *watch.Log { return watch.NewLog(h.DB, time.Now, nil) }

func watchPublish(t *testing.T, h *daemontest.Harness, events ...watch.Event) {
	t.Helper()
	if err := watchLog(h).Publish(t.Context(), events...); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func watchRef(account, folder string, uid uint32) string {
	return ref.Ref{Account: account, Folder: folder, UIDValidity: 7, UID: uid}.String()
}

func watchNew(account, folder string, uid uint32, subject string) watch.Event {
	return watch.Event{
		At: time.Now(), Kind: watch.KindNew, Account: account, Folder: folder, Ref: watchRef(account, folder, uid),
		Message: &watch.Message{
			Date: time.Now(), From: []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			Subject: subject, Flags: []string{}, Size: 100,
		},
	}
}

func watchFlags(account, folder string, uid uint32, flags ...string) watch.Event {
	return watch.Event{At: time.Now(), Kind: watch.KindFlags, Account: account, Folder: folder, Ref: watchRef(account, folder, uid), Flags: flags}
}

func watchStatus(account, folder, state string) watch.Event {
	return watch.Event{At: time.Now(), Kind: watch.KindStatus, Account: account, Folder: folder, State: state}
}

// watchItems is what a stream sent up to and including a heartbeat.
type watchItems struct {
	events    []*chainmailv1.WatchEvent
	heartbeat *chainmailv1.Heartbeat
}

// subscribe opens a stream that ends with the test, or after 10 seconds.
func subscribe(t *testing.T, h *daemontest.Harness, req *chainmailv1.SubscribeRequest) *connect.ServerStreamForClient[chainmailv1.SubscribeResponse] {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := h.Client.Watch.Subscribe(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// untilHeartbeat receives events up to and including the next heartbeat.
func untilHeartbeat(t *testing.T, stream *connect.ServerStreamForClient[chainmailv1.SubscribeResponse]) watchItems {
	t.Helper()
	var got watchItems
	for stream.Receive() {
		msg := stream.Msg()
		if hb := msg.GetHeartbeat(); hb != nil {
			got.heartbeat = hb
			return got
		}
		got.events = append(got.events, msg.GetEvent())
	}
	t.Fatalf("stream ended before a heartbeat: %v", stream.Err())
	return got
}

// subscribeErr opens a stream, drains it and returns how it ended.
func subscribeErr(t *testing.T, h *daemontest.Harness, req *chainmailv1.SubscribeRequest) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := h.Client.Watch.Subscribe(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
	}
	return stream.Err()
}

func eventIDs(events []*chainmailv1.WatchEvent) []int64 {
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.GetId())
	}
	return ids
}

// TestSubscribeSendsBacklogThenHeartbeat checks that a stream sends what the
// log already holds after its start, then a heartbeat at once, without
// waiting out the 30 second interval, whose position is the last event.
func TestSubscribeSendsBacklogThenHeartbeat(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "one"), watchNew("host", "INBOX", 2, "two"), watchNew("work", "INBOX", 1, "three"))
	got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(0)}))
	if ids := eventIDs(got.events); !slices.Equal(ids, []int64{1, 2, 3}) {
		t.Fatalf("ids = %v, want [1 2 3]", ids)
	}
	if got.heartbeat.GetPosition() != 3 || got.heartbeat.GetAt() == nil {
		t.Errorf("heartbeat = %v, want position 3 with a time", got.heartbeat)
	}
	e := got.events[0]
	wantRef := watchRef("host", "INBOX", 1)
	if e.GetKind() != "new" || e.GetAccount() != "host" || e.GetFolder() != "INBOX" || e.GetRef() != wantRef {
		t.Errorf("event = %v", e)
	}
	m := e.GetMessage()
	if m.GetSubject().GetUntrusted() != "one" || m.GetRef() != wantRef || m.GetAccount() != "host" || m.GetFrom()[0].GetEmail().GetUntrusted() != "ann@example.com" {
		t.Errorf("message = %v", m)
	}
}

func TestSubscribeSinceResumesAfterTheID(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))
	tests := []struct {
		since int64
		want  []int64
	}{
		{since: 0, want: []int64{1, 2, 3}},
		{since: 2, want: []int64{3}},
		{since: 3, want: []int64{}},
	}
	for _, tt := range tests {
		got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(tt.since)}))
		if ids := eventIDs(got.events); !slices.Equal(ids, tt.want) || got.heartbeat.GetPosition() != 3 {
			t.Errorf("since %d: ids %v then position %d, want %v then 3", tt.since, ids, got.heartbeat.GetPosition(), tt.want)
		}
	}
}

// TestSubscribeGroupResumesAfterItsAcknowledgedEvent checks that a new group
// starts at the tail, that a later stream resumes after what the group has
// acknowledged, and that the daemon acknowledges nothing a stream sends:
// the client does, once it has written the events.
func TestSubscribeGroupResumesAfterItsAcknowledgedEvent(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"))
	first := subscribe(t, h, &chainmailv1.SubscribeRequest{Group: "bot"})
	got := untilHeartbeat(t, first)
	if len(got.events) != 0 || got.heartbeat.GetPosition() != 2 {
		t.Fatalf("new group: ids %v then position %d, want none then 2", eventIDs(got.events), got.heartbeat.GetPosition())
	}
	_ = first.Close()
	watchPublish(t, h, watchNew("host", "INBOX", 3, "c"), watchNew("host", "INBOX", 4, "d"))
	if err := watchLog(h).Ack(t.Context(), "bot", 3); err != nil {
		t.Fatal(err)
	}
	got = untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Group: "bot"}))
	if ids := eventIDs(got.events); !slices.Equal(ids, []int64{4}) || got.heartbeat.GetPosition() != 4 {
		t.Errorf("resumed group: ids %v then position %d, want [4] then 4", ids, got.heartbeat.GetPosition())
	}
	groups, err := watchLog(h).Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "bot" || groups[0].AckedID != 3 {
		t.Errorf("groups = %+v, want bot still at 3", groups)
	}
}

// TestSubscribeLiveSendsOnlyNewEvents checks that a stream with neither
// group nor since skips the backlog and sends what arrives later.
func TestSubscribeLiveSendsOnlyNewEvents(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 20*time.Millisecond)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "old"), watchNew("host", "INBOX", 2, "old"))
	stream := subscribe(t, h, &chainmailv1.SubscribeRequest{})
	got := untilHeartbeat(t, stream)
	if len(got.events) != 0 || got.heartbeat.GetPosition() != 2 {
		t.Fatalf("live start: ids %v then position %d, want none then 2", eventIDs(got.events), got.heartbeat.GetPosition())
	}
	watchPublish(t, h, watchNew("host", "INBOX", 3, "new"))
	var events []*chainmailv1.WatchEvent
	for len(events) == 0 {
		events = append(events, untilHeartbeat(t, stream).events...)
	}
	if ids := eventIDs(events); !slices.Equal(ids, []int64{3}) {
		t.Errorf("live ids = %v, want [3]", ids)
	}
}

// TestSubscribeFilters checks the filter in the daemon, and that the
// heartbeat position moves past filtered events too. A status event always
// passes in (contract C4), since it describes a folder, not a message.
func TestSubscribeFilters(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h,
		watchNew("host", "INBOX", 1, "one"),               // 1
		watchNew("work", "INBOX", 1, "two"),               // 2
		watchNew("host", "Sent", 1, "three"),              // 3
		watchFlags("host", "INBOX", 1, `\Seen`),           // 4
		watchStatus("host", "INBOX", watch.StateWatching), // 5
	)
	tests := []struct {
		name   string
		filter *chainmailv1.WatchFilter
		want   []int64
	}{
		{name: "kind new passes status", filter: &chainmailv1.WatchFilter{Kinds: []string{"new"}}, want: []int64{1, 2, 3, 5}},
		{name: "account", filter: &chainmailv1.WatchFilter{Accounts: []string{"work"}}, want: []int64{2}},
		{name: "in folder, any case, status passes", filter: &chainmailv1.WatchFilter{In: "sent"}, want: []int64{3, 5}},
		{
			name:   "combined, status not named",
			filter: &chainmailv1.WatchFilter{Kinds: []string{"new", "flags"}, Accounts: []string{"host"}, In: "inbox"},
			want:   []int64{1, 4, 5},
		},
		{name: "status named", filter: &chainmailv1.WatchFilter{Kinds: []string{"status"}}, want: []int64{5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(0), Filter: tt.filter}))
			if ids := eventIDs(got.events); !slices.Equal(ids, tt.want) {
				t.Errorf("ids = %v, want %v", ids, tt.want)
			}
			if got.heartbeat.GetPosition() != 5 {
				t.Errorf("position = %d, want 5", got.heartbeat.GetPosition())
			}
		})
	}
}

// TestSubscribeTrimmedGapComesFirst checks that a start below the oldest
// retained event gets a trimmed gap first, then the oldest event on.
func TestSubscribeTrimmedGapComesFirst(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))
	if _, err := h.DB.ExecContext(t.Context(), "DELETE FROM events WHERE id <= 2"); err != nil {
		t.Fatal(err)
	}
	got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(0)}))
	if len(got.events) != 2 {
		t.Fatalf("events = %v, want a gap then event 3", got.events)
	}
	if g := got.events[0]; g.GetKind() != "gap" || g.GetId() != 0 || g.GetReason() != "trimmed" || g.GetNextId() != 3 {
		t.Errorf("first = %v, want a trimmed gap with next_id 3", g)
	}
	if got.events[1].GetId() != 3 || got.heartbeat.GetPosition() != 3 {
		t.Errorf("then %v and position %d, want event 3 and 3", got.events[1], got.heartbeat.GetPosition())
	}
}

// TestSubscribeHeartbeatCadence checks that an idle stream sends a
// heartbeat every interval, and no sooner: timers never fire early, so
// three heartbeats take at least two intervals.
func TestSubscribeHeartbeatCadence(t *testing.T) {
	t.Parallel()
	const beat = 20 * time.Millisecond
	h := startWatch(t, beat)
	start := time.Now()
	stream := subscribe(t, h, &chainmailv1.SubscribeRequest{})
	for i := range 3 {
		got := untilHeartbeat(t, stream)
		if len(got.events) != 0 || got.heartbeat.GetPosition() != 0 {
			t.Fatalf("heartbeat %d: ids %v, position %d; want none and 0", i, eventIDs(got.events), got.heartbeat.GetPosition())
		}
	}
	if elapsed := time.Since(start); elapsed < 2*beat {
		t.Errorf("3 heartbeats in %v, want at least %v", elapsed, 2*beat)
	}
}

// TestShutdownEndsSubscribeStreams checks that a shutdown ends an open
// stream at once, rather than waiting out the 10 second shutdown timeout.
func TestShutdownEndsSubscribeStreams(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	stream := subscribe(t, h, &chainmailv1.SubscribeRequest{})
	untilHeartbeat(t, stream)
	start := time.Now()
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("shutdown took %v with a stream open", elapsed)
	}
	if stream.Receive() {
		t.Errorf("stream sent %v after shutdown", stream.Msg())
	}
}

// TestSubscribeRefusesBadArguments checks each refusal, and that every one
// is audited as an error without the client's bytes.
func TestSubscribeRefusesBadArguments(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	tests := []struct {
		name    string
		req     *chainmailv1.SubscribeRequest
		reason  string
		targets string
	}{
		{
			name: "bad group", req: &chainmailv1.SubscribeRequest{Group: "Bad Group"},
			reason: api.ReasonInvalidArgument, targets: `["group=(invalid)","accounts=0","kinds=","in=unset"]`,
		},
		{
			name: "group and since", req: &chainmailv1.SubscribeRequest{Group: "bot", Since: proto.Int64(1)},
			reason: api.ReasonInvalidArgument, targets: `["group=bot","accounts=0","kinds=","in=unset"]`,
		},
		{
			name: "negative since", req: &chainmailv1.SubscribeRequest{Since: proto.Int64(-1)},
			reason: api.ReasonInvalidArgument, targets: `["since=-1","accounts=0","kinds=","in=unset"]`,
		},
		{
			name: "unknown kind", req: &chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Kinds: []string{"new", "bogus"}}},
			reason: api.ReasonInvalidArgument, targets: `["live","accounts=0","kinds=(invalid)","in=unset"]`,
		},
		{
			name: "unknown account", req: &chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Accounts: []string{"nope"}}},
			reason: api.ReasonNotFound, targets: `["live","accounts=1","kinds=","in=unset"]`,
		},
		{
			name: "invalid account name", req: &chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Accounts: []string{"Not Valid"}}},
			reason: api.ReasonInvalidArgument, targets: `["live","accounts=1","kinds=","in=unset"]`,
		},
	}
	for _, tt := range tests {
		err := subscribeErr(t, h, tt.req)
		if got := reasonOf(err); got != tt.reason {
			t.Errorf("%s: reason = %s (%v), want %s", tt.name, got, err, tt.reason)
		}
		if tt.name == "bad group" {
			if hint := api.Detail(err).GetHint(); hint != groupHint {
				t.Errorf("bad group: hint = %q, want %q", hint, groupHint)
			}
		}
	}
	rows := h.AuditRows(t)
	if len(rows) != len(tests) {
		t.Fatalf("audit rows = %+v, want %d", rows, len(tests))
	}
	for i, tt := range tests {
		want := daemontest.AuditRow{Client: "test", Action: "watch.subscribe", Targets: tt.targets, Result: "error", Reason: tt.reason}
		if rows[i] != want {
			t.Errorf("%s: audit = %+v, want %+v", tt.name, rows[i], want)
		}
	}
}

// TestSubscribeAudit checks the watch.subscribe row: the start, the number
// of accounts, the kinds sorted without repeats, and whether in is set,
// never its value, which can be a label name.
func TestSubscribeAudit(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	tests := []struct {
		name string
		req  *chainmailv1.SubscribeRequest
		want daemontest.AuditRow
	}{
		{
			name: "group",
			req: &chainmailv1.SubscribeRequest{Group: "bot", Filter: &chainmailv1.WatchFilter{
				Accounts: []string{"host"}, Kinds: []string{"new", "flags", "new"}, In: "Secret Label",
			}},
			want: daemontest.AuditRow{Client: "test", Account: "host", Action: "watch.subscribe", Targets: `["group=bot","accounts=1","kinds=flags,new","in=set"]`, Result: "ok"},
		},
		{
			name: "live",
			req:  &chainmailv1.SubscribeRequest{},
			want: daemontest.AuditRow{Client: "test", Action: "watch.subscribe", Targets: `["live","accounts=0","kinds=","in=unset"]`, Result: "ok"},
		},
		{
			name: "since",
			req:  &chainmailv1.SubscribeRequest{Since: proto.Int64(5), Filter: &chainmailv1.WatchFilter{Accounts: []string{"work", "host"}, Kinds: []string{"status"}}},
			want: daemontest.AuditRow{Client: "test", Action: "watch.subscribe", Targets: `["since=5","accounts=2","kinds=status","in=unset"]`, Result: "ok"},
		},
	}
	for i, tt := range tests {
		// The row is written before the first item, so it is there by the heartbeat.
		untilHeartbeat(t, subscribe(t, h, tt.req))
		rows := h.AuditRows(t)
		if len(rows) != i+1 || rows[i] != tt.want {
			t.Errorf("%s: audit = %+v, want last %+v", tt.name, rows, tt.want)
			continue
		}
		if strings.Contains(rows[i].Targets, "Secret") {
			t.Errorf("%s: audit records the in value: %s", tt.name, rows[i].Targets)
		}
	}
}

func readEvents(t *testing.T, h *daemontest.Harness, req *chainmailv1.ReadEventsRequest) *chainmailv1.ReadEventsResponse {
	t.Helper()
	resp, err := h.Client.Watch.ReadEvents(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	return resp.Msg
}

// TestReadEventsPages checks the limit, its default of 50, the cursor, and
// that a read with neither cursor nor group and no wait returns nothing,
// with the tail as its cursor.
func TestReadEventsPages(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	events := make([]watch.Event, 60)
	for i := range events {
		events[i] = watchNew("host", "INBOX", uint32(i+1), "m")
	}
	watchPublish(t, h, events...)
	tests := []struct {
		name              string
		req               *chainmailv1.ReadEventsRequest
		count             int
		first, last, next int64
	}{
		{name: "limit", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Limit: 2}, count: 2, first: 1, last: 2, next: 2},
		{name: "default limit", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0)}, count: 50, first: 1, last: 50, next: 50},
		{name: "cursor", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(58)}, count: 2, first: 59, last: 60, next: 60},
		{name: "live", req: &chainmailv1.ReadEventsRequest{}, count: 0, next: 60},
	}
	for _, tt := range tests {
		resp := readEvents(t, h, tt.req)
		ids := eventIDs(resp.GetEvents())
		if len(ids) != tt.count || resp.GetNextCursor() != tt.next {
			t.Errorf("%s: %d events, next %d; want %d, next %d", tt.name, len(ids), resp.GetNextCursor(), tt.count, tt.next)
			continue
		}
		if tt.count > 0 && (ids[0] != tt.first || ids[len(ids)-1] != tt.last) {
			t.Errorf("%s: ids %d to %d, want %d to %d", tt.name, ids[0], ids[len(ids)-1], tt.first, tt.last)
		}
	}
}

// TestReadEventsGroupAcknowledges checks that a group read starts after the
// group's acknowledged event, a new group at the tail, and acknowledges the
// page it returns.
func TestReadEventsGroupAcknowledges(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))
	if resp := readEvents(t, h, &chainmailv1.ReadEventsRequest{Group: "mcp"}); len(resp.GetEvents()) != 0 || resp.GetNextCursor() != 3 {
		t.Fatalf("new group: %v, want nothing at 3", resp)
	}
	watchPublish(t, h, watchNew("host", "INBOX", 4, "d"), watchNew("host", "INBOX", 5, "e"))
	resp := readEvents(t, h, &chainmailv1.ReadEventsRequest{Group: "mcp", Limit: 1})
	if ids := eventIDs(resp.GetEvents()); !slices.Equal(ids, []int64{4}) || resp.GetNextCursor() != 4 {
		t.Fatalf("group read: ids %v next %d, want [4] next 4", ids, resp.GetNextCursor())
	}
	groups, err := watchLog(h).Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "mcp" || groups[0].AckedID != 4 || groups[0].Lag != 1 {
		t.Errorf("groups = %+v, want mcp acked at 4 with lag 1", groups)
	}
	if ids := eventIDs(readEvents(t, h, &chainmailv1.ReadEventsRequest{Group: "mcp"}).GetEvents()); !slices.Equal(ids, []int64{5}) {
		t.Errorf("next group read: ids %v, want [5]", ids)
	}
}

func TestReadEventsRefusesBadArguments(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	tests := []struct {
		name   string
		req    *chainmailv1.ReadEventsRequest
		reason string
	}{
		{name: "wait too long", req: &chainmailv1.ReadEventsRequest{WaitSeconds: 61}, reason: api.ReasonInvalidArgument},
		{name: "negative wait", req: &chainmailv1.ReadEventsRequest{WaitSeconds: -1}, reason: api.ReasonInvalidArgument},
		{name: "limit too big", req: &chainmailv1.ReadEventsRequest{Limit: 201}, reason: api.ReasonInvalidArgument},
		{name: "negative limit", req: &chainmailv1.ReadEventsRequest{Limit: -1}, reason: api.ReasonInvalidArgument},
		{name: "group and cursor", req: &chainmailv1.ReadEventsRequest{Group: "mcp", Cursor: proto.Int64(1)}, reason: api.ReasonInvalidArgument},
		{name: "negative cursor", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(-1)}, reason: api.ReasonInvalidArgument},
		{name: "bad group", req: &chainmailv1.ReadEventsRequest{Group: "-starts-with-dash"}, reason: api.ReasonInvalidArgument},
		{name: "unknown kind", req: &chainmailv1.ReadEventsRequest{Filter: &chainmailv1.WatchFilter{Kinds: []string{"NEW"}}}, reason: api.ReasonInvalidArgument},
		{name: "unknown account", req: &chainmailv1.ReadEventsRequest{Filter: &chainmailv1.WatchFilter{Accounts: []string{"nope"}}}, reason: api.ReasonNotFound},
	}
	for _, tt := range tests {
		_, err := h.Client.Watch.ReadEvents(t.Context(), connect.NewRequest(tt.req))
		if got := reasonOf(err); got != tt.reason {
			t.Errorf("%s: reason = %s (%v), want %s", tt.name, got, err, tt.reason)
		}
	}
	rows := h.AuditRows(t)
	if len(rows) != len(tests) {
		t.Fatalf("audit rows = %+v, want %d", rows, len(tests))
	}
	for i, tt := range tests {
		r := rows[i]
		if r.Action != "watch.read" || r.Result != "error" || r.Reason != tt.reason || !strings.HasSuffix(r.Targets, `"events=0"]`) {
			t.Errorf("%s: audit = %+v", tt.name, r)
		}
	}
}

// TestReadEventsAudit checks the watch.read row: the start and filter as
// watch.subscribe records them, plus the number of events returned.
func TestReadEventsAudit(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("work", "INBOX", 1, "b"), watchNew("host", "INBOX", 2, "c"))
	readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Filter: &chainmailv1.WatchFilter{Accounts: []string{"host"}}})
	readEvents(t, h, &chainmailv1.ReadEventsRequest{Group: "mcp", Filter: &chainmailv1.WatchFilter{In: "Private"}})
	want := []daemontest.AuditRow{
		{Client: "test", Account: "host", Action: "watch.read", Targets: `["since=0","accounts=1","kinds=","in=unset","events=2"]`, Result: "ok"},
		{Client: "test", Action: "watch.read", Targets: `["group=mcp","accounts=0","kinds=","in=set","events=0"]`, Result: "ok"},
	}
	if rows := h.AuditRows(t); !slices.Equal(rows, want) {
		t.Errorf("audit = %+v, want %+v", rows, want)
	}
}

func seedFolders(t *testing.T, h *daemontest.Harness, rows ...watch.FolderRow) {
	t.Helper()
	tx, err := h.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := watch.PutFolder(t.Context(), tx, r); err != nil {
			_ = tx.Rollback() // the put error is the one to report
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAckRaisesAGroupAndIsNotAudited checks that Ack creates an unknown
// group, raises it, never lowers it, refuses bad arguments, and, like
// GetWatchStatus, writes no audit row.
func TestAckRaisesAGroupAndIsNotAudited(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))
	ack := func(group string, id int64) error {
		_, err := h.Client.Watch.Ack(t.Context(), connect.NewRequest(&chainmailv1.AckRequest{Group: group, Id: id}))
		return err
	}
	if err := ack("bot", 2); err != nil {
		t.Fatal(err)
	}
	if err := ack("bot", 1); err != nil {
		t.Fatal(err)
	}
	groups, err := watchLog(h).Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "bot" || groups[0].AckedID != 2 || groups[0].Lag != 1 {
		t.Errorf("groups = %+v, want bot at 2 with lag 1", groups)
	}
	for _, tt := range []struct {
		group string
		id    int64
	}{{"Bad Name", 1}, {"", 1}, {"bot", -1}} {
		if got := reasonOf(ack(tt.group, tt.id)); got != api.ReasonInvalidArgument {
			t.Errorf("Ack(%q, %d): reason = %s, want INVALID_ARGUMENT", tt.group, tt.id, got)
		}
	}
	if _, err := h.Client.Watch.GetWatchStatus(t.Context(), connect.NewRequest(&chainmailv1.GetWatchStatusRequest{})); err != nil {
		t.Fatal(err)
	}
	if rows := h.AuditRows(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for Ack and GetWatchStatus", rows)
	}
}

// TestGetWatchStatus checks that each sync_folders row is reported, ordered
// by account then folder, with its status and no checkpoint.
func TestGetWatchStatus(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	synced := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	seedFolders(t, h,
		watch.FolderRow{Account: "work", Folder: "INBOX", Mode: "poll", State: watch.StateError, LastError: "FOLDER_NOT_FOUND"},
		watch.FolderRow{
			Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 42, HighestModSeq: 900, CondStore: true,
			Mode: "idle", State: watch.StateWatching, LastSyncAt: synced, LastSweepAt: synced.Add(-time.Hour),
		},
	)
	resp, err := h.Client.Watch.GetWatchStatus(t.Context(), connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []*chainmailv1.FolderSyncStatus{
		{
			Account: "host", Folder: "INBOX", Mode: "idle", State: "watching", Condstore: true,
			LastSyncAt: timestamppb.New(synced), LastSweepAt: timestamppb.New(synced.Add(-time.Hour)),
		},
		{Account: "work", Folder: "INBOX", Mode: "poll", State: "error", LastError: "FOLDER_NOT_FOUND"},
	}
	got := resp.Msg.GetFolders()
	if len(got) != len(want) {
		t.Fatalf("folders = %v, want %d", got, len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("folder %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestListAndDeleteGroups checks both calls and their audit rows.
func TestListAndDeleteGroups(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	for _, g := range []string{"alpha", "beta"} {
		if _, _, err := watchLog(h).Group(t.Context(), g); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := h.Client.Watch.ListGroups(t.Context(), connect.NewRequest(&chainmailv1.ListGroupsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range resp.Msg.GetGroups() {
		names = append(names, g.GetName())
		if g.GetAckedId() != 0 || g.GetCreatedAt() == nil || g.GetLastSeenAt() == nil {
			t.Errorf("group = %v", g)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Errorf("names = %v", names)
	}
	del := func(name string) (bool, error) {
		resp, err := h.Client.Watch.DeleteGroup(t.Context(), connect.NewRequest(&chainmailv1.DeleteGroupRequest{Name: name}))
		if err != nil {
			return false, err
		}
		return resp.Msg.GetDeleted(), nil
	}
	if deleted, err := del("alpha"); err != nil || !deleted {
		t.Errorf("delete alpha = %v, %v; want true", deleted, err)
	}
	if deleted, err := del("alpha"); err != nil || deleted {
		t.Errorf("delete alpha again = %v, %v; want false", deleted, err)
	}
	if _, err := del("No Such"); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("delete bad name: %v, want INVALID_ARGUMENT", err)
	}
	want := []daemontest.AuditRow{
		{Client: "test", Action: "watch.groups.list", Targets: `["groups=2"]`, Result: "ok"},
		{Client: "test", Action: "watch.groups.delete", Targets: `["group=alpha"]`, Result: "ok"},
		{Client: "test", Action: "watch.groups.delete", Targets: `["group=alpha"]`, Result: "ok"},
		{Client: "test", Action: "watch.groups.delete", Targets: `["group=(invalid)"]`, Result: "error", Reason: api.ReasonInvalidArgument},
	}
	if rows := h.AuditRows(t); !slices.Equal(rows, want) {
		t.Errorf("audit = %+v, want %+v", rows, want)
	}
}

// TestWatchForbiddenPeer checks that the watch routes sit behind the peer
// check like every other: a peer the Authorizer refuses gets FORBIDDEN and
// leaves no audit row. It passes as soon as the routes are mounted.
func TestWatchForbiddenPeer(t *testing.T) {
	t.Parallel()
	other := daemon.NewAuthorizer(uint32(os.Getuid())+1, -1)
	h := daemontest.Start(t, watchConfig, daemontest.Options{Authorizer: other})
	_, err := h.Client.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != api.ReasonForbidden {
		t.Errorf("ReadEvents: %v, want PermissionDenied FORBIDDEN", err)
	}
	if err := subscribeErr(t, h, &chainmailv1.SubscribeRequest{}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("Subscribe: %v, want PermissionDenied", err)
	}
	if rows := h.AuditRows(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}

// wantPastTailGap checks that e is the trimmed gap a start past the tail
// gets: ID 0, reason trimmed, next_id one past the tail.
func wantPastTailGap(t *testing.T, e *chainmailv1.WatchEvent, tail int64) {
	t.Helper()
	if e.GetKind() != "gap" || e.GetId() != 0 || e.GetReason() != "trimmed" || e.GetNextId() != tail+1 || e.GetAt() == nil {
		t.Errorf("event = %v, want a trimmed gap with next_id %d", e, tail+1)
	}
}

// TestSubscribeSinceAboveTailReportsAGap checks that a since past the tail,
// which only a reset of the daemon's state can produce, gets a trimmed gap
// first and then the events that arrive after the tail, rather than
// silently skipping every ID up to since.
func TestSubscribeSinceAboveTailReportsAGap(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 20*time.Millisecond)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))
	stream := subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(50)})
	got := untilHeartbeat(t, stream)
	if len(got.events) != 1 || got.heartbeat.GetPosition() != 3 {
		t.Fatalf("since past tail: events %v then position %d, want one gap then 3", got.events, got.heartbeat.GetPosition())
	}
	wantPastTailGap(t, got.events[0], 3)
	watchPublish(t, h, watchNew("host", "INBOX", 4, "d"))
	var events []*chainmailv1.WatchEvent
	for len(events) == 0 {
		events = append(events, untilHeartbeat(t, stream).events...)
	}
	if ids := eventIDs(events); !slices.Equal(ids, []int64{4}) {
		t.Errorf("later ids = %v, want [4]", ids)
	}
}

// TestReadEventsCursorAboveTailReportsAGap checks that a cursor past the
// tail returns a trimmed gap with the tail as the next cursor, and that
// reading on from there returns the later events. The gap counts toward
// the limit.
func TestReadEventsCursorAboveTailReportsAGap(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))
	for _, limit := range []int32{0, 1} {
		resp := readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(50), Limit: limit})
		if len(resp.GetEvents()) != 1 || resp.GetNextCursor() != 3 {
			t.Fatalf("limit %d: %v, want one gap and cursor 3", limit, resp)
		}
		wantPastTailGap(t, resp.GetEvents()[0], 3)
	}
	watchPublish(t, h, watchNew("host", "INBOX", 4, "d"), watchNew("host", "INBOX", 5, "e"))
	resp := readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(3)})
	if ids := eventIDs(resp.GetEvents()); !slices.Equal(ids, []int64{4, 5}) || resp.GetNextCursor() != 5 {
		t.Errorf("read on: ids %v next %d, want [4 5] next 5", ids, resp.GetNextCursor())
	}
}

// seedGroupPastTail creates group with its acknowledged ID at acked, past
// the tail, as a reset of the daemon's state leaves it.
func seedGroupPastTail(t *testing.T, h *daemontest.Harness, group string, acked int64) {
	t.Helper()
	if _, _, err := watchLog(h).Group(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.ExecContext(t.Context(), "UPDATE event_groups SET acked_id = ? WHERE name = ?", acked, group); err != nil {
		t.Fatal(err)
	}
}

func groupAcked(t *testing.T, h *daemontest.Harness, group string) int64 {
	t.Helper()
	groups, err := watchLog(h).Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == group {
			return g.AckedID
		}
	}
	t.Fatalf("no group %s in %+v", group, groups)
	return 0
}

// TestGroupAckedAboveTailIsReset checks that a group whose acknowledged ID
// is past the tail, so belongs to a log that no longer exists, is reset to
// the tail on its first read, which reports one trimmed gap. Later reads
// and reconnects resume normally, with no repeated gap and nothing skipped.
func TestGroupAckedAboveTailIsReset(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"), watchNew("host", "INBOX", 2, "b"), watchNew("host", "INBOX", 3, "c"))

	seedGroupPastTail(t, h, "mcp", 50)
	resp := readEvents(t, h, &chainmailv1.ReadEventsRequest{Group: "mcp"})
	if len(resp.GetEvents()) != 1 || resp.GetNextCursor() != 3 {
		t.Fatalf("first read: %v, want one gap and cursor 3", resp)
	}
	wantPastTailGap(t, resp.GetEvents()[0], 3)
	if acked := groupAcked(t, h, "mcp"); acked != 3 {
		t.Errorf("after the first read mcp is at %d, want reset to 3", acked)
	}

	seedGroupPastTail(t, h, "bot", 50)
	got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Group: "bot"}))
	if len(got.events) != 1 || got.heartbeat.GetPosition() != 3 {
		t.Fatalf("first stream: events %v then position %d, want one gap then 3", got.events, got.heartbeat.GetPosition())
	}
	wantPastTailGap(t, got.events[0], 3)

	watchPublish(t, h, watchNew("host", "INBOX", 4, "d"), watchNew("host", "INBOX", 5, "e"))
	resp = readEvents(t, h, &chainmailv1.ReadEventsRequest{Group: "mcp"})
	if ids := eventIDs(resp.GetEvents()); !slices.Equal(ids, []int64{4, 5}) || resp.GetNextCursor() != 5 {
		t.Errorf("second read: ids %v next %d, want [4 5] next 5 with no gap", ids, resp.GetNextCursor())
	}
	if acked := groupAcked(t, h, "mcp"); acked != 5 {
		t.Errorf("after the second read mcp is at %d, want 5", acked)
	}
	got = untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Group: "bot"}))
	if ids := eventIDs(got.events); !slices.Equal(ids, []int64{4, 5}) || got.heartbeat.GetPosition() != 5 {
		t.Errorf("reconnect: ids %v then position %d, want [4 5] then 5 with no gap", ids, got.heartbeat.GetPosition())
	}
}

// idRange is from to to inclusive.
func idRange(from, to int64) []int64 {
	ids := make([]int64, 0, to-from+1)
	for id := from; id <= to; id++ {
		ids = append(ids, id)
	}
	return ids
}

// TestSubscribeBacklogCrossesPages checks a backlog longer than one
// 100-event read: every event arrives once, in order, before the single
// caught-up heartbeat.
func TestSubscribeBacklogCrossesPages(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	events := make([]watch.Event, 250)
	for i := range events {
		events[i] = watchNew("host", "INBOX", uint32(i+1), "m")
	}
	watchPublish(t, h, events...)
	got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(0)}))
	if ids := eventIDs(got.events); !slices.Equal(ids, idRange(1, 250)) {
		t.Errorf("ids = %d events from %v, want 1 to 250", len(ids), ids[:min(len(ids), 3)])
	}
	if got.heartbeat.GetPosition() != 250 {
		t.Errorf("position = %d, want 250", got.heartbeat.GetPosition())
	}
}

// TestFilteredReadsCrossTheScanCap checks a filter that matches events
// either side of the 5000 rows one log read examines. Subscribe reads on
// past the cap before its heartbeat. A ReadEvents page with no wait stops
// at the cap, possibly empty, with the cursor where it stopped, and a
// waiting read reads on past it.
func TestFilteredReadsCrossTheScanCap(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	events := make([]watch.Event, 5004)
	for i := range events {
		events[i] = watchNew("host", "INBOX", uint32(i+1), "m")
	}
	events[1] = watchNew("work", "INBOX", 2, "early")      // ID 2
	events[5002] = watchNew("work", "INBOX", 5003, "late") // ID 5003
	watchPublish(t, h, events...)
	work := &chainmailv1.WatchFilter{Accounts: []string{"work"}}

	got := untilHeartbeat(t, subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(0), Filter: work}))
	if ids := eventIDs(got.events); !slices.Equal(ids, []int64{2, 5003}) || got.heartbeat.GetPosition() != 5004 {
		t.Errorf("Subscribe: ids %v then position %d, want [2 5003] then 5004", ids, got.heartbeat.GetPosition())
	}

	tests := []struct {
		name   string
		req    *chainmailv1.ReadEventsRequest
		want   []int64
		cursor int64
	}{
		{name: "first page stops at the cap", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Filter: work}, want: []int64{2}, cursor: 5000},
		{name: "empty page past the cap", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(2), Filter: work}, want: []int64{}, cursor: 5002},
		{name: "from the cap", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(5000), Filter: work}, want: []int64{5003}, cursor: 5004},
		{name: "waiting reads on", req: &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(2), Filter: work, WaitSeconds: 5}, want: []int64{5003}, cursor: 5004},
	}
	for _, tt := range tests {
		resp := readEvents(t, h, tt.req)
		if ids := eventIDs(resp.GetEvents()); !slices.Equal(ids, tt.want) || resp.GetNextCursor() != tt.cursor {
			t.Errorf("%s: ids %v cursor %d, want %v cursor %d", tt.name, ids, resp.GetNextCursor(), tt.want, tt.cursor)
		}
	}
}
