//go:build live

package live

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// maxEventPages bounds how far countReplyMail reads the event log.
const maxEventPages = 20

// TestEveryAccountItemHearsItsReply is the live test of spec section 10.5
// and done when item 3. For each account it creates a task in the inbox,
// sends a message to the account's own address as the watch test does,
// links that message's thread to the task, and replies to it from the
// account. It requires the reply's item_mail event within 30 seconds of
// the reply's send, exactly one of them, and the task's view to show the
// thread with both messages. Then it soft-deletes the task. Like the other
// write tests it logs refs, IDs and counts only, and its sweeper removes
// both messages afterwards. Each run leaves one deleted task behind, since
// items are never removed for good.
func TestEveryAccountItemHearsItsReply(t *testing.T) {
	requireWrites(t)
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			waitBudget(t, 0)
			token := "cmlive" + randomToken(16)
			t.Logf("token %s", token)
			roles := folderRoles(t, c, a.GetName())
			sw := newSweeper(t, c, a, roles, token)

			// 1. A task in the inbox.
			id, remove := liveTask(t, c, token)

			// 2. A message to the account's own address.
			sw.sending(t)
			resp, err := c.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{
				Spec:           selfSpec(a, "chainmail live item "+token, "A chainmail live item test. It deletes itself."),
				IdempotencyKey: liveKey(token, "item-send"),
			}))
			if err != nil {
				t.Fatalf("Send: %v (%s)", err, hint(err))
			}
			orig := sw.waitDelivered(t, sw.sentOf(t, "Send", resp.Msg.GetResult()).GetMessageId())

			// 3. Its thread, linked to the task and anchored on it.
			linked, err := c.Items.LinkThread(t.Context(), connect.NewRequest(&chainmailv1.LinkThreadRequest{ItemId: id, Ref: orig, Anchor: true}))
			if err != nil {
				t.Fatalf("LinkThread %s: %v (%s)", orig, err, hint(err))
			}
			l := linked.Msg.GetLink()
			if !linked.Msg.GetCreated() || l.GetAccount() != a.GetName() || l.GetThreadKey() == "" {
				t.Fatalf("LinkThread %s: created %v, account %q, thread key %q, want a new link on %s",
					orig, linked.Msg.GetCreated(), l.GetAccount(), l.GetThreadKey(), a.GetName())
			}
			if l.GetState() == "unavailable" {
				t.Fatalf("LinkThread %s: the new link is unavailable: %s", orig, l.GetReason())
			}
			t.Logf("linked thread %s: state %s, source %q, %d messages", l.GetThreadKey(), l.GetState(), l.GetSource(), l.GetMessages())

			// 4. A live stream, from now on, of the task's item_mail events.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream, err := c.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{
				Filter: &chainmailv1.WatchFilter{Item: id, Kinds: []string{"item_mail"}},
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
			// The first heartbeat says the stream is live, so the reply
			// below cannot slip in before it. Its position is where the
			// count in step 6 starts.
			guard := time.AfterFunc(caughtUpWithin, cancel)
			hb := receive("the first heartbeat").GetHeartbeat()
			for hb == nil { // events before it are passed over
				hb = receive("the first heartbeat").GetHeartbeat()
			}
			if !guard.Stop() {
				t.Fatal("the first heartbeat came too late")
			}

			// 5. The account replies to its own message. The reply's
			// item_mail event must arrive within watchWithin of the send.
			start := time.Now()
			guard = time.AfterFunc(watchWithin, cancel)
			defer guard.Stop()
			sw.sending(t)
			rr, err := c.Compose.Reply(t.Context(), connect.NewRequest(&chainmailv1.ReplyRequest{
				Ref: orig, Content: liveContent("A chainmail live item reply."), To: []string{a.GetAddress()},
				IdempotencyKey: liveKey(token, "item-reply"),
			}))
			if err != nil {
				t.Fatalf("Reply: %v (%s)", err, hint(err))
			}
			reply := sw.sentOf(t, "Reply", rr.Msg.GetResult())
			var ev *chainmailv1.WatchEvent
			for ev == nil {
				e := receive("the item_mail event of the reply").GetEvent()
				if e == nil || e.GetKind() != "item_mail" {
					continue
				}
				if !isReplyMail(e, token) {
					t.Logf("passing over item_mail event %d for %s: not the reply", e.GetId(), e.GetRef())
					continue
				}
				ev = e
			}
			guard.Stop()
			cancel()
			t.Logf("item_mail event %d for %s in %q after %v", ev.GetId(), ev.GetRef(), ev.GetFolder(), time.Since(start).Round(time.Millisecond))
			sw.track(ev.GetRef())
			if ev.GetItemId() != id || ev.GetAccount() != a.GetName() || ev.GetRef() == "" {
				t.Errorf("item_mail event: item %q, account %q, ref %q, want item %s on %s with a ref", ev.GetItemId(), ev.GetAccount(), ev.GetRef(), id, a.GetName())
			}
			if !ev.GetFromSelf() {
				t.Error("item_mail event: from_self is false for the account's own reply")
			}

			// 6. Exactly one item_mail event for the reply. Off Google the
			// reply has an inbox copy and a Sent copy, and both sync, but
			// they share a Message-ID. The count waits until every synced
			// copy has its new event, so a duplicate from the later copy
			// is in the log when it runs.
			delivered := sw.waitDelivered(t, reply.GetMessageId())
			waitReplyCopies(t, c, a.GetName(), hb.GetPosition(), token, replyFolders(t, c, a.GetName(), delivered, reply.GetSentRef()))
			if n := countReplyMail(t, c, id, hb.GetPosition(), token); n != 1 {
				t.Errorf("%d item_mail events for the reply, want 1", n)
			}

			// 7. The task's view shows the thread with both messages.
			waitLinkedThread(t, c, id, l.GetThreadKey(), token)

			// 8. Soft-delete the task.
			remove(t)
		})
	}
}

