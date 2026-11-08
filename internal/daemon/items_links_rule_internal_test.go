package daemon

import (
	"fmt"
	"os"
	"slices"
	"testing"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
)

// TestLinkThreadAsRecordsTheRuleTargets links a message's thread to an
// item as the rule caller, with no socket peer in the context. The link
// and the item are stamped with the actor <uid>/rule, and item.link is
// audited under client rule with the rule's targets after the call's own.
// A repeat changes nothing and is still audited, and a refused call is
// audited with the targets too.
func TestLinkThreadAsRecordsTheRuleTargets(t *testing.T) {
	t.Parallel()
	s, fakes, _ := linkServer(t)
	ctx := t.Context()
	msg := fakes["host"].Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(1), Subject: "invoice", MessageID: "inv@x"})
	it, _, _, err := s.items.Create(ctx, items.Actor{UID: 1000, Client: "test"}, items.NewItem{Type: items.TypeTask, Title: "Invoices", Area: items.AreaWork})
	if err != nil {
		t.Fatal(err)
	}
	req := &chainmailv1.LinkThreadRequest{ItemId: it.ID, Ref: msg.String(), Anchor: true}

	resp, err := s.linkThreadAs(ctx, ruleCaller(), req, testRuleTargets)
	if err != nil || !resp.GetCreated() {
		t.Fatalf("linkThreadAs = %+v, %v; want a new link", resp, err)
	}
	wantActor := fmt.Sprintf("%d/rule", os.Getuid())
	links, err := s.items.Links(ctx, it.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %+v, %v; want one", links, err)
	}
	if l := links[0]; l.CreatedBy != wantActor || l.Account != "host" || l.AnchorMsgID != "inv@x" {
		t.Errorf("link = %+v, want created by %s on host anchored at inv@x", l, wantActor)
	}
	got, err := s.items.Get(ctx, it.ID)
	if err != nil || got.UpdatedBy != wantActor {
		t.Errorf("item updated by %q (%v), want %s", got.UpdatedBy, err, wantActor)
	}

	again, err := s.linkThreadAs(ctx, ruleCaller(), req, testRuleTargets)
	if err != nil || again.GetCreated() {
		t.Errorf("repeat = %+v, %v; want the existing link", again, err)
	}

	_, err = s.linkThreadAs(ctx, ruleCaller(), &chainmailv1.LinkThreadRequest{ItemId: "nope", Ref: msg.String()}, testRuleTargets)
	if reason := api.Detail(err).GetReason(); reason != api.ReasonInvalidArgument {
		t.Errorf("bad item ID: err = %v, want INVALID_ARGUMENT", err)
	}

	rows := ruleAuditRows(t, s, "item.link")
	if len(rows) != 3 {
		t.Fatalf("audit rows = %+v, want three", rows)
	}
	linked := slices.Concat([]string{"item=" + it.ID, "account=host", "thread=" + links[0].ThreadKey}, testRuleTargets)
	for i, want := range []struct {
		targets []string
		result  audit.Result
		reason  string
	}{
		{targets: linked, result: audit.ResultOK},
		{targets: linked, result: audit.ResultOK},
		{targets: slices.Concat([]string{"item=" + invalidTarget}, testRuleTargets), result: audit.ResultError, reason: api.ReasonInvalidArgument},
	} {
		row := rows[i]
		if row.Client != ruleClient || row.CallerUID != uint32(os.Getuid()) || row.Result != want.result || row.Reason != want.reason {
			t.Errorf("row %d = %+v, want client rule, uid %d, %s %s", i, row, os.Getuid(), want.result, want.reason)
		}
		if !slices.Equal(row.Targets, want.targets) {
			t.Errorf("row %d targets = %q, want %q", i, row.Targets, want.targets)
		}
	}
}
