package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// TestWatchServerDefaults checks the watch fields New fills in: the 30
// second heartbeat unless Options sets one, the event log and folder state,
// and a ReadEvents wait unit of one second.
func TestWatchServerDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		set      time.Duration
		wantBeat time.Duration
	}{
		{name: "zero means 30s", set: 0, wantBeat: 30 * time.Second},
		{name: "set", set: 20 * time.Millisecond, wantBeat: 20 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: tt.set})
			if s.opts.WatchHeartbeat != tt.wantBeat {
				t.Errorf("heartbeat = %v, want %v", s.opts.WatchHeartbeat, tt.wantBeat)
			}
			if s.watchLog == nil || s.watchState == nil {
				t.Error("watch log or state not built")
			}
			if s.watchWaitUnit != time.Second {
				t.Errorf("wait unit = %v, want 1s", s.watchWaitUnit)
			}
		})
	}
}

// TestEventToProto checks each kind's shape. A new event's message is a
// search row: every message-derived string is wrapped and cleaned as
// rowFromSummary does, and the row carries the event's ref, account and
// folder. A trimmed gap keeps ID 0.
func TestEventToProto(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	snippet := "see you at noon"
	const r = "cm:host:INBOX:7:42"
	tests := []struct {
		name string
		in   watch.Event
		want *chainmailv1.WatchEvent
	}{
		{
			name: "new",
			in: watch.Event{
				ID: 7, At: at, Kind: watch.KindNew, Account: "host", Folder: "INBOX", Ref: r,
				Message: &watch.Message{
					Date: at, InternalDate: at.Add(time.Minute),
					From:    []mailstore.Address{{Name: "Ann\tLee", Email: "ann@example.com"}},
					To:      []mailstore.Address{{Email: "me@example.com"}},
					Subject: "Lunch\r\nBcc: x", Flags: []string{`\Seen`}, Labels: []string{`\Inbox`},
					Size: 1200, HasAttachment: true, ThreadID: "t1", Snippet: &snippet,
				},
			},
			want: &chainmailv1.WatchEvent{
				Id: 7, At: timestamppb.New(at), Kind: "new", Account: "host", Folder: "INBOX", Ref: r,
				Message: &chainmailv1.MessageRow{
					Ref: r, Account: "host", Folder: "INBOX",
					Date: timestamppb.New(at), InternalDate: timestamppb.New(at.Add(time.Minute)),
					From: []*chainmailv1.Address{{
						Name:  &chainmailv1.Untrusted{Untrusted: "Ann Lee"},
						Email: &chainmailv1.Untrusted{Untrusted: "ann@example.com"},
					}},
					To: []*chainmailv1.Address{{
						Name:  &chainmailv1.Untrusted{},
						Email: &chainmailv1.Untrusted{Untrusted: "me@example.com"},
					}},
					Subject: &chainmailv1.Untrusted{Untrusted: "Lunch Bcc: x"},
					Flags:   []string{`\Seen`}, Labels: []string{`\Inbox`}, Size: 1200, HasAttachment: true, ThreadId: "t1",
					Snippet: &chainmailv1.Untrusted{Untrusted: snippet},
				},
			},
		},
		{
			name: "new without snippet",
			in:   watch.Event{ID: 8, At: at, Kind: watch.KindNew, Account: "host", Folder: "INBOX", Ref: r, Message: &watch.Message{Subject: "hi"}},
			want: &chainmailv1.WatchEvent{
				Id: 8, At: timestamppb.New(at), Kind: "new", Account: "host", Folder: "INBOX", Ref: r,
				Message: &chainmailv1.MessageRow{Ref: r, Account: "host", Folder: "INBOX", Subject: &chainmailv1.Untrusted{Untrusted: "hi"}},
			},
		},
		{
			name: "flags",
			in:   watch.Event{ID: 9, At: at, Kind: watch.KindFlags, Account: "gm", Folder: "[Gmail]/All Mail", Ref: r, Flags: []string{`\Seen`}, Labels: []string{`\Inbox`}},
			want: &chainmailv1.WatchEvent{Id: 9, At: timestamppb.New(at), Kind: "flags", Account: "gm", Folder: "[Gmail]/All Mail", Ref: r, Flags: []string{`\Seen`}, Labels: []string{`\Inbox`}},
		},
		{
			name: "expunged",
			in:   watch.Event{ID: 10, At: at, Kind: watch.KindExpunged, Account: "host", Folder: "INBOX", Ref: r},
			want: &chainmailv1.WatchEvent{Id: 10, At: timestamppb.New(at), Kind: "expunged", Account: "host", Folder: "INBOX", Ref: r},
		},
		{
			name: "status",
			in:   watch.Event{ID: 11, At: at, Kind: watch.KindStatus, Account: "host", State: watch.StateAuthRequired, Detail: "run: chainmail secret set accounts/host"},
			want: &chainmailv1.WatchEvent{Id: 11, At: timestamppb.New(at), Kind: "status", Account: "host", State: "auth_required", Detail: "run: chainmail secret set accounts/host"},
		},
		{
			name: "trimmed gap",
			in:   watch.Event{At: at, Kind: watch.KindGap, Reason: watch.GapTrimmed, NextID: 12},
			want: &chainmailv1.WatchEvent{At: timestamppb.New(at), Kind: "gap", Reason: "trimmed", NextId: 12},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := eventToProto(tt.in); !proto.Equal(got, tt.want) {
				t.Errorf("eventToProto =\n%v\nwant\n%v", got, tt.want)
			}
		})
	}
}