// liveTask creates an inbox task for the run, titled with token, and
// returns its ID and a function that soft-deletes it and checks the
// delete. A cleanup soft-deletes the task when the test ends before that
// function did.
func liveTask(t *testing.T, c *client.Client, token string) (string, func(*testing.T)) {
	t.Helper()
	resp, err := c.Items.CreateItem(t.Context(), connect.NewRequest(&chainmailv1.CreateItemRequest{
		Type: "task", Title: "chainmail live item " + token, Area: "personal",
		IdempotencyKey: liveKey(token, "item"),
	}))
	if err != nil {
		t.Fatalf("CreateItem: %v (%s)", err, hint(err))
	}
	it := resp.Msg.GetItem()
	if it.GetId() == "" || it.GetType() != "task" || it.GetParentId() != "" || resp.Msg.GetDuplicate() {
		t.Fatalf("CreateItem: id %q, type %q, parent %q, duplicate %v, want a new task in the inbox",
			it.GetId(), it.GetType(), it.GetParentId(), resp.Msg.GetDuplicate())
	}
	id := it.GetId()
	t.Logf("task %s", id)
	deleted := false
	// t.Context is cancelled before cleanups run, so the cleanup keeps its
	// values but drops the cancellation.
	base := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		if deleted {
			return
		}
		ctx, cancel := context.WithTimeout(base, cleanupTimeout)
		defer cancel()
		if _, err := c.Items.DeleteItem(ctx, connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: id})); err != nil {
			t.Errorf("cleanup: soft-deleting task %s: %v (%s); run: chainmail item delete %s", id, err, hint(err), id)
		}
	})
	remove := func(t *testing.T) {
		t.Helper()
		del, err := c.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: id}))
		if err != nil {
			t.Fatalf("DeleteItem %s: %v (%s)", id, err, hint(err))
		}
		deleted = true
		if ids := del.Msg.GetIds(); !slices.Equal(ids, []string{id}) {
			t.Errorf("DeleteItem %s deleted %q, want only the task", id, ids)
		}
		got, err := c.Items.GetItem(t.Context(), connect.NewRequest(&chainmailv1.GetItemRequest{Id: id}))
		if err != nil {
			t.Fatalf("GetItem %s after the delete: %v (%s)", id, err, hint(err))
		}
		if got.Msg.GetItem().GetDeletedAt() == nil {
			t.Errorf("task %s has no deleted_at after DeleteItem", id)
		}
	}
	return id, remove
}

// isReplyMail says e is the item_mail event of the test's reply: its
// subject holds the token and starts with "Re:". The original message can
// raise one too, when a copy of it, such as a Sent copy off Google, syncs
// only after the link.
func isReplyMail(e *chainmailv1.WatchEvent, token string) bool {
	return isReplyRow(e.GetMessage(), token)
}

// isReplyRow reports whether row is the test's reply: its subject holds token
// and starts with Re:.
func isReplyRow(row *chainmailv1.MessageRow, token string) bool {
	s := row.GetSubject().GetUntrusted()
	return strings.Contains(s, token) && strings.HasPrefix(strings.ToLower(s), "re:")
}

// countReplyMail reads the log after the event ID after, through the item
// filter, and counts the item_mail events of the test's reply for itemID.
func countReplyMail(t *testing.T, c *client.Client, itemID string, after int64, token string) int {
	t.Helper()
	n := 0
	cursor := after
	for range maxEventPages {
		resp, err := c.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{
			Cursor: &cursor,
			Filter: &chainmailv1.WatchFilter{Item: itemID, Kinds: []string{"item_mail"}},
			Limit:  200,
		}))
		if err != nil {
			t.Fatalf("ReadEvents after %d: %v (%s)", cursor, err, hint(err))
		}
		events := resp.Msg.GetEvents()
		for _, e := range events {
			if e.GetKind() == "item_mail" && e.GetItemId() == itemID && isReplyMail(e, token) {
				n++
			}
		}
		// A read examines a bounded number of rows, so an empty page can
		// still move the cursor. The log is read to its end only when the
		// cursor stops moving.
		next := resp.Msg.GetNextCursor()
		if next <= cursor {
			return n
		}
		cursor = next
	}
	t.Fatalf("ReadEvents: still more events after %d pages", maxEventPages)
	return 0
}

