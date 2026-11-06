//go:build live

package live

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

const (
	// watchWithin is how soon after the send starts its new event must
	// arrive: spec section 7, Live.
	watchWithin = 30 * time.Second
	// caughtUpWithin bounds the wait for a live stream's first heartbeat,
	// which the daemon sends as soon as the stream has caught up.
	caughtUpWithin = 45 * time.Second
)

// TestEveryAccountWatchSeesItsOwnSend is the done-when test of spec
// section 1: for each account it opens what chainmail watch --account <a>
// --kind new opens, sends a message to the account's own address, and
// requires the new event for its delivered copy within 30 seconds. On
// Google the event is for the one All Mail message and carries \Sent.
// Like the other write tests it logs refs and counts only, and its
// sweeper removes the message afterwards.
func TestEveryAccountWatchSeesItsOwnSend(t *testing.T) {
	requireWrites(t)
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			waitBudget(t, 0)
			google := isGoogle(a)
			token := "cmlive" + randomToken(16)
			t.Logf("token %s", token)
			roles := folderRoles(t, c, a.GetName())
			sw := newSweeper(t, c, a, roles, token)

			// A live stream, from now on, of the account's new events.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream, err := c.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{
				Filter: &chainmailv1.WatchFilter{Accounts: []string{a.GetName()}, Kinds: []string{"new"}},
			}))
			if err != nil {
				t.Fatalf("Subscribe: %v (%s)", err, hint(err))
			}
			defer func() { _ = stream.Close() }() // cancelled by then; nothing left to report
			receive := func(what string) *chainmailv1.SubscribeResponse {
				t.Helper()
				if stream.Receive() {
					return stream.Msg()
				}
				if ctx.Err() != nil {
					t.Fatalf("no %s within the wait", what)
				}
				t.Fatalf("waiting for %s: the stream ended: %v (%s)", what, stream.Err(), hint(stream.Err()))
				return nil
			}

			// The first heartbeat says the stream is live, so the send
			// below cannot slip in before it.
			guard := time.AfterFunc(caughtUpWithin, cancel)
			hb := receive("the first heartbeat").GetHeartbeat()
			for hb == nil { // events before it are passed over
				hb = receive("the first heartbeat").GetHeartbeat()
			}
			if !guard.Stop() {
				t.Fatal("the first heartbeat came too late")
			}

			start := time.Now()
			guard = time.AfterFunc(watchWithin, cancel)
			defer guard.Stop()
			sw.sending(t)
			resp, err := c.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{
				Spec:           selfSpec(a, "chainmail live watch "+token, "A chainmail live watch test. It deletes itself."),
				IdempotencyKey: liveKey(token, "watch"),
			}))
			if err != nil {
				t.Fatalf("Send: %v (%s)", err, hint(err))
			}
			sent := sw.sentOf(t, "Send", resp.Msg.GetResult())

			// The delivered copy's new event: the inbox copy, or on Google
			// the one All Mail message. A Sent copy in a synced Sent folder
			// is passed over.
			var ev *chainmailv1.WatchEvent
			for ev == nil {
				e := receive("the new event of the send").GetEvent()
				if e == nil || e.GetKind() != "new" || e.GetAccount() != a.GetName() {
					continue
				}
				m := e.GetMessage()
				if !strings.Contains(m.GetSubject().GetUntrusted(), token) {
					continue
				}
				if !deliveredCopy(&chainmailv1.Message{Folder: e.GetFolder(), Flags: m.GetFlags(), Labels: m.GetLabels()}, google, roles) {
					t.Logf("passing over new event %d for %s in %q: not the delivered copy", e.GetId(), e.GetRef(), e.GetFolder())
					continue
				}
				ev = e
			}
			t.Logf("new event %d for %s in %q after %v", ev.GetId(), ev.GetRef(), ev.GetFolder(), time.Since(start).Round(time.Millisecond))
			// The event names the delivered copy, so cleanup need not
			// search for it.
			sw.delivered(sent.GetMessageId())
			sw.track(ev.GetRef())

			if google {
				labels := ev.GetMessage().GetLabels()
				if !slices.Contains(labels, `\Sent`) {
					t.Errorf("the new event's labels %q lack \\Sent", labels)
				}
				// Gmail files a self-send in Sent only (TestDeliveredCopy in
				// rules_test.go), so \Inbox is logged, not required.
				t.Logf("labels %q, \\Inbox %v", labels, slices.Contains(labels, `\Inbox`))
			}
		})
	}
}