// TestFolderStatusProto checks that a folder row keeps its status fields
// and drops its checkpoint, and that a time never set stays unset.
func TestFolderStatusProto(t *testing.T) {
	t.Parallel()
	synced := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   watch.FolderRow
		want *chainmailv1.FolderSyncStatus
	}{
		{
			name: "watching",
			in: watch.FolderRow{
				Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 42, HighestModSeq: 900, CondStore: true,
				Mode: "idle", State: watch.StateWatching, LastSyncAt: synced, LastSweepAt: synced.Add(-time.Hour),
			},
			want: &chainmailv1.FolderSyncStatus{
				Account: "host", Folder: "INBOX", Mode: "idle", State: "watching", Condstore: true,
				LastSyncAt: timestamppb.New(synced), LastSweepAt: timestamppb.New(synced.Add(-time.Hour)),
			},
		},
		{
			name: "never synced",
			in:   watch.FolderRow{Account: "host", Folder: "Sent", Mode: "poll", State: watch.StateError, LastError: "FOLDER_NOT_FOUND"},
			want: &chainmailv1.FolderSyncStatus{Account: "host", Folder: "Sent", Mode: "poll", State: "error", LastError: "FOLDER_NOT_FOUND"},
		},
	}
	for _, tt := range tests {
		if got := folderStatusProto(tt.in); !proto.Equal(got, tt.want) {
			t.Errorf("%s: folderStatusProto =\n%v\nwant\n%v", tt.name, got, tt.want)
		}
	}
}

func TestGroupProto(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	got := groupProto(watch.GroupInfo{Name: "bot", AckedID: 4, Lag: 2, CreatedAt: created, LastSeenAt: created.Add(time.Hour)})
	want := &chainmailv1.GroupStatus{Name: "bot", AckedId: 4, Lag: 2, CreatedAt: timestamppb.New(created), LastSeenAt: timestamppb.New(created.Add(time.Hour))}
	if !proto.Equal(got, want) {
		t.Errorf("groupProto = %v, want %v", got, want)
	}
}

// serveWatchTest serves s until the test ends and returns a client of it.
// Build s with DisableWatch when the test reads the event log, or the
// supervisors' status events land in it too.
func serveWatchTest(t *testing.T, s *Server) *client.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	c := client.New(s.opts.Listener.Addr().String(), "test")
	t.Cleanup(func() {
		c.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return c
}

func testWatchEvent(subject string) watch.Event {
	return watch.Event{At: time.Now(), Kind: watch.KindNew, Account: "host", Folder: "INBOX", Ref: "ref-" + subject, Message: &watch.Message{Subject: subject}}
}

// TestSubscribeWakesOnNotify checks that a caught-up stream learns of an
// event from the log's notify. The heartbeat is an hour, so nothing else
// could wake it before the test's deadline.
func TestSubscribeWakesOnNotify(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: time.Hour, DisableWatch: true})
	c := serveWatchTest(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := c.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() || stream.Msg().GetHeartbeat() == nil {
		t.Fatalf("first item = %v (%v), want the caught-up heartbeat", stream.Msg(), stream.Err())
	}
	if err := s.watchLog.Publish(ctx, testWatchEvent("woken")); err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("stream ended: %v", stream.Err())
	}
	if e := stream.Msg().GetEvent(); e.GetId() != 1 || e.GetMessage().GetSubject().GetUntrusted() != "woken" {
		t.Errorf("item = %v, want event 1", stream.Msg())
	}
}

// TestSubscribeFailsWhenTheAuditWriteFails checks that a stream that could
// not be audited never starts. The failure comes before the first send, so
// the stream itself is never touched.
func TestSubscribeFailsWhenTheAuditWriteFails(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	if err := stateDB.Close(); err != nil {
		t.Fatal(err)
	}
	w := &watchService{s: s}
	err := w.Subscribe(peerContext(t.Context()), connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}), nil)
	if connect.CodeOf(err) != connect.CodeInternal || api.Detail(err).GetReason() != api.ReasonInternal {
		t.Errorf("err = %v, want INTERNAL", err)
	}
	if err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Errorf("err = %v, want the audit failure", err)
	}
}

