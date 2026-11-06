package daemon_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/watch"
)

// watchRunWait bounds every wait on the daemon's watchers in these tests.
// A fake store answers at once, so only a broken daemon reaches it.
const watchRunWait = 10 * time.Second

var watchRunDate = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// startWatchRun runs a daemon whose host account is fk, watching or not.
// The password is stored before the daemon starts, so a supervisor does not
// park the account as needing authentication.
func startWatchRun(t *testing.T, fk *fake.Store, watching bool) *daemontest.Harness {
	t.Helper()
	return daemontest.Start(t, hostingerConfig, daemontest.Options{
		Watch:    watching,
		Secrets:  map[string]string{"accounts/host": "hunter2"},
		NewStore: func(config.ResolvedAccount) mailstore.Store { return fk },
	})
}

// subscribeFromStart streams every event in the log, then the new ones,
// until ctx ends.
func subscribeFromStart(ctx context.Context, t *testing.T, h *daemontest.Harness) *connect.ServerStreamForClient[chainmailv1.SubscribeResponse] {
	t.Helper()
	stream, err := h.Client.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Since: proto.Int64(0)}))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// awaitWatchEvent returns the next event match accepts, skipping heartbeats
// and other events. It fails the test when the stream ends first, which the
// caller's context deadline bounds.
func awaitWatchEvent(t *testing.T, stream *connect.ServerStreamForClient[chainmailv1.SubscribeResponse], match func(*chainmailv1.WatchEvent) bool) *chainmailv1.WatchEvent {
	t.Helper()
	for stream.Receive() {
		if ev := stream.Msg().GetEvent(); ev != nil && match(ev) {
			return ev
		}
	}
	t.Fatalf("the stream ended before the event arrived: %v", stream.Err())
	return nil
}

// watchingStatus matches the status event of folder going to watching.
func watchingStatus(folder string) func(*chainmailv1.WatchEvent) bool {
	return func(ev *chainmailv1.WatchEvent) bool {
		return ev.GetKind() == string(watch.KindStatus) && ev.GetFolder() == folder && ev.GetState() == string(watch.StateWatching)
	}
}

// A message delivered after the daemon started arrives as a new event over
// Subscribe, mail already there when the folder was baselined does not, and
// admin status then lists the watched folder.
func TestWatchDeliversNewMail(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	fk.Add(fake.Message{Folder: "INBOX", Subject: "already here", Date: watchRunDate})
	h := startWatchRun(t, fk, true)
	ctx, cancel := context.WithTimeout(t.Context(), watchRunWait)
	defer cancel()
	stream := subscribeFromStart(ctx, t, h)
	awaitWatchEvent(t, stream, watchingStatus("INBOX"))

	fk.Add(fake.Message{Folder: "INBOX", Subject: "hello", Date: watchRunDate})
	ev := awaitWatchEvent(t, stream, func(ev *chainmailv1.WatchEvent) bool { return ev.GetKind() == string(watch.KindNew) })
	if got := ev.GetMessage().GetSubject().GetUntrusted(); got != "hello" {
		t.Errorf("first new event subject = %q, want hello: baselined mail must not produce new events", got)
	}
	if ev.GetAccount() != "host" || ev.GetFolder() != "INBOX" || ev.GetRef() == "" || ev.GetId() == 0 {
		t.Errorf("event = %v, want host INBOX with a ref and an ID", ev)
	}

	resp, err := h.Client.Admin.GetStatus(ctx, connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range resp.Msg.GetSync() {
		found = found || (f.GetAccount() == "host" && f.GetFolder() == "INBOX" && f.GetMode() == "idle")
	}
	if !found {
		t.Errorf("admin status sync = %v, want host INBOX in idle mode", resp.Msg.GetSync())
	}
}

// Stopping the daemon with a watcher connected returns within the
// supervisors' logout bound, and every watch connection is closed before
// the store logs out.
func TestStopEndsWatchers(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	h := startWatchRun(t, fk, true)
	ctx, cancel := context.WithTimeout(t.Context(), watchRunWait)
	defer cancel()
	stream := subscribeFromStart(ctx, t, h)
	awaitWatchEvent(t, stream, watchingStatus("INBOX"))
	if n := fk.WatchConns(); n != 1 {
		t.Fatalf("watch connections while watching = %d, want 1", n)
	}

	start := time.Now()
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if d := time.Since(start); d > watch.ShutdownLogout {
		t.Errorf("Stop took %v, want at most %v", d, watch.ShutdownLogout)
	}
	if n := fk.WatchConns(); n != 0 {
		t.Errorf("watch connections after Stop = %d, want 0", n)
	}
	if !fk.Closed() {
		t.Error("the store was not closed")
	}
}

// Without Watch the harness passes DisableWatch: the store is used, but no
// supervisor opens a watch connection, polls or writes sync state.
func TestDisableWatchStartsNoSupervisor(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	h := startWatchRun(t, fk, false)
	if _, err := h.Client.Message.ListFolders(t.Context(), connect.NewRequest(&chainmailv1.ListFoldersRequest{})); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, method := range []string{"OpenWatch", "FolderState", "SyncFolder"} {
		if n := fk.Calls(method); n != 0 {
			t.Errorf("%s calls = %d, want 0", method, n)
		}
	}
	var n int
	if err := h.DB.QueryRowContext(t.Context(), "SELECT count(*) FROM sync_folders").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sync_folders rows = %d, want 0", n)
	}
}

// admin status lists each synced folder from the sync state, in account
// and folder order, and is still not audited.
func TestGetStatusReportsSync(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	st := watch.NewState(h.DB)
	if err := st.SetStatus(t.Context(), "host", "Sent", "poll", watch.StateError, "FOLDER_NOT_FOUND"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(t.Context(), "host", "INBOX", "idle", watch.StateWatching, ""); err != nil {
		t.Fatal(err)
	}
	resp, err := h.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.GetSync()
	if len(got) != 2 {
		t.Fatalf("sync = %v, want 2 rows", got)
	}
	want := []struct{ folder, mode, state, lastError string }{
		{"INBOX", "idle", string(watch.StateWatching), ""},
		{"Sent", "poll", string(watch.StateError), "FOLDER_NOT_FOUND"},
	}
	for i, w := range want {
		g := got[i]
		if g.GetAccount() != "host" || g.GetFolder() != w.folder || g.GetMode() != w.mode || g.GetState() != w.state || g.GetLastError() != w.lastError {
			t.Errorf("sync[%d] = %v, want host %s %s %s %q", i, g, w.folder, w.mode, w.state, w.lastError)
		}
	}
	if rows := h.AuditRows(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}