// replyFolders returns the folders of the reply's copies, the delivered
// ref and the Sent ref when there is one, that the daemon syncs for
// account. A copy in a folder it does not sync makes no event.
func replyFolders(t *testing.T, c *client.Client, account string, refs ...string) []string {
	t.Helper()
	resp, err := c.Watch.GetWatchStatus(t.Context(), connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
	if err != nil {
		t.Fatalf("GetWatchStatus: %v (%s)", err, hint(err))
	}
	synced := map[string]bool{}
	for _, f := range resp.Msg.GetFolders() {
		if f.GetAccount() == account {
			synced[f.GetFolder()] = true
		}
	}
	var folders []string
	for _, s := range refs {
		if s == "" {
			continue
		}
		r, err := ref.Parse(s)
		if err != nil {
			t.Fatalf("parsing the reply's ref %s: %v", s, err)
		}
		if synced[r.Folder] && !slices.Contains(folders, r.Folder) {
			folders = append(folders, r.Folder)
		}
	}
	t.Logf("reply copies in synced folders %q", folders)
	return folders
}

// waitReplyCopies reads account's new events after the event ID after
// until the test's reply has one in each of folders, then to the next
// heartbeat. The item_mail events a sync batch makes commit with its new
// events, so by that heartbeat the log holds every item_mail event the
// copies raised. It waits up to searchTimeout, since a polled folder may
// sync a copy only on its next poll.
func waitReplyCopies(t *testing.T, c *client.Client, account string, after int64, token string, folders []string) {
	t.Helper()
	within := waitBudget(t, searchTimeout)
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	stream, err := c.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{
		Since:  &after,
		Filter: &chainmailv1.WatchFilter{Accounts: []string{account}, Kinds: []string{"new"}},
	}))
	if err != nil {
		t.Fatalf("Subscribe: %v (%s)", err, hint(err))
	}
	defer func() { _ = stream.Close() }() // cancelled by then; nothing left to report
	missing := slices.Clone(folders)
	for {
		if !stream.Receive() {
			if ctx.Err() != nil {
				t.Fatalf("the reply's copies in %q had no new event within %v", missing, within)
			}
			t.Fatalf("waiting for the reply's copies: the stream ended: %v (%s)", stream.Err(), hint(stream.Err()))
		}
		msg := stream.Msg()
		if msg.GetHeartbeat() != nil {
			if len(missing) == 0 {
				return
			}
			continue
		}
		e := msg.GetEvent()
		if e.GetKind() != "new" || !isReplyMail(e, token) {
			continue
		}
		if i := slices.Index(missing, e.GetFolder()); i >= 0 {
			t.Logf("new event %d for the reply's copy %s in %q", e.GetId(), e.GetRef(), e.GetFolder())
			missing = slices.Delete(missing, i, i+1)
		}
	}
}

// waitLinkedThread waits up to searchTimeout for the task's view to show
// the thread with key in state ok, with its anchor found, at least two
// messages, and the reply (its subject holds token) as the latest message.
// Off Google the original's inbox and Sent copies already make two
// messages, so only the latest message proves the reply is in the view. It
// logs where the view came from.
func waitLinkedThread(t *testing.T, c *client.Client, itemID, key, token string) {
	t.Helper()
	within := waitBudget(t, searchTimeout)
	start := time.Now()
	// The deadline bounds the calls and pauses too, so a slow call cannot
	// carry the wait into the cleanup reserve.
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	last := "no GetItem answered"
	for n := 0; ; n++ {
		resp, err := c.Items.GetItem(ctx, connect.NewRequest(&chainmailv1.GetItemRequest{Id: itemID}))
		if err != nil && ctx.Err() == nil {
			t.Fatalf("GetItem %s: %v (%s)", itemID, err, hint(err))
		}
		if err == nil {
			last = fmt.Sprintf("%d threads, none with key %s", len(resp.Msg.GetThreads()), key)
			for _, th := range resp.Msg.GetThreads() {
				if th.GetThreadKey() != key {
					continue
				}
				last = fmt.Sprintf("state %s (%s), source %q, %d messages, anchor %q, latest is the reply %v", th.GetState(), th.GetReason(), th.GetSource(), th.GetMessages(), th.GetAnchorRef(), isReplyRow(th.GetLatest(), token))
				if th.GetState() == "ok" && th.GetMessages() >= 2 && th.GetAnchorRef() != "" && isReplyRow(th.GetLatest(), token) {
					t.Logf("thread %s after %v: %s, latest %s", key, time.Since(start).Round(time.Second), last, th.GetLatest().GetRef())
					return
				}
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("the task's view did not show thread %s with the reply within %v: %s", key, within, last)
		}
		if err := pause(ctx, n); err != nil && ctx.Err() == nil {
			t.Fatalf("waiting for thread %s: %v", key, err)
		}
	}
}