// TestReadEventsWaitEndsWhenAnEventArrives checks that a waiting read
// returns as soon as an event is published, long before its 60 seconds.
// It reads from cursor 0, so the result is the same if the publish wins
// the race with the first read.
func TestReadEventsWaitEndsWhenAnEventArrives(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	w := &watchService{s: s}
	ctx, cancel := context.WithTimeout(peerContext(t.Context()), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		time.Sleep(20 * time.Millisecond)
		if err := s.watchLog.Publish(ctx, testWatchEvent("late")); err != nil {
			t.Error(err)
		}
	})
	start := time.Now()
	resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), WaitSeconds: 60}))
	if err != nil {
		t.Fatal(err)
	}
	if ev := resp.Msg.GetEvents(); len(ev) != 1 || ev[0].GetId() != 1 || resp.Msg.GetNextCursor() != 1 {
		t.Errorf("resp = %v, want event 1 and cursor 1", resp.Msg)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %v, want soon after the publish", elapsed)
	}
}

// TestReadEventsWaitTimesOutEmpty checks that a wait with nothing arriving
// returns no events and the tail as its cursor, after the whole wait. The
// wait unit is a millisecond here, so wait_seconds 30 is 30ms.
func TestReadEventsWaitTimesOutEmpty(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	s.watchWaitUnit = time.Millisecond
	w := &watchService{s: s}
	start := time.Now()
	resp, err := w.ReadEvents(peerContext(t.Context()), connect.NewRequest(&chainmailv1.ReadEventsRequest{WaitSeconds: 30}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.GetEvents()) != 0 || resp.Msg.GetNextCursor() != 0 {
		t.Errorf("resp = %v, want nothing at cursor 0", resp.Msg)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned after %v, want the whole 30ms wait", elapsed)
	}
	var targets string
	if err := stateDB.QueryRowContext(t.Context(), "SELECT targets FROM audit WHERE action = 'watch.read'").Scan(&targets); err != nil {
		t.Fatal(err)
	}
	if targets != `["live","accounts=0","kinds=","in=unset","events=0"]` {
		t.Errorf("targets = %s", targets)
	}
}

// TestReadEventsCancelEndsTheWait checks that a client cancelling a read
// ends its wait, and that the read is audited as a TIMEOUT error.
func TestReadEventsCancelEndsTheWait(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	w := &watchService{s: s}
	ctx, cancel := context.WithCancel(peerContext(t.Context()))
	defer cancel()
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()
	start := time.Now()
	_, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{WaitSeconds: 60}))
	if connect.CodeOf(err) != connect.CodeCanceled {
		t.Errorf("err = %v, want Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %v, want soon after the cancel", elapsed)
	}
	var result, reason string
	if err := stateDB.QueryRowContext(t.Context(), "SELECT result, reason FROM audit WHERE action = 'watch.read'").Scan(&result, &reason); err != nil {
		t.Fatal(err)
	}
	if result != "error" || reason != api.ReasonTimeout {
		t.Errorf("audit = %s %s, want error TIMEOUT", result, reason)
	}
}

// TestReadEventsWaitEndsBeforeTheDeadline checks that a wait longer than
// the time the request's deadline leaves ends before the deadline and
// returns what it has: a quiet wait is a normal result, never TIMEOUT
// (contract C8).
func TestReadEventsWaitEndsBeforeTheDeadline(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	w := &watchService{s: s}
	// wait_seconds 60 would overrun the 1s deadline 60 times over. The wait
	// gets half of the second, so the call must return at least 200ms
	// before the deadline, which leaves a loaded machine room to be late.
	const deadline = time.Second
	ctx, cancel := context.WithTimeout(peerContext(t.Context()), deadline)
	defer cancel()
	start := time.Now()
	resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{WaitSeconds: 60}))
	if err != nil {
		t.Fatalf("ReadEvents: %v, want an empty page", err)
	}
	if elapsed := time.Since(start); elapsed >= deadline-200*time.Millisecond {
		t.Errorf("returned after %v, want well before the %v deadline", elapsed, deadline)
	}
	if len(resp.Msg.GetEvents()) != 0 || resp.Msg.GetNextCursor() != 0 {
		t.Errorf("resp = %v, want nothing at cursor 0", resp.Msg)
	}
	var result, reason string
	if err := stateDB.QueryRowContext(t.Context(), "SELECT result, reason FROM audit WHERE action = 'watch.read'").Scan(&result, &reason); err != nil {
		t.Fatal(err)
	}
	if result != "ok" || reason != "" {
		t.Errorf("audit = %s %s, want ok", result, reason)
	}
}

// TestWaitBudget checks how long a ReadEvents read may wait: the whole
// wait without a deadline, else ending before the deadline with a second
// to spare, or half the time left when less than two seconds remain.
func TestWaitBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		left     time.Duration // 0 means no deadline
		wait     time.Duration
		min, max time.Duration
	}{
		{name: "no deadline", wait: time.Minute, min: time.Minute, max: time.Minute},
		{name: "deadline far off", left: time.Minute + 30*time.Second, wait: time.Minute, min: time.Minute, max: time.Minute},
		{name: "deadline first", left: 10 * time.Second, wait: time.Minute, min: 8 * time.Second, max: 9 * time.Second},
		{name: "little time left", left: time.Second, wait: time.Minute, min: 400 * time.Millisecond, max: 500 * time.Millisecond},
		{name: "no wait", left: time.Minute, wait: 0, min: 0, max: 0},
		{name: "deadline passed", left: -time.Second, wait: time.Minute, min: 0, max: 0},
	}
	for _, tt := range tests {
		ctx := t.Context()
		if tt.left != 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, time.Now().Add(tt.left))
			defer cancel()
		}
		if got := waitBudget(ctx, tt.wait); got < tt.min || got > tt.max {
			t.Errorf("%s: waitBudget = %v, want %v to %v", tt.name, got, tt.min, tt.max)
		}
	}
}

