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
)

// itemMailConfig watches INBOX and Sent, so a reply and its Sent copy
// arrive within a pass each.
const itemMailConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
watch = ["INBOX", "Sent"]
`

// TestItemMailReachesItemWatchers links a thread on a watching daemon,
// delivers a self-sent reply to INBOX and its copy to Sent, and unrelated
// mail, and checks that a stream with the item filter gets the item_mail
// event, that the log holds exactly one for the reply, with from_self set
// although the From address differs in case, and that a reader with no
// kinds sees none of it.
func TestItemMailReachesItemWatchers(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	root := fk.Add(fake.Message{Folder: "INBOX", Date: watchRunDate, From: itemAnn, Subject: "plan", MessageID: "root@example.com"})
	h := daemontest.Start(t, itemMailConfig, daemontest.Options{
		Watch:    true,
		Secrets:  map[string]string{"accounts/host": "hunter2"},
		NewStore: func(config.ResolvedAccount) mailstore.Store { return fk },
	})
	ctx, cancel := context.WithTimeout(t.Context(), watchRunWait)
	defer cancel()
	status := subscribeFromStart(ctx, t, h)
	for watching := map[string]bool{}; !watching["INBOX"] || !watching["Sent"]; {
		ev := awaitWatchEvent(t, status, func(ev *chainmailv1.WatchEvent) bool {
			return ev.GetKind() == "status" && ev.GetState() == "watching"
		})
		watching[ev.GetFolder()] = true
	}
	id := itemTask(t, h, "Ship the plan")
	key := itemLink(t, h, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: root.String()}).GetLink().GetThreadKey()
	mail, err := h.Client.Watch.Subscribe(ctx, connect.NewRequest(&chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Item: id, Kinds: []string{"item_mail"}}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mail.Close() })
	untilHeartbeat(t, mail)

	// The reply arrives after the link was made, by the daemon's clock, so
	// it is news for the item; mail the server had before the link is not.
	reply := fake.Message{
		Folder: "INBOX", Date: watchRunDate.Add(time.Hour), InternalDate: time.Now().UTC(),
		From: []mailstore.Address{{Email: "Me@Example.com"}}, Subject: "re: plan",
		MessageID: "reply@example.com", InReplyTo: []string{"root@example.com"}, References: []string{"root@example.com"},
	}
	fk.Add(reply)
	reply.Folder = "Sent"
	fk.Add(reply)
	fk.Add(fake.Message{Folder: "INBOX", Date: watchRunDate.Add(time.Hour), From: itemAnn, Subject: "lunch", MessageID: "lunch@example.com"})

	ev := awaitWatchEvent(t, mail, func(ev *chainmailv1.WatchEvent) bool { return ev.GetKind() == "item_mail" })
	if ev.GetItemId() != id || !ev.GetFromSelf() || ev.GetAccount() != "host" || ev.GetRef() == "" ||
		(ev.GetFolder() != "INBOX" && ev.GetFolder() != "Sent") ||
		ev.GetMessage().GetSubject().GetUntrusted() != "re: plan" || ev.GetMessage().GetThreadId() != key {
		t.Errorf("item_mail = %v, want the reply for %s, from self, on thread %s", ev, id, key)
	}

	deadline := time.Now().Add(watchRunWait)
	for {
		news := 0
		for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Limit: 200, Filter: &chainmailv1.WatchFilter{Kinds: []string{"new"}}}).GetEvents() {
			if e.GetKind() == "new" {
				news++
			}
		}
		if news == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d new events after %v, want the reply, its Sent copy and the lunch mail", news, watchRunWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mails, changes := 0, 0
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Limit: 200, Filter: &chainmailv1.WatchFilter{Item: id}}).GetEvents() {
		switch e.GetKind() {
		case "item_mail":
			mails++
		case "item":
			changes++
		}
	}
	if mails != 1 || changes != 2 {
		t.Errorf("for the item: %d item_mail and %d item events, want 1 (deduplicated by Message-ID) and 2 (created, linked)", mails, changes)
	}
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Limit: 200}).GetEvents() {
		if e.GetKind() == "item" || e.GetKind() == "item_mail" {
			t.Errorf("a reader with no kinds got %v", e)
		}
	}
}
