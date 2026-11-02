//go:build integration

package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/dovecot"
)

// watchDoveConfig is one account on Dovecot that watches INBOX, the
// default. The SMTP endpoint is never dialled: the test sends nothing.
const watchDoveConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = 1, security = "starttls" }
`

const (
	// doveHeartbeat is the stream heartbeat, short so the test can tell
	// when a stream has gone quiet.
	doveHeartbeat = 250 * time.Millisecond
	// doveSweepFloor replaces the 5 minute floor between sweeps that a
	// server EXPUNGE triggers, which counts from the baseline's sweep.
	doveSweepFloor = time.Second
	// doveEventWithin is the spec's bound for new and flags events.
	doveEventWithin = 5 * time.Second
	// doveExpungeWithin covers the sweep an EXPUNGE triggers, at most
	// doveSweepFloor after the one before.
	doveExpungeWithin = 15 * time.Second
	// doveReadyWithin bounds a baseline, a catch-up and a first heartbeat.
	doveReadyWithin = 30 * time.Second
)

// doveWatchMail is a small message with subject and Message-ID <id@example.org>.
func doveWatchMail(subject, id string) string {
	return "From: Ann <ann@example.org>\r\nTo: watch@example.test\r\nSubject: " + subject + "\r\n" +
		"Message-ID: <" + id + "@example.org>\r\nDate: Mon, 02 Nov 2026 10:00:00 +0000\r\n\r\nWatch me.\r\n"
}

// TestWatchAgainstDovecot runs the daemon's watcher against Dovecot, the
// CONDSTORE path end to end. Another IMAP client delivers a message, flags
// it and expunges it, and a group stream gets new, flags and expunged.
// Then the daemon stops, three messages arrive while it is down, and after
// a restart on the same state the group resumes with exactly those three.
func TestWatchAgainstDovecot(t *testing.T) {
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	const user = "watch@example.test"
	cfg := fmt.Sprintf(watchDoveConfig, user, dove.Endpoint.Port)
	opts := daemontest.Options{
		RootCAs:         dove.CA.Pool,
		Watch:           true,
		WatchHeartbeat:  doveHeartbeat,
		WatchSweepFloor: doveSweepFloor,
		Secrets:         map[string]string{"accounts/dove": dovecot.Password},
	}
	h := daemontest.Start(t, cfg, opts)

	// 0. INBOX is baselined and watched over IDLE, with CONDSTORE.
	st := waitDoveWatching(t, h, "dove", "INBOX")
	if st.GetMode() != "idle" || !st.GetCondstore() || st.GetLastError() != "" {
		t.Fatalf("INBOX sync status = %+v, want mode idle, CONDSTORE and no error", st)
	}

	// 1. A message another client delivers arrives as new.
	w := subscribeDove(t, h, &chainmailv1.SubscribeRequest{
		Group:  "it",
		Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}},
	})
	w.heartbeat(doveReadyWithin) // the group exists and the stream has caught up
	uid, err := dove.Append(user, "INBOX", doveWatchMail("Hello watcher", "w1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ev := w.next("new", doveEventWithin, nil)
	r := parseRef(t, ev.GetRef())
	if ev.GetAccount() != "dove" || ev.GetFolder() != "INBOX" || r.UID != uint32(uid) {
		t.Errorf("new event %s in %s/%s, want UID %d in dove/INBOX", ev.GetRef(), ev.GetAccount(), ev.GetFolder(), uid)
	}
	msg := ev.GetMessage()
	if got := msg.GetSubject().GetUntrusted(); got != "Hello watcher" {
		t.Errorf("new event subject = %q, want Hello watcher", got)
	}
	if msg.GetRef() != ev.GetRef() || msg.GetAccount() != "dove" || msg.GetFolder() != "INBOX" {
		t.Errorf("new event message row = ref %q in %s/%s, want the event's ref in dove/INBOX", msg.GetRef(), msg.GetAccount(), msg.GetFolder())
	}

	// 2. A flag another client sets arrives as flags, with the current values.
	if err := dove.SetFlag(user, "INBOX", uid, `\Flagged`, true); err != nil {
		t.Fatal(err)
	}
	fl := w.next("flags", doveEventWithin, func(e *chainmailv1.WatchEvent) bool { return e.GetRef() == ev.GetRef() })
	if !slices.Contains(fl.GetFlags(), `\Flagged`) {
		t.Errorf("flags event flags = %q, want \\Flagged among them", fl.GetFlags())
	}

	// 3. An expunge from another client arrives as expunged, after the
	// sweep the server's EXPUNGE triggers. Marking it \Deleted first may
	// add a flags event, which next passes over.
	if err := dove.Expunge(user, "INBOX", uid); err != nil {
		t.Fatal(err)
	}
	w.next("expunged", doveExpungeWithin, func(e *chainmailv1.WatchEvent) bool { return e.GetRef() == ev.GetRef() })

	// The group acknowledges everything so far, as chainmail watch --group
	// does with a heartbeat's position.
	hb := w.heartbeat(doveReadyWithin)
	ackDove(t, h, "it", hb.GetPosition())
	w.close()

	// 4. Restart durability: three messages arrive while the daemon is
	// down, and the group resumes with exactly those three new events.
	if err := h.Stop(); err != nil {
		t.Fatalf("stopping the daemon: %v", err)
	}
	var want []string
	for i := range 3 {
		subject := fmt.Sprintf("While down %d", i+1)
		if _, err := dove.Append(user, "INBOX", doveWatchMail(subject, fmt.Sprintf("down%d", i+1)), time.Now()); err != nil {
			t.Fatal(err)
		}
		want = append(want, subject)
	}
	restarted := opts
	restarted.Reuse = h
	h2 := daemontest.Start(t, cfg, restarted)
	w2 := subscribeDove(t, h2, &chainmailv1.SubscribeRequest{
		Group:  "it",
		Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}, Kinds: []string{"new"}},
	})
	var got []string
	for len(got) < len(want) {
		got = append(got, w2.next("new", doveReadyWithin, nil).GetMessage().GetSubject().GetUntrusted())
	}
	if !slices.Equal(got, want) {
		t.Errorf("new events after the restart = %q, want %q", got, want)
	}
	// The catch-up commits its batch in one transaction, so a duplicate
	// would already be in the log. Four quiet heartbeats show there is none.
	w2.quiet("new", 4)
	w2.close()

	// 5. Audit: one watch.subscribe row per stream, naming the group and
	// holding no message content. Ack and GetWatchStatus are not audited.
	subs := 0
	for _, row := range h2.AuditRows(t) {
		text := fmt.Sprintf("%+v", row)
		if strings.Contains(text, "Hello watcher") || strings.Contains(text, "While down") {
			t.Errorf("audit row %s holds message content", text)
		}
		switch {
		case row.Action == "watch.subscribe":
			subs++
			if row.Client != "test" || !strings.Contains(row.Targets, "group=it") {
				t.Errorf("watch.subscribe row %s, want client test and target group=it", text)
			}
		case strings.HasPrefix(row.Action, "watch."):
			t.Errorf("unexpected audit row %s: Ack and GetWatchStatus are not audited", text)
		}
	}
	if subs != 2 {
		t.Errorf("%d watch.subscribe audit rows, want 2 (one per daemon)", subs)
	}
}

// waitDoveWatching polls GetWatchStatus until account's folder is in the
// watching state and returns its status. A restarted daemon keeps the
// state stored before it stopped, so use this on a fresh daemon only.
func waitDoveWatching(t *testing.T, h *daemontest.Harness, account, folder string) *chainmailv1.FolderSyncStatus {
	t.Helper()
	deadline := time.Now().Add(doveReadyWithin)
	for {
		resp, err := h.Client.Watch.GetWatchStatus(t.Context(), connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
		if err != nil {
			t.Fatalf("GetWatchStatus: %v", err)
		}
		for _, f := range resp.Msg.GetFolders() {
			if f.GetAccount() == account && f.GetFolder() == folder && f.GetState() == "watching" {
				return f
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s/%s not watching within %v: %v", account, folder, doveReadyWithin, resp.Msg.GetFolders())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ackDove acknowledges id for group.
func ackDove(t *testing.T, h *daemontest.Harness, group string, id int64) {
	t.Helper()
	if _, err := h.Client.Watch.Ack(t.Context(), connect.NewRequest(&chainmailv1.AckRequest{Group: group, Id: id})); err != nil {
		t.Fatalf("Ack(%s, %d): %v", group, id, err)
	}
}

// doveStream reads a Subscribe stream on a goroutine of its own, so the
// test can wait for an item with a deadline.
type doveStream struct {
	t      *testing.T
	items  chan *chainmailv1.SubscribeResponse
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// err is how the stream ended. Read it only once items is closed.
	err error
}

// subscribeDove opens a Subscribe stream. The test's cleanup closes it.
func subscribeDove(t *testing.T, h *daemontest.Harness, req *chainmailv1.SubscribeRequest) *doveStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := h.Client.Watch.Subscribe(ctx, connect.NewRequest(req))
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	w := &doveStream{t: t, items: make(chan *chainmailv1.SubscribeResponse, 64), cancel: cancel}
	w.wg.Go(func() {
		defer close(w.items)
		defer func() { _ = stream.Close() }() // how it ended is in err
		for stream.Receive() {
			select {
			case w.items <- stream.Msg():
			case <-ctx.Done():
				w.err = ctx.Err()
				return
			}
		}
		w.err = stream.Err()
	})
	t.Cleanup(w.close)
	return w
}

// close ends the stream and waits for its reader. Later calls do nothing.
func (w *doveStream) close() {
	w.cancel()
	w.wg.Wait()
}

// item returns the stream's next item, failing the test after within.
func (w *doveStream) item(within time.Duration, what string) *chainmailv1.SubscribeResponse {
	w.t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case it, ok := <-w.items:
		if !ok {
			w.t.Fatalf("waiting for %s: the stream ended: %v", what, w.err)
		}
		return it
	case <-timer.C:
		w.t.Fatalf("no %s within %v", what, within)
	}
	return nil
}

// next returns the first event of kind that match accepts (any, when match
// is nil), passing over heartbeats and other events, within the deadline.
func (w *doveStream) next(kind string, within time.Duration, match func(*chainmailv1.WatchEvent) bool) *chainmailv1.WatchEvent {
	w.t.Helper()
	deadline := time.Now().Add(within)
	for {
		ev := w.item(time.Until(deadline), kind+" event").GetEvent()
		if ev == nil {
			continue
		}
		if ev.GetKind() == kind && (match == nil || match(ev)) {
			return ev
		}
		w.t.Logf("passing over event %d: %s %s %s", ev.GetId(), ev.GetKind(), ev.GetRef(), ev.GetState())
	}
}

// heartbeat returns the next heartbeat, passing over events.
func (w *doveStream) heartbeat(within time.Duration) *chainmailv1.Heartbeat {
	w.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if hb := w.item(time.Until(deadline), "heartbeat").GetHeartbeat(); hb != nil {
			return hb
		}
	}
}

// quiet reads until beats heartbeats have passed and fails the test for
// every event of kind among them.
func (w *doveStream) quiet(kind string, beats int) {
	w.t.Helper()
	for seen := 0; seen < beats; {
		it := w.item(doveReadyWithin, "heartbeat")
		if it.GetHeartbeat() != nil {
			seen++
			continue
		}
		if ev := it.GetEvent(); ev.GetKind() == kind {
			w.t.Errorf("extra %s event %d: %s", kind, ev.GetId(), ev.GetRef())
		}
	}
}
