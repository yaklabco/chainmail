package daemon

import (
	"context"
	"strings"
	"sync"
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
	// The wait gets half of the 100ms, which leaves the audit write the rest.
	const deadline = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(peerContext(t.Context()), deadline)
	defer cancel()
	start := time.Now()
	resp, err := w.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{WaitSeconds: 60}))
	if err != nil {
		t.Fatalf("ReadEvents: %v, want an empty page", err)
	}
	if elapsed := time.Since(start); elapsed >= deadline {
		t.Errorf("returned after %v, want before the %v deadline", elapsed, deadline)
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
	w := &watchService{s: s, afterTail: func() {
		once.Do(func() {
			if err := s.watchLog.Publish(ctx, testWatchEvent("raced")); err != nil {
				t.Error(err)
			}
		})
	}}
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
