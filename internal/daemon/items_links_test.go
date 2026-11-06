package daemon_test

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// itemAnn is a sender that is not the account.
var itemAnn = []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}}

// itemLink links a thread and fails the test on an error.
func itemLink(t *testing.T, h *daemontest.Harness, req *chainmailv1.LinkThreadRequest) *chainmailv1.LinkThreadResponse {
	t.Helper()
	resp, err := h.Client.Items.LinkThread(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("LinkThread: %v", err)
	}
	return resp.Msg
}

// TestGetItemResolvesLinksFromTheServer links a thread of two messages on
// a standard and on a Gmail account with no cache, archives the anchor,
// and checks that the view follows it on the server, that linking the
// same thread again through the reply changes nothing, and that a thread
// whose messages all went to Trash is reported empty and kept.
func TestGetItemResolvesLinksFromTheServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, account, folder, replyFolder string
		labels                             []string
	}{
		{name: "standard", account: "host", folder: "INBOX", replyFolder: "Sent"},
		{name: "gmail", account: "gmail", folder: gmailAllMail, replyFolder: gmailAllMail, labels: []string{`\Inbox`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := startFakeMail(t)
			st := m.host
			if tt.account == "gmail" {
				st = m.gmail
			}
			root := st.Add(fake.Message{Folder: tt.folder, Labels: tt.labels, Date: daysAgo(3), From: itemAnn, Subject: "plan", MessageID: "root@example.com"})
			reply := st.Add(fake.Message{
				Folder: tt.replyFolder, Date: daysAgo(1), From: itemAnn, Subject: "re: plan", MessageID: "reply@example.com",
				InReplyTo: []string{"root@example.com"}, References: []string{"root@example.com"},
			})
			id := itemTask(t, m.Harness, "Ship the plan")
			note := render.CleanHeader("kick\toff")

			linked := itemLink(t, m.Harness, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: root.String(), Anchor: true, Note: "kick\toff"})
			l := linked.GetLink()
			if !linked.GetCreated() || linked.GetRevision() <= 0 || l.GetAccount() != tt.account || l.GetThreadKey() == "" ||
				l.GetState() != "ok" || l.GetSource() != "server" || l.GetMessages() != 2 || l.GetAnchorRef() != root.String() ||
				l.GetLatest().GetRef() != reply.String() || l.GetLatest().GetThreadCount() != 2 || l.GetNote().GetUntrusted() != note ||
				l.GetCreatedBy() != itemActor() || l.GetCreatedAt() == nil {
				t.Fatalf("link = %v", linked)
			}

			archived, err := m.Client.Organize.Archive(t.Context(), connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: []string{root.String()}}))
			if err != nil {
				t.Fatal(err)
			}
			moved := archived.Msg.GetOutcomes()[0].GetNewRef()
			if moved == "" {
				moved = root.String() // Gmail keeps the ref
			}
			if tt.account == "host" && moved == root.String() {
				t.Fatalf("archive outcome %v, want the message moved to Archive", archived.Msg)
			}
			view := itemGet(t, m.Harness, id)
			th := view.GetThreads()
			if view.GetThreadsTotal() != 1 || len(th) != 1 || th[0].GetState() != "ok" || th[0].GetSource() != "server" ||
				th[0].GetMessages() != 2 || th[0].GetAnchorRef() != moved || th[0].GetLatest().GetRef() != reply.String() ||
				th[0].GetThreadKey() != l.GetThreadKey() {
				t.Errorf("view after the archive = %v, want the anchor at %s", th, moved)
			}

			again := itemLink(t, m.Harness, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: reply.String()})
			if again.GetCreated() || again.GetRevision() != linked.GetRevision() || again.GetLink().GetAnchorRef() != moved || again.GetLink().GetNote().GetUntrusted() != note {
				t.Errorf("relink = %v, want the same link unchanged at revision %d", again, linked.GetRevision())
			}

			if _, err := m.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: []string{moved, reply.String()}})); err != nil {
				t.Fatal(err)
			}
			th = itemGet(t, m.Harness, id).GetThreads()
			if len(th) != 1 || th[0].GetState() != "empty" || th[0].GetMessages() != 0 || th[0].GetLatest() != nil || th[0].GetAnchorRef() != "" || th[0].GetSource() != "server" {
				t.Errorf("view after the trash = %v, want the link kept and empty", th)
			}
		})
	}
}