// seedStaleGroup creates group acknowledged at acked, past the tail.
func seedStaleGroup(t *testing.T, s *Server, group string, acked int64) {
	t.Helper()
	if _, _, err := s.watchLog.Group(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if _, err := s.opts.StateDB.ExecContext(t.Context(), "UPDATE event_groups SET acked_id = ? WHERE name = ?", acked, group); err != nil {
		t.Fatal(err)
	}
}

// TestStaleGroupResetRacesAPublish checks that a group left past the tail
// is reset to the very position its read starts from. An event published
// between reading the tail and the reset must not be skipped: the group
// may not be reset past it.
func TestStaleGroupResetRacesAPublish(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	ctx := peerContext(t.Context())
	if err := s.watchLog.Publish(ctx, testWatchEvent("a"), testWatchEvent("b"), testWatchEvent("c")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 50)
	var once sync.Once
	s.watchAfterTail = func(context.Context) {
		once.Do(func() {
			if err := s.watchLog.Publish(ctx, testWatchEvent("raced")); err != nil {
				t.Error(err)
			}
		})
	}
	w := &watchService{s: s}
	read := func() *chainmailv1.ReadEventsResponse {
		t.Helper()
		resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{Group: "bot", Limit: 1}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg
	}
	first := read()
	if ev := first.GetEvents(); len(ev) != 1 || ev[0].GetKind() != "gap" || ev[0].GetNextId() != 4 || first.GetNextCursor() != 3 {
		t.Fatalf("first read = %v, want the gap to 4 at cursor 3", first)
	}
	second := read()
	if ev := second.GetEvents(); len(ev) != 1 || ev[0].GetId() != 4 || ev[0].GetMessage().GetSubject().GetUntrusted() != "raced" {
		t.Errorf("second read = %v, want event 4, published during the reset", second)
	}
}

// TestReadEventsAuditFailureLeavesTheGroup checks that a group read whose
// audit write fails does not move the group, so the next read returns the
// same page. The audit insert fails through a trigger; the event log still
// works.
func TestReadEventsAuditFailureLeavesTheGroup(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	ctx := peerContext(t.Context())
	w := &watchService{s: s}
	if _, _, err := s.watchLog.Group(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if err := s.watchLog.Publish(ctx, testWatchEvent("a"), testWatchEvent("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := stateDB.ExecContext(ctx, "CREATE TRIGGER audit_down BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT, 'audit down'); END"); err != nil {
		t.Fatal(err)
	}
	req := &chainmailv1.ReadEventsRequest{Group: "bot"}
	_, err := w.ReadEvents(ctx, connect.NewRequest(req))
	if connect.CodeOf(err) != connect.CodeInternal || err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("err = %v, want the audit failure", err)
	}
	groups, err := s.watchLog.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].AckedID != 0 {
		t.Errorf("groups = %+v, want bot still at 0", groups)
	}
	if _, err := stateDB.ExecContext(ctx, "DROP TRIGGER audit_down"); err != nil {
		t.Fatal(err)
	}
	resp, err := w.ReadEvents(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if ev := resp.Msg.GetEvents(); len(ev) != 2 || ev[0].GetId() != 1 || ev[1].GetId() != 2 || resp.Msg.GetNextCursor() != 2 {
		t.Errorf("read after the failure = %v, want events 1 and 2 again", resp.Msg)
	}
}

// TestReadEventsAuditFailureLeavesAStaleGroup checks that the reset of a
// group left past the tail waits for the audit row too: a read that could
// not be audited leaves the group where it was, so the next read still
// reports the gap rather than losing it.
func TestReadEventsAuditFailureLeavesAStaleGroup(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	ctx := peerContext(t.Context())
	w := &watchService{s: s}
	if err := s.watchLog.Publish(ctx, testWatchEvent("a")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 50)
	if _, err := stateDB.ExecContext(ctx, "CREATE TRIGGER audit_down BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT, 'audit down'); END"); err != nil {
		t.Fatal(err)
	}
	req := &chainmailv1.ReadEventsRequest{Group: "bot"}
	if _, err := w.ReadEvents(ctx, connect.NewRequest(req)); err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("err = %v, want the audit failure", err)
	}
	groups, err := s.watchLog.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].AckedID != 50 {
		t.Errorf("groups = %+v, want bot still at 50", groups)
	}
	if _, err := stateDB.ExecContext(ctx, "DROP TRIGGER audit_down"); err != nil {
		t.Fatal(err)
	}
	resp, err := w.ReadEvents(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if ev := resp.Msg.GetEvents(); len(ev) != 1 || ev[0].GetKind() != "gap" || ev[0].GetNextId() != 2 || resp.Msg.GetNextCursor() != 1 {
		t.Errorf("read after the failure = %v, want the gap to 2 at cursor 1", resp.Msg)
	}
}

