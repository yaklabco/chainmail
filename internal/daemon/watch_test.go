package daemon_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
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
func watchLog(h *daemontest.Harness) *watch.Log { return watch.NewLog(h.DB, time.Now) }

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
