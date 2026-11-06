//go:build integration

package daemon_test

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/dovecot"
)

// doveItemsConfig is one account on Dovecot that watches INBOX and Archive
// with IDLE, so a copy filed in either arrives within seconds. Without
// sync.folders the account also polls the rest of its default search scope.
// The cache stays off, so every link resolves on the mail server. The SMTP
// endpoint is never dialled.
const doveItemsConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = 1, security = "starttls" }
watch = ["INBOX", "Archive"]
`

// doveItemUser is the account's address and the Dovecot user.
const doveItemUser = "items@example.test"

// doveItemNote is the note of the first link. Like the titles, it must
// never appear in an audit row.
const doveItemNote = "Kickoff notes live here"

// doveItemTitles are the titles of the three tasks the steps create, in
// order.
var doveItemTitles = []string{"Follow the kickoff", "File the budget", "Settle the plans"}

// doveItemMail is a message from from with Message-ID <id@example.org>,
// dated date. replyTo, when set, is the id of the message it answers,
// named in In-Reply-To and References.
func doveItemMail(from, subject, id, replyTo string, date time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@example.org>\r\nDate: %s\r\n",
		from, doveItemUser, subject, id, date.Format(time.RFC1123Z))
	if replyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: <%s@example.org>\r\nReferences: <%s@example.org>\r\n", replyTo, replyTo)
	}
	b.WriteString("\r\nItem test mail.\r\n")
	return b.String()
}

// doveItemSeed holds the refs of the mail seeded before the daemon starts.
// The first sync baselines it, so none of it makes an event.
type doveItemSeed struct {
	// kickoff is a thread root in INBOX, and kickoffReply its reply in
	// Sent, a day newer.
	kickoff, kickoffReply string
	// budget and plans are one-message threads in INBOX.
	budget, plans string
}

// seedDoveItems appends the seed mail, dated back from now, plus one
// unrelated message in INBOX that no thread may count.
func seedDoveItems(t *testing.T, dove *dovecot.Server, now time.Time) doveItemSeed {
	t.Helper()
	day := 24 * time.Hour
	add := func(folder, raw string, date time.Time) string {
		t.Helper()
		uid, err := dove.Append(doveItemUser, folder, raw, date)
		if err != nil {
			t.Fatalf("seeding %s: %v", folder, err)
		}
		return doveRef(t, dove, doveItemUser, folder, uint32(uid))
	}
	const ann = "Ann <ann@example.org>"
	var s doveItemSeed
	s.kickoff = add("INBOX", doveItemMail(ann, "Kickoff", "itroot1", "", now.Add(-3*day)), now.Add(-3*day))
	s.kickoffReply = add("Sent", doveItemMail(doveItemUser, "Re: Kickoff", "itreply1", "itroot1", now.Add(-2*day)), now.Add(-2*day))
	add("INBOX", doveItemMail(ann, "Lunch", "itlunch", "", now.Add(-day)), now.Add(-day))
	s.budget = add("INBOX", doveItemMail(ann, "Budget", "itroot2", "", now.Add(-3*day)), now.Add(-3*day))
	s.plans = add("INBOX", doveItemMail(ann, "Plans", "itroot3", "", now.Add(-3*day)), now.Add(-3*day))
	return s
}

// TestItemsAgainstDovecot runs items end to end on Dovecot with the cache
// off (spec section 10.4). It checks in order: a link resolves on the
// server by the thread's root Message-ID, and finds its anchor again after
// the message is archived; a reply filed in INBOX and in Archive makes one
// item_mail event, and an item-filtered stream sees the item's own events
// and no mail events; a reply delivered while the daemon is stopped makes
// its item_mail event after a restart on the same state; and the audit
// rows name the items and hold no item text.
func TestItemsAgainstDovecot(t *testing.T) {
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	seed := seedDoveItems(t, dove, time.Now())
	cfg := fmt.Sprintf(doveItemsConfig, doveItemUser, dove.Endpoint.Port)
	opts := daemontest.Options{
		RootCAs:        dove.CA.Pool,
		Watch:          true,
		WatchHeartbeat: doveHeartbeat,
		Secrets:        map[string]string{"accounts/dove": dovecot.Password},
	}
	h := daemontest.Start(t, cfg, opts)
	for _, folder := range []string{"INBOX", "Archive"} {
		if st := waitDoveWatching(t, h, "dove", folder); st.GetMode() != "idle" || st.GetLastError() != "" {
			t.Fatalf("%s sync status = %+v, want mode idle and no error", folder, st)
		}
	}

	var ids []string
	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"server fallback", func(t *testing.T) { ids = append(ids, checkDoveItemFallback(t, h, seed)) }},
		{"one event for two copies", func(t *testing.T) { ids = append(ids, checkDoveItemCopies(t, h, dove, seed)) }},
		{"restart between link and delivery", func(t *testing.T) {
			var id string
			h, id = checkDoveItemRestart(t, h, cfg, opts, dove, seed)
			ids = append(ids, id)
		}},
		{"audit", func(t *testing.T) { checkDoveItemAudit(t, h, ids) }},
	}
	for _, s := range steps {
		// Each step leaves the state the next one starts from.
		if !t.Run(s.name, s.run) {
			return
		}
	}
}

// createDoveTask creates a work task in the inbox and returns its ID.
func createDoveTask(t *testing.T, h *daemontest.Harness, title string) string {
	t.Helper()
	resp, err := h.Client.Items.CreateItem(t.Context(), connect.NewRequest(&chainmailv1.CreateItemRequest{
		Type: "task", Title: title, Area: "work",
	}))
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	it := resp.Msg.GetItem()
	if it.GetId() == "" || it.GetType() != "task" || it.GetParentId() != "" ||
		it.GetEffectiveArea() != "work" || it.GetEffectivePriority() != "p2" {
		t.Fatalf("CreateItem = %v, want a work task in the inbox with priority p2", it)
	}
	return it.GetId()
}

// linkDoveThread links r's thread to itemID and returns the new link,
// failing the test unless the call made a link on the dove account.
func linkDoveThread(t *testing.T, h *daemontest.Harness, itemID, r string, anchor bool, note string) *chainmailv1.ThreadLink {
	t.Helper()
	resp, err := h.Client.Items.LinkThread(t.Context(), connect.NewRequest(&chainmailv1.LinkThreadRequest{
		ItemId: itemID, Ref: r, Anchor: anchor, Note: note,
	}))
	if err != nil {
		t.Fatalf("LinkThread %s: %v", r, err)
	}
	if !resp.Msg.GetCreated() {
		t.Errorf("LinkThread %s: created false, want a new link", r)
	}
	l := resp.Msg.GetLink()
	if l.GetAccount() != "dove" || l.GetThreadKey() == "" {
		t.Fatalf("LinkThread %s: link %v, want a thread key on dove", r, l)
	}
	return l
}

// soleDoveThread reads itemID's view and returns its one linked thread.
func soleDoveThread(t *testing.T, h *daemontest.Harness, itemID string) *chainmailv1.ThreadLink {
	t.Helper()
	resp, err := h.Client.Items.GetItem(t.Context(), connect.NewRequest(&chainmailv1.GetItemRequest{Id: itemID}))
	if err != nil {
		t.Fatalf("GetItem %s: %v", itemID, err)
	}
	if got := resp.Msg.GetItem().GetId(); got != itemID {
		t.Fatalf("GetItem %s returned item %q", itemID, got)
	}
	if resp.Msg.GetThreadsTotal() != 1 || len(resp.Msg.GetThreads()) != 1 {
		t.Fatalf("GetItem %s: %d of %d threads, want 1 of 1", itemID, len(resp.Msg.GetThreads()), resp.Msg.GetThreadsTotal())
	}
	return resp.Msg.GetThreads()[0]
}

// checkDoveItemFallback links the Kickoff thread, anchored on its root,
// with the cache off. The link resolves on the server: the root in INBOX
// and the reply in Sent, found by the root's Message-ID, and not the
// unrelated Lunch message. Then the root is archived, and the item view
// finds the anchor at its new ref in Archive (done when item 4). It
// returns the task's ID.
//
// The archived root arrives in the watched Archive folder as a new
// message on the linked thread. IMAP keeps its INTERNALDATE across the
// move, three days before the link, so it is old mail and raises no
// item_mail event for the task (done when item 4).
func checkDoveItemFallback(t *testing.T, h *daemontest.Harness, seed doveItemSeed) string {
	id := createDoveTask(t, h, doveItemTitles[0])
	l := linkDoveThread(t, h, id, seed.kickoff, true, doveItemNote)
	if want := mailstore.ThreadKey("itroot1@example.org"); l.GetThreadKey() != want {
		t.Errorf("thread key = %q, want %q, the key of the root Message-ID", l.GetThreadKey(), want)
	}
	if l.GetState() != "ok" || l.GetSource() != "server" || l.GetMessages() != 2 {
		t.Errorf("new link: state %q, source %q, %d messages, want ok from the server with 2", l.GetState(), l.GetSource(), l.GetMessages())
	}
	if l.GetAnchorRef() != seed.kickoff || l.GetLatest().GetRef() != seed.kickoffReply {
		t.Errorf("new link: anchor %q, latest %q, want anchor %s and latest %s", l.GetAnchorRef(), l.GetLatest().GetRef(), seed.kickoff, seed.kickoffReply)
	}
	if got := l.GetNote().GetUntrusted(); got != doveItemNote {
		t.Errorf("new link note = %q, want %q", got, doveItemNote)
	}

	arrivals := subscribeDove(t, h, &chainmailv1.SubscribeRequest{
		Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}, Kinds: []string{"new"}},
	})
	start := arrivals.heartbeat(doveReadyWithin).GetPosition()
	arch, err := h.Client.Organize.Archive(t.Context(), connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: []string{seed.kickoff}}))
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	outs := arch.Msg.GetOutcomes()
	if len(outs) != 1 || outs[0].GetError() != nil {
		t.Fatalf("Archive outcomes = %v, want one success", outs)
	}
	// Dovecot has MOVE and UIDPLUS, so the outcome names the new ref.
	archived := outs[0].GetNewRef()
	if archived == "" || parseRef(t, archived).Folder != "Archive" {
		t.Fatalf("Archive new ref = %q, want a ref in Archive", archived)
	}
	// The item_mail events a batch makes commit with its new events, so by
	// the heartbeat after the archived root's new event the log holds any
	// item_mail the move raised.
	arrivals.next("new", doveReadyWithin, func(ev *chainmailv1.WatchEvent) bool { return ev.GetRef() == archived })
	arrivals.heartbeat(doveReadyWithin)
	arrivals.close()
	moved := readEvents(t, h, &chainmailv1.ReadEventsRequest{
		Cursor: &start, Limit: 200, Filter: &chainmailv1.WatchFilter{Item: id, Kinds: []string{"item_mail"}},
	})
	for _, ev := range moved.GetEvents() {
		t.Errorf("the archive of old mail raised item_mail event %d for %s, want none", ev.GetId(), ev.GetRef())
	}

	th := soleDoveThread(t, h, id)
	if th.GetThreadKey() != l.GetThreadKey() || th.GetState() != "ok" || th.GetSource() != "server" || th.GetMessages() != 2 {
		t.Errorf("thread after the archive: key %q, state %q, source %q, %d messages, want key %q, ok from the server with 2",
			th.GetThreadKey(), th.GetState(), th.GetSource(), th.GetMessages(), l.GetThreadKey())
	}
	if th.GetAnchorRef() != archived {
		t.Errorf("anchor after the archive = %q, want the archived ref %s", th.GetAnchorRef(), archived)
	}
	if th.GetLatest().GetRef() != seed.kickoffReply || th.GetLatest().GetSubject().GetUntrusted() != "Re: Kickoff" {
		t.Errorf("latest after the archive = %s %q, want %s", th.GetLatest().GetRef(), th.GetLatest().GetSubject().GetUntrusted(), seed.kickoffReply)
	}
	return id
}

// checkDoveItemCopies links the Budget thread, then another IMAP client
// files one reply from Ann in INBOX and a copy of it, the same Message-ID,
// in Archive. Both copies make new events, and the item gets exactly one
// item_mail event, for one of them, with from_self false (spec section
// 10.4). A stream with the item filter and no kinds, started before the
// item existed, gets the item's created and linked events and that one
// item_mail, and never a new, flags or expunged event (spec section 14.4).
// It returns the task's ID.
func checkDoveItemCopies(t *testing.T, h *daemontest.Harness, dove *dovecot.Server, seed doveItemSeed) string {
	all := subscribeDove(t, h, &chainmailv1.SubscribeRequest{
		Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}, Kinds: []string{"new", "item_mail"}},
	})
	start := all.heartbeat(doveReadyWithin).GetPosition()
	id := createDoveTask(t, h, doveItemTitles[1])
	linkDoveThread(t, h, id, seed.budget, false, "")
	scoped := subscribeDove(t, h, &chainmailv1.SubscribeRequest{
		Since:  &start,
		Filter: &chainmailv1.WatchFilter{Item: id},
	})

	raw := doveItemMail("Ann <ann@example.org>", "Re: Budget", "itreply2", "itroot2", time.Now())
	copies := map[string]bool{} // ref -> its new event has passed
	for _, folder := range []string{"INBOX", "Archive"} {
		uid, err := dove.Append(doveItemUser, folder, raw, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		copies[doveRef(t, dove, doveItemUser, folder, uint32(uid))] = false
	}

	// Read until both copies' new events have passed, then to the next
	// heartbeat. The item_mail events a batch makes commit with its new
	// events, so by that heartbeat every one of them has been sent.
	var mails []*chainmailv1.WatchEvent
	seen := 0
	deadline := time.Now().Add(doveReadyWithin)
	for {
		it := all.item(time.Until(deadline), "the new events of both copies and a heartbeat after them")
		if it.GetHeartbeat() != nil {
			if seen == len(copies) {
				break
			}
			continue
		}
		ev := it.GetEvent()
		switch ev.GetKind() {
		case "new":
			if done, ok := copies[ev.GetRef()]; ok && !done {
				copies[ev.GetRef()] = true
				seen++
			}
		case "item_mail":
			if ev.GetItemId() == id {
				mails = append(mails, ev)
			}
		}
	}
	all.close()
	if len(mails) != 1 {
		t.Fatalf("%d item_mail events for the item from two copies of one reply, want 1: %v", len(mails), mails)
	}
	m := mails[0]
	if _, ok := copies[m.GetRef()]; !ok || m.GetAccount() != "dove" || m.GetFolder() != parseRef(t, m.GetRef()).Folder {
		t.Errorf("item_mail event %s in %s/%s, want one of the copies %v", m.GetRef(), m.GetAccount(), m.GetFolder(), copies)
	}
	if m.GetFromSelf() || m.GetMessage().GetSubject().GetUntrusted() != "Re: Budget" || m.GetMessage().GetRef() != m.GetRef() {
		t.Errorf("item_mail event: from_self %v, subject %q, message ref %q, want Ann's Re: Budget at the event's ref",
			m.GetFromSelf(), m.GetMessage().GetSubject().GetUntrusted(), m.GetMessage().GetRef())
	}

	var ops []string
	// Heartbeats keep arriving, so the wait needs one deadline for the loop.
	scopedDeadline := time.Now().Add(doveReadyWithin)
	for got := false; !got; {
		ev := scoped.item(time.Until(scopedDeadline), "the item's item_mail event").GetEvent()
		switch ev.GetKind() {
		case "item":
			if ev.GetItem().GetItemId() != id || ev.GetAccount() != "" || ev.GetItem().GetType() != "task" {
				t.Errorf("item event %d = %v, want one for task %s with no account", ev.GetId(), ev.GetItem(), id)
			}
			ops = append(ops, ev.GetItem().GetOp())
		case "item_mail":
			if ev.GetId() != m.GetId() {
				t.Errorf("the item filter passed item_mail event %d, want %d", ev.GetId(), m.GetId())
			}
			got = true
		case "new", "flags", "expunged":
			t.Errorf("the item filter passed a %s event: %d %s", ev.GetKind(), ev.GetId(), ev.GetRef())
		}
	}
	if !slices.Equal(ops, []string{"created", "linked"}) {
		t.Errorf("item events before the item_mail = %q, want [created linked]", ops)
	}
	scoped.quiet("item_mail", 4)
	scoped.close()
	return id
}

// checkDoveItemRestart links the Plans thread and opens a group on the
// item's item_mail events. With the group caught up the daemon stops, the
// account's own reply arrives, and the daemon restarts on the same state.
// The group resumes with exactly one item_mail event for the reply, with
// from_self set, and the item view counts it (spec section 10.4). It
// returns the restarted harness and the task's ID.
func checkDoveItemRestart(t *testing.T, h *daemontest.Harness, cfg string, opts daemontest.Options, dove *dovecot.Server, seed doveItemSeed) (*daemontest.Harness, string) {
	const group = "items"
	id := createDoveTask(t, h, doveItemTitles[2])
	linkDoveThread(t, h, id, seed.plans, false, "")
	filter := &chainmailv1.WatchFilter{Item: id, Kinds: []string{"item_mail"}}
	w := subscribeDove(t, h, &chainmailv1.SubscribeRequest{Group: group, Filter: filter})
	ackDove(t, h, group, w.heartbeat(doveReadyWithin).GetPosition())
	w.close()

	if err := h.Stop(); err != nil {
		t.Fatalf("stopping the daemon: %v", err)
	}
	uid, err := dove.Append(doveItemUser, "INBOX", doveItemMail(doveItemUser, "Re: Plans", "itreply3", "itroot3", time.Now()), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := doveRef(t, dove, doveItemUser, "INBOX", uint32(uid))
	restarted := opts
	restarted.Reuse = h
	h2 := daemontest.Start(t, cfg, restarted)

	w2 := subscribeDove(t, h2, &chainmailv1.SubscribeRequest{Group: group, Filter: filter})
	ev := w2.next("item_mail", doveReadyWithin, nil)
	if ev.GetItemId() != id || ev.GetRef() != want || ev.GetAccount() != "dove" || ev.GetFolder() != "INBOX" {
		t.Errorf("item_mail after the restart = item %q, %s in %s/%s, want item %s, %s in dove/INBOX",
			ev.GetItemId(), ev.GetRef(), ev.GetAccount(), ev.GetFolder(), id, want)
	}
	if !ev.GetFromSelf() || ev.GetMessage().GetSubject().GetUntrusted() != "Re: Plans" {
		t.Errorf("item_mail after the restart: from_self %v, subject %q, want the account's own Re: Plans", ev.GetFromSelf(), ev.GetMessage().GetSubject().GetUntrusted())
	}
	// The catch-up commits the new event and its item_mail event together,
	// so a duplicate would already be in the log.
	w2.quiet("item_mail", 4)
	w2.close()

	th := soleDoveThread(t, h2, id)
	if th.GetState() != "ok" || th.GetSource() != "server" || th.GetMessages() != 2 || th.GetLatest().GetRef() != want {
		t.Errorf("thread after the restart: state %q, source %q, %d messages, latest %q, want ok from the server with 2, latest %s",
			th.GetState(), th.GetSource(), th.GetMessages(), th.GetLatest().GetRef(), want)
	}
	return h2, id
}

// checkDoveItemAudit reads the audit log both daemons wrote. Every item
// call the test made has a row with client kind test that names one of
// its items, link rows name the account and the thread key, and no row
// holds a title or a note (spec section 9).
func checkDoveItemAudit(t *testing.T, h *daemontest.Harness, ids []string) {
	text := append([]string{doveItemNote}, doveItemTitles...)
	counts := map[string]int{}
	for _, row := range h.AuditRows(t) {
		line := fmt.Sprintf("%+v", row)
		for _, s := range text {
			if strings.Contains(line, s) {
				t.Errorf("audit row %s holds item text %q", line, s)
			}
		}
		// Rows of the daemon's own work, such as a background probe, are
		// not the test's calls.
		if !strings.HasPrefix(row.Action, "item.") || row.Client == "daemon" {
			continue
		}
		counts[row.Action]++
		if row.Client != "test" || row.Result != "ok" {
			t.Errorf("audit row %s, want client test and result ok", line)
		}
		if !slices.ContainsFunc(ids, func(id string) bool { return strings.Contains(row.Targets, "item="+id) }) {
			t.Errorf("audit row %s names none of the test's items %v", line, ids)
		}
		wantAccount := ""
		if row.Action == "item.link" {
			wantAccount = "dove"
			if !strings.Contains(row.Targets, "thread=") {
				t.Errorf("item.link row %s names no thread", line)
			}
		}
		if row.Account != wantAccount {
			t.Errorf("audit row %s has account %q, want %q", line, row.Account, wantAccount)
		}
	}
	if want := map[string]int{"item.create": 3, "item.link": 3, "item.get": 2}; !maps.Equal(counts, want) {
		t.Errorf("item audit rows by action = %v, want %v", counts, want)
	}
}