// firstTime runs f the first time it is called and does nothing after, so
// a hook that starts another read, which runs the hook too, does not
// recurse. Unlike sync.Once it does not block a nested call.
func firstTime(f func(context.Context)) func(context.Context) {
	var fired atomic.Bool
	return func(ctx context.Context) {
		if fired.CompareAndSwap(false, true) {
			f(ctx)
		}
	}
}

// readerB is the second reader of a stale group bot, run while the first
// has found the group stale but not yet recovered it. Its first read gets
// the gap and recovers the group at the tail, 3. Then events 4 and 5 are
// published and its second read gets them, so bot is acknowledged at 5.
// ctx is the first reader's, which carries its peer.
// It returns every event it read.
func readerB(ctx context.Context, t *testing.T, s *Server) []*chainmailv1.WatchEvent {
	t.Helper()
	w := &watchService{s: s}
	var got []*chainmailv1.WatchEvent
	for i, publish := range []bool{false, true} {
		if publish {
			if err := s.watchLog.Publish(ctx, testWatchEvent("d"), testWatchEvent("e")); err != nil {
				t.Error(err)
				return got
			}
		}
		resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{Group: "bot"}))
		if err != nil {
			t.Errorf("reader B read %d: %v", i+1, err)
			return got
		}
		got = append(got, resp.Msg.GetEvents()...)
	}
	return got
}

// TestStaleGroupTwoReadersReadEvents checks two ReadEvents readers of one
// stale group. Reader A finds the group at 50 with the tail at 3. Before A
// recovers it, reader B recovers it and is acknowledged at 5 for events 4
// and 5 (B's first read recovers the group, then B reads 4 and 5). A's
// recovery must then not apply: it may not lower the group to
// the 3 its one-event page reached, and A reads on as a normal read from
// 5, with no second gap and no replay of 4 or 5.
func TestStaleGroupTwoReadersReadEvents(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	ctx := peerContext(t.Context())
	w := &watchService{s: s}
	if err := s.watchLog.Publish(ctx, testWatchEvent("a"), testWatchEvent("b"), testWatchEvent("c")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 50)
	var bGot []*chainmailv1.WatchEvent
	s.watchAfterTail = firstTime(func(hookCtx context.Context) { bGot = readerB(hookCtx, t, s) })
	resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{Group: "bot", Limit: 1}))
	if err != nil {
		t.Fatalf("reader A: %v", err)
	}
	if len(bGot) != 3 || bGot[0].GetKind() != "gap" || bGot[1].GetId() != 4 || bGot[2].GetId() != 5 {
		t.Fatalf("reader B = %v, want the gap then 4 and 5", bGot)
	}
	if len(resp.Msg.GetEvents()) != 0 || resp.Msg.GetNextCursor() != 5 {
		t.Errorf("reader A = %v, want nothing at cursor 5: no second gap, no replay", resp.Msg)
	}
	groups, err := s.watchLog.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].AckedID != 5 {
		t.Errorf("groups = %+v, want bot at 5", groups)
	}
}

// TestStaleGroupTwoReadersSubscribe is the same race with a Subscribe
// stream as reader A. A has already found the group stale, so it sends the
// one gap it owes, but its recovery does not apply, so it resumes from the
// group's 5 rather than lowering it to 3 and replaying 4 and 5.
func TestStaleGroupTwoReadersSubscribe(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: time.Hour, DisableWatch: true})
	ctx := peerContext(t.Context())
	if err := s.watchLog.Publish(ctx, testWatchEvent("a"), testWatchEvent("b"), testWatchEvent("c")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 50)
	s.watchAfterTail = firstTime(func(hookCtx context.Context) { readerB(hookCtx, t, s) })
	c := serveWatchTest(t, s)
	sctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := c.Watch.Subscribe(sctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var items []*chainmailv1.SubscribeResponse
	for stream.Receive() {
		items = append(items, stream.Msg())
		if stream.Msg().GetHeartbeat() != nil {
			break
		}
	}
	if len(items) != 2 || items[0].GetEvent().GetKind() != "gap" || items[1].GetHeartbeat().GetPosition() != 5 {
		t.Errorf("reader A got %v (%v), want the gap then a heartbeat at 5 with no replay", items, stream.Err())
	}
	groups, err := s.watchLog.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].AckedID != 5 {
		t.Errorf("groups = %+v, want bot at 5", groups)
	}
}

