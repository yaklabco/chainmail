package items_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/watch"
)

func TestLinkThreadCreatesALink(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	id := r.task(t)
	before := r.revision(t)
	at := r.clock.Now()
	tgt := linkTarget("work", "t1")
	got, created, rev, err := r.store.LinkThread(t.Context(), linkActor, id, tgt, true, noteMarker)
	if err != nil {
		t.Fatalf("LinkThread: %v", err)
	}
	want := items.Link{
		ItemID: id, Account: "work", ThreadKey: "t1", RootMsgID: tgt.RootMsgID, AnchorMsgID: tgt.MsgID,
		Note: noteMarker, CreatedAt: at, CreatedBy: "1000/test",
	}
	if !created || rev != before+1 || !sameLink(got, want) {
		t.Errorf("LinkThread = %+v, %v, %d; want %+v, true, %d", got, created, rev, want, before+1)
	}
	if now := r.revision(t); now != rev {
		t.Errorf("revision = %d after the link, want %d", now, rev)
	}
	links, err := r.store.Links(t.Context(), id)
	if err != nil || len(links) != 1 || !sameLink(links[0], want) {
		t.Errorf("Links = %+v, %v; want the new link", links, err)
	}
	it, err := r.store.Get(t.Context(), id)
	if err != nil || it.Revision != rev || it.UpdatedBy != "1000/test" || !it.UpdatedAt.Equal(at) {
		t.Errorf("item after the link: revision %d, updated by %q at %v, %v; want %d by 1000/test at %v",
			it.Revision, it.UpdatedBy, it.UpdatedAt, err, rev, at)
	}
	events := r.events(t, watch.KindItem)
	if len(events) != 2 {
		t.Fatalf("item events = %d, want created then linked", len(events))
	}
	wantItemEvent(t, events[1], id, items.TypeTask, rev, items.OpLinked)
	if n := r.count(t, "SELECT count(*) FROM events WHERE instr(payload, ?) > 0", noteMarker); n != 0 {
		t.Errorf("%d events hold the note text", n)
	}
}

func TestLinkThreadOnALinkedThread(t *testing.T) {
	t.Parallel()
	first := linkTarget("work", "t1")
	reply := "reply-t1@example.net"
	tests := []struct {
		name        string
		firstAnchor bool
		firstNote   string
		anchor      bool
		msgID       string // the second call's message
		note        string
		wantAnchor  string
		wantNote    string
		wantChange  bool
	}{
		{"sets the anchor", false, "first", true, reply, "", reply, "first", true},
		{"moves the anchor", true, "first", true, reply, "", reply, "first", true},
		{"sets the note", false, "first", false, first.MsgID, "second", "", "second", true},
		{"sets both in one revision", false, "", true, reply, "second", reply, "second", true},
		{"no anchor and no note change nothing", true, "first", false, reply, "", first.MsgID, "first", false},
		{"the same anchor and note change nothing", true, "first", true, first.MsgID, "first", first.MsgID, "first", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLinkRig(t)
			id := r.task(t)
			old, _, _, err := r.store.LinkThread(t.Context(), linkActor, id, first, tc.firstAnchor, tc.firstNote)
			if err != nil {
				t.Fatalf("first LinkThread: %v", err)
			}
			before := r.revision(t)
			logged := len(r.events(t, watch.KindItem))
			r.clock.Advance(time.Second)
			second := first
			second.MsgID = tc.msgID
			got, created, rev, err := r.store.LinkThread(t.Context(), linkActor, id, second, tc.anchor, tc.note)
			if err != nil || created {
				t.Fatalf("second LinkThread = created %v, %v; want an update of the link", created, err)
			}
			want := old
			want.AnchorMsgID, want.Note = tc.wantAnchor, tc.wantNote
			if !sameLink(got, want) {
				t.Errorf("link = %+v, want %+v", got, want)
			}
			wantRev := before
			if tc.wantChange {
				wantRev++
			}
			if rev != wantRev || r.revision(t) != wantRev {
				t.Errorf("revision = %d (stored %d), want %d", rev, r.revision(t), wantRev)
			}
			events := r.events(t, watch.KindItem)
			switch {
			case !tc.wantChange && len(events) != logged:
				t.Errorf("item events = %d, want still %d", len(events), logged)
			case tc.wantChange && len(events) != logged+1:
				t.Errorf("item events = %d, want %d", len(events), logged+1)
			case tc.wantChange:
				wantItemEvent(t, events[len(events)-1], id, items.TypeTask, wantRev, items.OpLinked)
			}
			links, err := r.store.Links(t.Context(), id)
			if err != nil || len(links) != 1 || !sameLink(links[0], want) {
				t.Errorf("Links = %+v, %v; want only %+v", links, err, want)
			}
		})
	}
}

