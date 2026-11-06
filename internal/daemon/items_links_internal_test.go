package daemon

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// linkServer is a daemon that is never served, over routeConfig's fake
// stores, logging at Debug into the returned buffer.
func linkServer(t *testing.T) (*Server, map[string]*fake.Store, *syncBuffer) {
	t.Helper()
	fakes := map[string]*fake.Store{"host": fake.New("host", false), "gmail": fake.New("gmail", true)}
	for _, fk := range fakes {
		fk.SetNow(routeClock{}.Now)
	}
	logs := &syncBuffer{}
	s, _ := newTestServerOptions(t, routeConfig, Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store { return fakes[a.Name] },
		Now:      routeClock{}.Now,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return s, fakes, logs
}

// TestUnavailableLinkLogs checks how resolving a link that cannot be
// resolved is logged: a link to an account no longer configured is a
// routine state, logged at Debug, and a failure to reach the mail is a
// warning. Both log the account and the reason, never the error's text.
func TestUnavailableLinkLogs(t *testing.T) {
	t.Parallel()
	const secret = "the server said hunter2"
	tests := []struct {
		name    string
		link    items.Link
		spoil   func(map[string]*fake.Store)
		reason  string
		level   string
		message string
	}{
		{
			name: "account no longer configured", link: items.Link{Account: "gone", ThreadKey: "abc", RootMsgID: "root@x"},
			reason: api.ReasonNotFound, level: "DEBUG", message: "a linked thread's account is no longer configured",
		},
		{
			name: "mail server unavailable", link: items.Link{Account: "host", ThreadKey: "abc", RootMsgID: "root@x"},
			spoil: func(f map[string]*fake.Store) {
				f["host"].Fail("FindThread", fmt.Errorf("%w: %s", mailstore.ErrUnavailable, secret))
			},
			reason: api.ReasonServerUnavailable, level: "WARN", message: "resolving a linked thread failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, fakes, logs := linkServer(t)
			if tt.spoil != nil {
				tt.spoil(fakes)
			}
			got := s.resolveLink(t.Context(), tt.link)
			if got.rank != rankUnavailable || got.link.GetState() != linkUnavailable || got.link.GetReason() != tt.reason {
				t.Fatalf("link = %v (rank %d), want unavailable with %s", got.link, got.rank, tt.reason)
			}
			out := logs.String()
			want := fmt.Sprintf("level=%s msg=%q account=%s reason=%s", tt.level, tt.message, tt.link.Account, tt.reason)
			if !strings.Contains(out, want) {
				t.Errorf("log = %q, want a line with %q", out, want)
			}
			if tt.level != "WARN" && strings.Contains(out, "level=WARN") {
				t.Errorf("log = %q, want no warning", out)
			}
			if strings.Contains(out, secret) || strings.Contains(out, "err=") {
				t.Errorf("log = %q, want no error text", out)
			}
		})
	}
}

// linkTo is the link a view would hold for the message at r, as
// LinkThread stores it.
func linkTo(t *testing.T, s *Server, r ref.Ref) items.Link {
	t.Helper()
	acct, err := s.opts.Config.Account(r.Account)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.linkTarget(t.Context(), acct, r)
	if err != nil {
		t.Fatal(err)
	}
	return items.Link{Account: target.Account, ThreadKey: target.ThreadKey, RootMsgID: target.RootMsgID}
}

// linkKeys is each view link as account/thread_key state.
func linkKeys(links []*chainmailv1.ThreadLink) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.GetAccount() + "/" + l.GetThreadKey() + " " + l.GetState()
	}
	return out
}

// TestLinkViewsOrder checks the view's order of links: those with mail by
// their latest message, newest first, whatever order they come in, then
// the empty ones, then the unavailable, each group keeping the order
// given.
func TestLinkViewsOrder(t *testing.T) {
	t.Parallel()
	s, fakes, _ := linkServer(t)
	host := fakes["host"]
	plan := host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(5), Subject: "plan", MessageID: "plan@x"})
	planReply := host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(4), Subject: "re: plan", MessageID: "plan1@x", References: []string{"plan@x"}})
	lunch := host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(1), Subject: "lunch", MessageID: "lunch@x"})
	planLink, lunchLink := linkTo(t, s, plan), linkTo(t, s, lunch)
	if planLink.ThreadKey == lunchLink.ThreadKey {
		t.Fatalf("plan and lunch share thread key %s", planLink.ThreadKey)
	}
	goneA := items.Link{Account: "gone", ThreadKey: "a1", RootMsgID: "a1@x"}
	goneB := items.Link{Account: "gone", ThreadKey: "a2", RootMsgID: "a2@x"}
	emptyA := items.Link{Account: "host", ThreadKey: "e1", RootMsgID: "nobody1@x"}
	emptyB := items.Link{Account: "host", ThreadKey: "e2", RootMsgID: "nobody2@x"}

	got := s.linkViews(t.Context(), []items.Link{goneA, emptyA, planLink, goneB, emptyB, lunchLink})
	want := []string{
		"host/" + lunchLink.ThreadKey + " ok", "host/" + planLink.ThreadKey + " ok",
		"host/e1 empty", "host/e2 empty", "gone/a1 unavailable", "gone/a2 unavailable",
	}
	if keys := linkKeys(got); !slices.Equal(keys, want) {
		t.Fatalf("view order = %q, want %q", keys, want)
	}
	if got[0].GetLatest().GetRef() != lunch.String() || got[1].GetLatest().GetRef() != planReply.String() || got[1].GetMessages() != 2 {
		t.Errorf("ok links = %v, want lunch then the plan thread's reply as latest", got[:2])
	}
	if reversed := s.linkViews(t.Context(), []items.Link{lunchLink, planLink}); !slices.Equal(linkKeys(reversed), want[:2]) {
		t.Errorf("view order from newest first = %q, want %q", linkKeys(reversed), want[:2])
	}
}