// TestSubscribeStaleGroupStaysStaleUntilTheGapIsSent checks that a stream
// that ends before it delivers the trimmed gap leaves the group stale, so
// the reconnect gets the gap. The first stream is cancelled right after
// its start is read, and the hook waits for the daemon to see that.
func TestSubscribeStaleGroupStaysStaleUntilTheGapIsSent(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: time.Hour, DisableWatch: true})
	ctx := peerContext(t.Context())
	if err := s.watchLog.Publish(ctx, testWatchEvent("a"), testWatchEvent("b"), testWatchEvent("c")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 50)
	c := serveWatchTest(t, s)
	firstCtx, cancelFirst := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelFirst()
	s.watchAfterTail = firstTime(func(handlerCtx context.Context) {
		cancelFirst()
		<-handlerCtx.Done()
	})
	first, err := c.Watch.Subscribe(firstCtx, connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
	if err == nil {
		for first.Receive() {
		}
		_ = first.Close()
	}
	groups, err := s.watchLog.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].AckedID != 50 {
		t.Fatalf("after the cancelled stream groups = %+v, want bot still stale at 50", groups)
	}
	sctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := c.Watch.Subscribe(sctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() || stream.Msg().GetEvent().GetKind() != "gap" || stream.Msg().GetEvent().GetNextId() != 4 {
		t.Errorf("reconnect first item = %v (%v), want the gap to 4", stream.Msg(), stream.Err())
	}
}

// TestWaitReadQuietWhenTheDeadlinePassesToo checks that a wait that ends
// with nothing new is a quiet result even when the request's deadline has
// passed by the time waitRead looks, as a late wakeup on a loaded machine
// finds it. Here the deadline comes before the wait ends, so both have
// passed when the wait returns.
func TestWaitReadQuietWhenTheDeadlinePassesToo(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	w := &watchService{s: s}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	page, err := w.waitRead(ctx, watch.Query{After: 0}, watch.Page{Next: 0}, time.Second)
	if err != nil || len(page.Events) != 0 || page.Next != 0 {
		t.Errorf("waitRead = %+v, %v; want an empty page at 0 and no error", page, err)
	}
}

// lateCtx is a request context whose deadline the test passes by hand,
// so a deadline that passes during a late wakeup is deterministic. It
// reports a deadline an hour away until expire, then ends with
// DeadlineExceeded. Values come from the parent; its cancellation does not.
type lateCtx struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	once     sync.Once
}

func newLateCtx(parent context.Context) *lateCtx {
	return &lateCtx{Context: parent, deadline: time.Now().Add(time.Hour), done: make(chan struct{})}
}

func (c *lateCtx) Deadline() (time.Time, bool) { return c.deadline, true }

func (c *lateCtx) Done() <-chan struct{} { return c.done }

func (c *lateCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *lateCtx) expire() { c.once.Do(func() { close(c.done) }) }

// TestReadEventsGroupQuietPastTheDeadline checks a group read whose wait
// wakes late, after the request's deadline: it is still a quiet result,
// an empty page with no error, and the group is acknowledged at the page's
// Next, past the events its filter dropped. The acknowledgement must not
// fail on the expired deadline.
func TestReadEventsGroupQuietPastTheDeadline(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	w := &watchService{s: s}
	base := peerContext(t.Context())
	if _, _, err := s.watchLog.Group(base, "bot"); err != nil {
		t.Fatal(err)
	}
	if err := s.watchLog.Publish(base, testWatchEvent("a"), testWatchEvent("b")); err != nil {
		t.Fatal(err)
	}
	// The request's deadline passes while the wait wakes, as on a loaded
	// machine. The wait itself is wait_seconds 20 of a millisecond each.
	s.watchWaitUnit = time.Millisecond
	ctx := newLateCtx(base)
	s.watchAfterWait = func(context.Context) { ctx.expire() }
	resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{
		Group: "bot", WaitSeconds: 20, Filter: &chainmailv1.WatchFilter{Kinds: []string{"flags"}},
	}))
	if err != nil {
		t.Fatalf("ReadEvents: %v, want an empty page", err)
	}
	if len(resp.Msg.GetEvents()) != 0 || resp.Msg.GetNextCursor() != 2 {
		t.Errorf("resp = %v, want nothing at cursor 2", resp.Msg)
	}
	groups, err := s.watchLog.Groups(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].AckedID != 2 {
		t.Errorf("groups = %+v, want bot acknowledged at 2", groups)
	}
	var result string
	if err := stateDB.QueryRowContext(base, "SELECT result FROM audit WHERE action = 'watch.read'").Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != "ok" {
		t.Errorf("audit result = %s, want ok", result)
	}
}