func TestLinkThreadRefusals(t *testing.T) {
	t.Parallel()
	live := func(t *testing.T, r *linkRig) string { return r.task(t) }
	deleted := func(t *testing.T, r *linkRig) string {
		id := r.task(t)
		r.remove(t, id)
		return id
	}
	unknown := func(*testing.T, *linkRig) string { return "it_missing" }
	tests := []struct {
		name     string
		item     func(*testing.T, *linkRig) string
		target   items.LinkTarget
		anchor   bool
		note     string
		notFound string // the ErrNotFound text; "" means an *items.InvalidError
	}{
		{"no thread key", live, items.LinkTarget{Account: "work", RootMsgID: "r@example.net", MsgID: "m@example.net"}, false, "", ""},
		{"no account", live, items.LinkTarget{ThreadKey: "t1", MsgID: "m@example.net"}, false, "", ""},
		{"an anchor with no Message-ID", live, items.LinkTarget{Account: "work", ThreadKey: "t1"}, true, "", ""},
		{"a note too long", live, linkTarget("work", "t1"), false, noteMarker + strings.Repeat("é", items.MaxNote), ""},
		{"a deleted item", deleted, linkTarget("work", "t1"), false, noteMarker, ""},
		{"an unknown item", unknown, linkTarget("work", "t1"), false, noteMarker, "not found: item"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLinkRig(t)
			id := tc.item(t, r)
			before := r.revision(t)
			_, _, _, err := r.store.LinkThread(t.Context(), linkActor, id, tc.target, tc.anchor, tc.note)
			var inv *items.InvalidError
			switch {
			case tc.notFound != "":
				if !errors.Is(err, items.ErrNotFound) || err.Error() != tc.notFound {
					t.Errorf("LinkThread = %v, want %q", err, tc.notFound)
				}
			case !errors.As(err, &inv):
				t.Errorf("LinkThread = %v, want an *items.InvalidError", err)
			case strings.Contains(inv.Msg+inv.Hint, noteMarker):
				t.Errorf("the refusal %q, %q repeats the note", inv.Msg, inv.Hint)
			}
			if err != nil && strings.Contains(err.Error(), noteMarker) {
				t.Errorf("error %q repeats the note", err)
			}
			if after := r.revision(t); after != before {
				t.Errorf("revision = %d after a refused link, want %d", after, before)
			}
			if n := r.count(t, "SELECT count(*) FROM item_links"); n != 0 {
				t.Errorf("%d links stored after a refused link", n)
			}
		})
	}
}

func TestUnlinkThread(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	id := r.task(t)
	r.link(t, id, "work", "t1")
	kept := r.link(t, id, "home", "t2")
	before := r.revision(t)
	r.clock.Advance(time.Second)
	rev, err := r.store.UnlinkThread(t.Context(), linkActor, id, "work", "t1")
	if err != nil || rev != before+1 {
		t.Fatalf("UnlinkThread = %d, %v; want revision %d", rev, err, before+1)
	}
	links, err := r.store.Links(t.Context(), id)
	if err != nil || len(links) != 1 || !sameLink(links[0], kept) {
		t.Errorf("Links after the unlink = %+v, %v; want only the home link", links, err)
	}
	it, err := r.store.Get(t.Context(), id)
	if err != nil || it.Revision != rev || !it.UpdatedAt.Equal(r.clock.Now()) {
		t.Errorf("item after the unlink: revision %d at %v, %v; want %d at %v", it.Revision, it.UpdatedAt, err, rev, r.clock.Now())
	}
	events := r.events(t, watch.KindItem)
	wantItemEvent(t, events[len(events)-1], id, items.TypeTask, rev, items.OpUnlinked)

	gone := r.task(t)
	r.link(t, gone, "work", "t3")
	r.remove(t, gone)
	tests := []struct {
		name, id, account, key string
		notFound               string // "" means an *items.InvalidError
	}{
		{"a link already removed", id, "work", "t1", "not found: link"},
		{"a link the item never had", id, "work", "t9", "not found: link"},
		{"an unknown item", "it_missing", "work", "t1", "not found: item"},
		{"a deleted item", gone, "work", "t3", ""},
	}
	for _, tc := range tests {
		at := r.revision(t)
		_, err := r.store.UnlinkThread(t.Context(), linkActor, tc.id, tc.account, tc.key)
		var inv *items.InvalidError
		switch {
		case tc.notFound != "" && (!errors.Is(err, items.ErrNotFound) || err.Error() != tc.notFound):
			t.Errorf("%s: UnlinkThread = %v, want %q", tc.name, err, tc.notFound)
		case tc.notFound == "" && !errors.As(err, &inv):
			t.Errorf("%s: UnlinkThread = %v, want an *items.InvalidError", tc.name, err)
		}
		if after := r.revision(t); after != at {
			t.Errorf("%s: revision = %d after a refused unlink, want %d", tc.name, after, at)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM item_links WHERE item_id = ?", gone); n != 1 {
		t.Errorf("the deleted item has %d links, want its link kept", n)
	}
}

func TestLinksNewestFirst(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	id := r.task(t)
	for _, l := range []struct{ account, key string }{{"work", "k1"}, {"home", "k2"}, {"work", "k3"}} {
		r.link(t, id, l.account, l.key)
		r.clock.Advance(time.Second)
	}
	keys := func() []string {
		t.Helper()
		links, err := r.store.Links(t.Context(), id)
		if err != nil {
			t.Fatalf("Links: %v", err)
		}
		var out []string
		for _, l := range links {
			out = append(out, l.ThreadKey)
		}
		return out
	}
	want := []string{"k3", "k2", "k1"}
	if got := keys(); !slices.Equal(got, want) {
		t.Errorf("Links keys = %q, want %q", got, want)
	}
	r.remove(t, id)
	if got := keys(); !slices.Equal(got, want) {
		t.Errorf("Links keys of the deleted item = %q, want them kept: %q", got, want)
	}
	if links, err := r.store.Links(t.Context(), r.task(t)); err != nil || len(links) != 0 {
		t.Errorf("Links of an item with none = %+v, %v; want none", links, err)
	}
	if _, err := r.store.Links(t.Context(), "it_missing"); !errors.Is(err, items.ErrNotFound) {
		t.Errorf("Links of an unknown item = %v, want ErrNotFound", err)
	}
}
