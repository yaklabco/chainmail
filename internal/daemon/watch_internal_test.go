package daemon

import (
	"context"
	"strings"
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
	s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: time.Hour})
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