// TestSubscribeGapSendFailureLeavesTheGroupStale checks the order of a
// stale group's gap and its recovery: a gap send that fails while the
// stream is still live leaves the group stale, so the reconnect gets the
// gap, and only a delivered gap recovers the group.
func TestSubscribeGapSendFailureLeavesTheGroupStale(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: time.Hour, DisableWatch: true})
	ctx := peerContext(t.Context())
	if err := s.watchLog.Publish(ctx, testWatchEvent("a"), testWatchEvent("b"), testWatchEvent("c")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 50)
	var failed atomic.Bool
	s.watchSendHook = func(item *chainmailv1.SubscribeResponse) error {
		if item.GetEvent().GetKind() == "gap" && failed.CompareAndSwap(false, true) {
			return errors.New("send failed")
		}
		return nil
	}
	c := serveWatchTest(t, s)
	acked := func() int64 {
		t.Helper()
		groups, err := s.watchLog.Groups(ctx)
		if err != nil || len(groups) != 1 {
			t.Fatalf("groups = %+v, %v", groups, err)
		}
		return groups[0].AckedID
	}
	first, err := c.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
	if err != nil {
		t.Fatal(err)
	}
	for first.Receive() {
		t.Errorf("first stream sent %v", first.Msg())
	}
	if first.Err() == nil {
		t.Error("first stream ended without an error")
	}
	_ = first.Close()
	if got := acked(); got != 50 {
		t.Fatalf("after the failed gap send bot is at %d, want still stale at 50", got)
	}
	sctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := c.Watch.Subscribe(sctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() || stream.Msg().GetEvent().GetKind() != "gap" {
		t.Fatalf("reconnect first item = %v (%v), want the gap", stream.Msg(), stream.Err())
	}
	if !stream.Receive() || stream.Msg().GetHeartbeat().GetPosition() != 3 {
		t.Fatalf("reconnect second item = %v (%v), want a heartbeat at 3", stream.Msg(), stream.Err())
	}
	if got := acked(); got != 3 {
		t.Errorf("after the delivered gap bot is at %d, want recovered at 3", got)
	}
}

// ackedAt is the acknowledged ID of the only group.
func ackedAt(t *testing.T, s *Server) int64 {
	t.Helper()
	groups, err := s.watchLog.Groups(t.Context())
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	return groups[0].AckedID
}

// TestSettleSkipsCancelledReads checks that a read whose client has gone
// away is never acknowledged, neither by an Ack nor by a stale group's
// reset, even though a read whose deadline merely passed still is.
func TestSettleSkipsCancelledReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		seed  int64 // the group's acknowledged ID before the read
		st    readStart
		id    int64
		after int64 // the group's acknowledged ID after a cancelled read
	}{
		{name: "ack", seed: 1, id: 3, after: 1},
		{name: "stale reset", seed: 50, st: readStart{after: 3, stale: true, from: 50}, id: 3, after: 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServer(t)
			w := &watchService{s: s}
			if err := s.watchLog.Publish(t.Context(), testWatchEvent("a"), testWatchEvent("b"), testWatchEvent("c")); err != nil {
				t.Fatal(err)
			}
			seedStaleGroup(t, s, "bot", tt.seed)
			ctx, cancel := context.WithCancel(peerContext(t.Context()))
			cancel()
			if _, err := w.ack(ctx, "bot", tt.st, tt.id); connect.CodeOf(err) != connect.CodeCanceled {
				t.Errorf("ack = %v, want Canceled", err)
			}
			if got := ackedAt(t, s); got != tt.after {
				t.Errorf("bot is at %d after a cancelled read, want %d", got, tt.after)
			}
			late := newLateCtx(peerContext(t.Context()))
			late.expire()
			if _, err := w.ack(late, "bot", tt.st, tt.id); err != nil {
				t.Errorf("ack past the deadline = %v, want it settled", err)
			}
			if got := ackedAt(t, s); got != tt.id {
				t.Errorf("bot is at %d after a read past its deadline, want %d", got, tt.id)
			}
		})
	}
}

// TestSettleEndsOnShutdown checks that a daemon shutdown ends settlement
// even once the request's deadline has passed, when the request context
// itself no longer reports the shutdown: nothing is acknowledged.
func TestSettleEndsOnShutdown(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	w := &watchService{s: s}
	if err := s.watchLog.Publish(t.Context(), testWatchEvent("a"), testWatchEvent("b")); err != nil {
		t.Fatal(err)
	}
	seedStaleGroup(t, s, "bot", 0)
	shutdown := make(chan struct{})
	s.shutdown = shutdown
	late := newLateCtx(peerContext(t.Context()))
	late.expire()
	close(shutdown)
	if _, err := w.ack(late, "bot", readStart{}, 2); connect.CodeOf(err) != connect.CodeCanceled {
		t.Errorf("ack during shutdown = %v, want Canceled", err)
	}
	if got := ackedAt(t, s); got != 0 {
		t.Errorf("bot is at %d after a shutdown, want 0", got)
	}
}

