package daemon_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/watch"
)

// TestRulesGroupsAreHiddenAndRefused checks that rules and cache groups
// are left out of ListGroups and its audit count, and that Subscribe,
// ReadEvents, Ack and DeleteGroup refuse a rules group's name with the
// group hint, leaving the group where it was.
func TestRulesGroupsAreHiddenAndRefused(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	rulesGroup := watch.RulesGroup("host")
	watchPublish(t, h, watchNew("host", "INBOX", 1, "a"))
	for _, g := range []string{"alpha", rulesGroup, watch.CacheGroup("host")} {
		if _, _, err := watchLog(h).Group(t.Context(), g); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := h.Client.Watch.ListGroups(t.Context(), connect.NewRequest(&chainmailv1.ListGroupsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range resp.Msg.GetGroups() {
		names = append(names, g.GetName())
	}
	if !slices.Equal(names, []string{"alpha"}) {
		t.Errorf("ListGroups names = %v, want [alpha]", names)
	}
	var listed string
	for _, r := range h.AuditRows(t) {
		if r.Action == "watch.groups.list" {
			listed = r.Targets
		}
	}
	if want := itemTargets(t, "groups=1"); listed != want {
		t.Errorf("watch.groups.list targets = %s, want %s", listed, want)
	}
	refused := func(route string, err error) {
		t.Helper()
		if reasonOf(err) != api.ReasonInvalidArgument || api.Detail(err).GetHint() != groupHint {
			t.Errorf("%s: %v (hint %q), want INVALID_ARGUMENT with the group hint", route, err, api.Detail(err).GetHint())
		}
	}
	refused("Subscribe", subscribeErr(t, h, &chainmailv1.SubscribeRequest{Group: rulesGroup}))
	_, err = h.Client.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{Group: rulesGroup}))
	refused("ReadEvents", err)
	_, err = h.Client.Watch.Ack(t.Context(), connect.NewRequest(&chainmailv1.AckRequest{Group: rulesGroup, Id: 1}))
	refused("Ack", err)
	_, err = h.Client.Watch.DeleteGroup(t.Context(), connect.NewRequest(&chainmailv1.DeleteGroupRequest{Name: rulesGroup}))
	refused("DeleteGroup", err)
	groups, err := watchLog(h).Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(groups, func(g watch.GroupInfo) bool { return g.Name == rulesGroup && g.AckedID == 1 }) {
		t.Errorf("groups = %+v, want %s still acknowledged at 1", groups, rulesGroup)
	}
}

// TestWatchServesTheRuleAndDueKinds checks that a reader naming the new
// kinds gets their payloads, that a reader naming none gets neither, and
// the refusal text for an unknown kind.
func TestWatchServesTheRuleAndDueKinds(t *testing.T) {
	t.Parallel()
	h := startWatch(t, 0)
	watchPublish(t, h,
		watch.Event{At: time.Now(), Kind: watch.KindRule, Rule: &watch.RuleChange{RuleID: "ru_a", Name: "tidy", Change: "created", Revision: 1, Actor: "1000/cli"}},
		watch.Event{At: time.Now(), Kind: watch.KindItemDue, Due: &watch.ItemDue{ItemID: "it_a", Type: "task", DueDate: "2026-11-07", State: "backlog"}},
		watchNew("host", "INBOX", 1, "a"),
	)
	kinds := []string{"rule", "item_due_soon", "item_due", "item_overdue"}
	events := readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Filter: &chainmailv1.WatchFilter{Kinds: kinds}}).GetEvents()
	if len(events) != 2 || events[0].GetRule().GetRuleId() != "ru_a" || events[0].GetRule().GetChange() != "created" ||
		events[1].GetKind() != "item_due" || events[1].GetDue().GetItemId() != "it_a" || events[1].GetDue().GetDueDate() != "2026-11-07" {
		t.Errorf("events = %v, want the rule event and the item_due event with their payloads", events)
	}
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0)}).GetEvents() {
		if e.GetKind() != "new" {
			t.Errorf("a reader with no kinds got %v; the rule and due kinds are opt-in", e)
		}
	}
	_, err := h.Client.Watch.ReadEvents(t.Context(), connect.NewRequest(&chainmailv1.ReadEventsRequest{Filter: &chainmailv1.WatchFilter{Kinds: []string{"bogus"}}}))
	var cerr *connect.Error
	if reasonOf(err) != api.ReasonInvalidArgument || !errors.As(err, &cerr) ||
		cerr.Message() != "kinds must be new, flags, expunged, status, gap, item, item_mail, rule, item_due_soon, item_due, item_overdue, invite, event_changed, event_cancelled or invite_reply" {
		t.Errorf("unknown kind: %v, want INVALID_ARGUMENT listing every kind", err)
	}
}