// TestGetItemReportsAnUnreadableThreadUnavailable links a thread of two
// messages on an account with no cache, makes the server send one or both
// of them in a form chainmail cannot read, and checks that the link is
// unavailable with MALFORMED_RESPONSE rather than ok with a short count or
// empty.
func TestGetItemReportsAnUnreadableThreadUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		unreadable func(root, reply ref.Ref) []ref.Ref
	}{
		{name: "partly", unreadable: func(_, reply ref.Ref) []ref.Ref { return []ref.Ref{reply} }},
		{name: "wholly", unreadable: func(root, reply ref.Ref) []ref.Ref { return []ref.Ref{root, reply} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := startFakeMail(t)
			root := m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(3), From: itemAnn, Subject: "plan", MessageID: "root@example.com"})
			reply := m.host.Add(fake.Message{
				Folder: "Sent", Date: daysAgo(1), From: itemAnn, Subject: "re: plan", MessageID: "reply@example.com",
				InReplyTo: []string{"root@example.com"}, References: []string{"root@example.com"},
			})
			id := itemTask(t, m.Harness, "Ship the plan")
			key := itemLink(t, m.Harness, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: root.String(), Note: "plan"}).GetLink().GetThreadKey()
			for _, r := range tt.unreadable(root, reply) {
				m.host.Malform(r.Folder, r.UID, fake.BadEnvelope)
			}

			th := itemGet(t, m.Harness, id).GetThreads()
			if len(th) != 1 {
				t.Fatalf("threads = %v, want the one link", th)
			}
			if g := th[0]; g.GetThreadKey() != key || g.GetState() != "unavailable" || g.GetReason() != api.ReasonMalformedResponse ||
				g.GetMessages() != 0 || g.GetLatest() != nil || g.GetAnchorRef() != "" || g.GetNote().GetUntrusted() != "plan" {
				t.Errorf("link = %v, want it unavailable with %s", g, api.ReasonMalformedResponse)
			}
		})
	}
}

// TestGetItemReportsAnAccountGoneFromConfig links a thread on each of two
// accounts, restarts the daemon without one of them, and checks that its
// link is reported unavailable with NOT_FOUND and listed last, its key and
// note kept.
func TestGetItemReportsAnAccountGoneFromConfig(t *testing.T) {
	t.Parallel()
	host, gm := fake.New("host", false), fake.New("gmail", true)
	opts := daemontest.Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			if a.Name == "gmail" {
				return gm
			}
			return host
		},
		Now: func() time.Time { return testNow },
	}
	h := daemontest.Start(t, fakeMailConfig, opts)
	invoice := host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(2), From: itemAnn, Subject: "invoice", MessageID: "invoice@example.com"})
	receipt := gm.Add(fake.Message{Folder: gmailAllMail, Labels: []string{`\Inbox`}, Date: daysAgo(1), From: itemAnn, Subject: "receipt", MessageID: "receipt@example.com"})
	id := itemTask(t, h, "File the paperwork")
	gmailKey := itemLink(t, h, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: receipt.String(), Note: "receipt"}).GetLink().GetThreadKey()
	itemLink(t, h, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: invoice.String()})

	opts.Reuse = h
	again := daemontest.Start(t, hostingerConfig, opts)
	view := itemGet(t, again, id)
	th := view.GetThreads()
	if view.GetThreadsTotal() != 2 || len(th) != 2 {
		t.Fatalf("threads = %v, want both links", th)
	}
	if th[0].GetAccount() != "host" || th[0].GetState() != "ok" || th[0].GetLatest().GetRef() != invoice.String() {
		t.Errorf("first = %v, want the host link, ok", th[0])
	}
	if g := th[1]; g.GetAccount() != "gmail" || g.GetThreadKey() != gmailKey || g.GetState() != "unavailable" || g.GetReason() != api.ReasonNotFound ||
		g.GetSource() != "" || g.GetMessages() != 0 || g.GetLatest() != nil || g.GetNote().GetUntrusted() != "receipt" {
		t.Errorf("last = %v, want the gmail link unavailable with NOT_FOUND", g)
	}
}