// settleAsync runs s.settle on a joined worker with a write that signals
// when it starts, then blocks until its context ends and returns that
// context's error. It returns the started signal and the channel settle's
// result arrives on. A cleanup releases the write, whatever the behavior
// under test did, and waits for the worker, so it never outlives the test.
func settleAsync(ctx context.Context, t *testing.T, s *Server) (started <-chan struct{}, result <-chan error) {
	t.Helper()
	start := make(chan struct{})
	release := make(chan struct{})
	res := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		res <- s.settle(ctx, func(wctx context.Context) error {
			close(start)
			select {
			case <-wctx.Done():
				return wctx.Err()
			case <-release:
				return errors.New("released by the test's cleanup")
			}
		})
	})
	t.Cleanup(func() {
		close(release)
		wg.Wait()
	})
	return start, res
}

// settleResult waits for settle to return, failing the test if it hangs.
func settleResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2500 * time.Millisecond): // half the 5s bound
		t.Fatal("settle did not return after its write's context should have ended")
		return nil
	}
}

// TestSettleCancelDuringWrite checks that a client going away while a
// settlement write runs ends the write's context, and that settle then
// returns the cancellation, with its watchers joined.
func TestSettleCancelDuringWrite(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(peerContext(t.Context()))
	defer cancel()
	started, result := settleAsync(ctx, t, s)
	<-started
	cancel()
	if err := settleResult(t, result); !errors.Is(err, context.Canceled) {
		t.Errorf("settle = %v, want Canceled", err)
	}
}

// TestSettleShutdownDuringWrite checks that a shutdown while a settlement
// write runs ends it, even though the request's context had already ended
// with DeadlineExceeded when settlement began, so the request context
// itself never reports the shutdown.
func TestSettleShutdownDuringWrite(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	shutdown := make(chan struct{})
	s.shutdown = shutdown
	late := newLateCtx(peerContext(t.Context()))
	late.expire()
	started, result := settleAsync(late, t, s)
	<-started
	close(shutdown)
	if err := settleResult(t, result); !errors.Is(err, context.Canceled) {
		t.Errorf("settle = %v, want Canceled", err)
	}
}

// TestSettleBound checks the bound on a settlement write: a deadline
// 5 seconds from when it starts, in place of the request's deadline,
// which has already passed. Only the deadline is read, so nothing waits
// the 5 seconds out.
func TestSettleBound(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	expired, cancel := context.WithTimeout(peerContext(t.Context()), -time.Second)
	defer cancel()
	before := time.Now()
	var deadline time.Time
	var ok bool
	err := s.settle(expired, func(wctx context.Context) error {
		deadline, ok = wctx.Deadline()
		return wctx.Err()
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("settle past the request's deadline = %v, want nil", err)
	}
	const bound = 5 * time.Second
	if !ok || deadline.Before(before.Add(bound)) || deadline.After(after.Add(bound)) {
		t.Errorf("write deadline = %v, %v; want 5s after the call, between %v and %v",
			deadline, ok, before.Add(bound), after.Add(bound))
	}
}

// TestQuietGroupStreamOutlivesRetention checks that a group stream that
// stays open past the retention window with nothing to send keeps its group:
// each heartbeat refreshes the group's last use, so Trim does not prune it,
// and a short disconnect then resumes after the group's acknowledged event
// instead of starting over at the tail.
func TestQuietGroupStreamOutlivesRetention(t *testing.T) {
	t.Parallel()
	var now atomic.Pointer[time.Time]
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	now.Store(&start)
	clock := func() time.Time { return *now.Load() }
	s, _ := newTestServerOptions(t, passwordConfig, Options{Now: clock, WatchHeartbeat: 5 * time.Millisecond, DisableWatch: true})
	c := serveWatchTest(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	open := func() *connect.ServerStreamForClient[chainmailv1.SubscribeResponse] {
		stream, err := c.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Group: "bot"}))
		if err != nil {
			t.Fatal(err)
		}
		return stream
	}
	heartbeat := func(stream *connect.ServerStreamForClient[chainmailv1.SubscribeResponse]) {
		t.Helper()
		if !stream.Receive() || stream.Msg().GetHeartbeat() == nil {
			t.Fatalf("item = %v (%v), want a heartbeat", stream.Msg(), stream.Err())
		}
	}
	stream := open()
	heartbeat(stream)
	later := start.Add(200 * time.Hour)
	now.Store(&later)
	// The first heartbeat after the jump may have stamped the old time.
	heartbeat(stream)
	heartbeat(stream)
	if _, err := s.watchLog.Trim(ctx, 168*time.Hour, 1000); err != nil {
		t.Fatal(err)
	}
	groups, err := s.watchLog.Groups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Name != "bot" {
		t.Fatalf("Groups after Trim = %+v, %v; want bot kept", groups, err)
	}
	_ = stream.Close() // a short disconnect
	if err := s.watchLog.Publish(ctx, testWatchEvent("while away")); err != nil {
		t.Fatal(err)
	}
	stream = open()
	defer func() { _ = stream.Close() }()
	if !stream.Receive() {
		t.Fatalf("stream ended: %v", stream.Err())
	}
	if e := stream.Msg().GetEvent(); e.GetId() != 1 || e.GetMessage().GetSubject().GetUntrusted() != "while away" {
		t.Errorf("first item after the reconnect = %v, want event 1", stream.Msg())
	}
}