// TestItemViewCapsThreads links one more thread than the view shows and
// checks that GetItem's view resolves and returns the first maxViewRows
// links in the store's order, with threads_total counting them all.
func TestItemViewCapsThreads(t *testing.T) {
	t.Parallel()
	s, fakes, _ := linkServer(t)
	ctx := t.Context()
	actor := items.Actor{UID: 1000, Client: "test"}
	it, _, _, err := s.items.Create(ctx, actor, items.NewItem{Type: items.TypeTask, Title: "Many threads", Area: items.AreaWork})
	if err != nil {
		t.Fatal(err)
	}
	const links = maxViewRows + 1
	for i := range links {
		target := items.LinkTarget{Account: "host", ThreadKey: fmt.Sprintf("%04x", i), RootMsgID: fmt.Sprintf("r%d@x", i)}
		if _, _, _, err := s.items.LinkThread(ctx, actor, it.ID, target, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	before := fakes["host"].Calls("FindThread")
	view, err := s.itemView(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.GetThreadsTotal() != links || len(view.GetThreads()) != maxViewRows {
		t.Fatalf("threads %d of %d, want %d of %d", len(view.GetThreads()), view.GetThreadsTotal(), maxViewRows, links)
	}
	for i, l := range view.GetThreads() {
		if want := fmt.Sprintf("%04x", i); l.GetThreadKey() != want || l.GetState() != linkEmpty {
			t.Errorf("thread %d = %s %s, want %s empty", i, l.GetThreadKey(), l.GetState(), want)
		}
	}
	if got := fakes["host"].Calls("FindThread") - before; got != maxViewRows {
		t.Errorf("FindThread calls = %d, want %d: only the links shown are resolved", got, maxViewRows)
	}
}

// TestLinkThreadRecoversAMovedAnchorFromTheCache links through a ref whose
// cached row went with a UIDVALIDITY rebuild while its message is cached
// at a new place, and checks that the link is made from the recovered row
// without reading the message from the server.
func TestLinkThreadRecoversAMovedAnchorFromTheCache(t *testing.T) {
	t.Parallel()
	f := newRouteFixture(t)
	f.s.cache = f.store
	host := f.fakes["host"]
	host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(3), Subject: "plan", MessageID: "root@x"})
	old := host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(2), Subject: "re: plan", MessageID: "moved@x", References: []string{"root@x"}})
	f.warm(t, "host")
	f.install(t, "host")
	ctx := t.Context()
	headers, err := host.FetchHeaders(ctx, mailstore.Source{Folder: old.Folder, UIDValidity: old.UIDValidity}, []uint32{old.UID})
	if err != nil || len(headers) != 1 {
		t.Fatalf("headers = %v, %v", headers, err)
	}
	if err := f.store.DropFolder(ctx, "host", "INBOX", true); err != nil {
		t.Fatal(err)
	}
	h := headers[0]
	h.Summary.Ref.Folder, h.Summary.Ref.UIDValidity, h.Summary.Ref.UID = "Archive", 77, 99
	if err := f.store.PutRows(ctx, []cache.Row{cache.RowFromHeader(h)}); err != nil {
		t.Fatal(err)
	}
	moved, ok, err := f.store.Row(ctx, h.Summary.Ref)
	if err != nil || !ok || moved.ThreadID == "" {
		t.Fatalf("moved row = %+v, %t, %v; want it cached with a thread key", moved, ok, err)
	}
	host.Fail("FetchMessages", fmt.Errorf("%w: the server must not be read", mailstore.ErrUnavailable))
	before := host.Calls("FetchMessages")

	actor := items.Actor{UID: 1000, Client: "test"}
	it, _, _, err := f.s.items.Create(ctx, actor, items.NewItem{Type: items.TypeTask, Title: "Plan", Area: items.AreaWork})
	if err != nil {
		t.Fatal(err)
	}
	entry := &audit.Entry{}
	if _, err := f.s.linkThread(ctx, actor, &chainmailv1.LinkThreadRequest{ItemId: it.ID, Ref: old.String(), Anchor: true}, entry); err != nil {
		t.Fatalf("linkThread: %v", err)
	}
	if got := host.Calls("FetchMessages"); got != before {
		t.Errorf("FetchMessages calls = %d, want %d: the cache recovers the ref", got, before)
	}
	got, err := f.s.items.Links(ctx, it.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("links = %+v, %v; want one", got, err)
	}
	if l := got[0]; l.Account != "host" || l.ThreadKey != moved.ThreadID || l.RootMsgID != "root@x" || l.AnchorMsgID != "moved@x" {
		t.Errorf("link = %+v, want host, thread %s, root root@x, anchor moved@x", l, moved.ThreadID)
	}
	if want := []string{"account=host", "thread=" + moved.ThreadID}; !slices.Equal(entry.Targets, want) {
		t.Errorf("audit targets = %q, want %q", entry.Targets, want)
	}
}