// TestLinkThreadRefusals checks each refusal's code, reason and hint, and
// the audit targets gathered before it.
func TestLinkThreadRefusals(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	ok := m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), From: itemAnn, Subject: "fine", MessageID: "fine@example.com"})
	noKey := m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), From: itemAnn, Subject: "no ids"})
	noID := m.gmail.Add(fake.Message{Folder: gmailAllMail, Date: daysAgo(1), From: itemAnn, Subject: "no Message-ID"})
	stale := m.host.Add(fake.Message{Folder: "Archive", Date: daysAgo(1), From: itemAnn, Subject: "stale", MessageID: "stale@example.com"})
	m.host.SetUIDValidity("Archive", 9)
	id := itemTask(t, m.Harness, "Triage")
	deleted := itemTask(t, m.Harness, "Gone")
	if _, err := m.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: deleted})); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		req     *chainmailv1.LinkThreadRequest
		code    connect.Code
		reason  string
		hint    string
		account string
		targets []string
	}{
		{name: "malformed item", req: &chainmailv1.LinkThreadRequest{ItemId: "Triage", Ref: ok.String()}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "item IDs start with it_; run: chainmail item list", targets: []string{"item=(invalid)"}},
		{name: "unknown item", req: &chainmailv1.LinkThreadRequest{ItemId: "it_nope", Ref: ok.String()}, code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list", targets: []string{"item=it_nope"}},
		{name: "deleted item", req: &chainmailv1.LinkThreadRequest{ItemId: deleted, Ref: ok.String()}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "restore it first: chainmail item restore <id>", targets: []string{"item=" + deleted}},
		{name: "malformed ref", req: &chainmailv1.LinkThreadRequest{ItemId: id, Ref: "INBOX/1"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "refs come from search results; pass them unchanged", targets: []string{"item=" + id}},
		{name: "unknown account", req: &chainmailv1.LinkThreadRequest{ItemId: id, Ref: ref.Ref{Account: "nope", Folder: "INBOX", UIDValidity: 1, UID: 1}.String()}, code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail accounts list", targets: []string{"item=" + id}},
		{name: "stale ref", req: &chainmailv1.LinkThreadRequest{ItemId: id, Ref: stale.String()}, code: connect.CodeNotFound, reason: api.ReasonStaleRef, hint: "search again", account: "host", targets: []string{"item=" + id, "account=host"}},
		{name: "no thread key", req: &chainmailv1.LinkThreadRequest{ItemId: id, Ref: noKey.String()}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, account: "host", targets: []string{"item=" + id, "account=host", "thread="}},
		{name: "anchor without a Message-ID", req: &chainmailv1.LinkThreadRequest{ItemId: id, Ref: noID.String(), Anchor: true}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, account: "gmail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.Client.Items.LinkThread(t.Context(), connect.NewRequest(tt.req))
			wantItemErr(t, tt.name, err, tt.code, tt.reason, tt.hint)
		})
	}
	rows := itemAudit(t, m.Harness, "item.link")
	if len(rows) != len(tests) {
		t.Fatalf("item.link rows = %+v, want one per call", rows)
	}
	for i, tt := range tests {
		r := rows[i]
		if r.Result != "error" || r.Reason != tt.reason || r.Account != tt.account {
			t.Errorf("%s: audit = %+v, want %s on account %q", tt.name, r, tt.reason, tt.account)
		}
		if tt.targets != nil && r.Targets != itemTargets(t, tt.targets...) {
			t.Errorf("%s: targets %s, want %s", tt.name, r.Targets, itemTargets(t, tt.targets...))
		}
	}
	// The Gmail thread key is the fake's, so only the targets before it are fixed.
	prefix := strings.TrimSuffix(itemTargets(t, "item="+id, "account=gmail"), "]") + `,"thread=`
	if last := rows[len(rows)-1]; !strings.HasPrefix(last.Targets, prefix) {
		t.Errorf("anchor refusal targets %s, want them to start %s", last.Targets, prefix)
	}
}

// TestUnlinkThread checks an unlink, the view without the link, a second
// unlink of the same link, malformed arguments, and the audit rows.
func TestUnlinkThread(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), From: itemAnn, Subject: "plan", MessageID: "plan@example.com"})
	id := itemTask(t, m.Harness, "Plan")
	linked := itemLink(t, m.Harness, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: r.String()})
	key := linked.GetLink().GetThreadKey()
	unlink := func(item, account, threadKey string) (*chainmailv1.UnlinkThreadResponse, error) {
		resp, err := m.Client.Items.UnlinkThread(t.Context(), connect.NewRequest(&chainmailv1.UnlinkThreadRequest{ItemId: item, Account: account, ThreadKey: threadKey}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	resp, err := unlink(id, "host", key)
	if err != nil || resp.GetRevision() <= linked.GetRevision() {
		t.Fatalf("unlink = %v, %v; want a later revision than %d", resp, err, linked.GetRevision())
	}
	if view := itemGet(t, m.Harness, id); view.GetThreadsTotal() != 0 || len(view.GetThreads()) != 0 {
		t.Errorf("view after the unlink = %v, want no threads", view)
	}
	_, err = unlink(id, "host", key)
	wantItemErr(t, "unlink again", err, connect.CodeNotFound, api.ReasonNotFound, "run: chainmail item list")
	_, err = unlink(id, "host", "XYZ")
	wantItemErr(t, "bad key", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "thread keys are lowercase hex, as chainmail item show prints them")
	_, err = unlink(id, "Bad Name", key)
	wantItemErr(t, "bad account", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")

	want := []daemontest.AuditRow{
		{Client: "test", Account: "host", Action: "item.unlink", Targets: itemTargets(t, "item="+id, "account=host", "thread="+key), Result: "ok"},
		{Client: "test", Account: "host", Action: "item.unlink", Targets: itemTargets(t, "item="+id, "account=host", "thread="+key), Result: "error", Reason: api.ReasonNotFound},
		{Client: "test", Account: "host", Action: "item.unlink", Targets: itemTargets(t, "item="+id, "account=host", "thread=(invalid)"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.unlink", Targets: itemTargets(t, "item="+id, "account=(invalid)", "thread="+key), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	got := itemAudit(t, m.Harness, "item.unlink")
	if len(got) != len(want) {
		t.Fatalf("audit = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unlink %d audit = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestItemLinkAuditHoldsNoItemText links with itemMarker as the note on an
// item titled with it, reads the view and unlinks, and checks the three
// audit rows exactly and that no row repeats the marker.
func TestItemLinkAuditHoldsNoItemText(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), From: itemAnn, Subject: "plan", MessageID: "plan@example.com"})
	id := itemCreate(t, m.Harness, &chainmailv1.CreateItemRequest{Type: "task", Title: itemMarker, Area: "work"}).GetItem().GetId()
	link := itemLink(t, m.Harness, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: r.String(), Note: itemMarker}).GetLink()
	if link.GetNote().GetUntrusted() != itemMarker {
		t.Fatalf("note = %q, want the marker back, wrapped", link.GetNote().GetUntrusted())
	}
	itemGet(t, m.Harness, id)
	if _, err := m.Client.Items.UnlinkThread(t.Context(), connect.NewRequest(&chainmailv1.UnlinkThreadRequest{ItemId: id, Account: "host", ThreadKey: link.GetThreadKey()})); err != nil {
		t.Fatal(err)
	}
	thread := itemTargets(t, "item="+id, "account=host", "thread="+link.GetThreadKey())
	want := []daemontest.AuditRow{
		{Client: "test", Account: "host", Action: "item.link", Targets: thread, Result: "ok"},
		{Client: "test", Action: "item.get", Targets: itemTargets(t, "item="+id, "type=task"), Result: "ok"},
		{Client: "test", Account: "host", Action: "item.unlink", Targets: thread, Result: "ok"},
	}
	got := itemAudit(t, m.Harness, "item.link", "item.get", "item.unlink")
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("audit = %+v, want %+v", got, want)
	}
	for _, row := range m.AuditRows(t) {
		if strings.Contains(row.Targets, itemMarker) {
			t.Errorf("audit row %+v repeats item text", row)
		}
	}
}
