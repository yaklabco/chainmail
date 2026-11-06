package daemon_test

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/render"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// TestEditAndTickCriteria checks one EditCriteria call that removes,
// edits, reorders and adds, its refusals, ticking with and without
// evidence, done refused while criteria are unticked, unticking, and the
// audit rows.
func TestEditAndTickCriteria(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	created := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Ship", Area: "work", Criteria: []string{"one", "two", "three"}})
	id := created.GetItem().GetId()
	c1, c2, c3 := created.GetCriteria()[0].GetId(), created.GetCriteria()[1].GetId(), created.GetCriteria()[2].GetId()

	edit, err := h.Client.Items.EditCriteria(t.Context(), connect.NewRequest(&chainmailv1.EditCriteriaRequest{
		ItemId: id, Remove: []string{c2}, Edit: []*chainmailv1.CriterionText{{Id: c1, Text: "one, edited"}},
		Order: []string{c3, c1}, Add: []string{"four"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for i, c := range edit.Msg.GetCriteria() {
		texts = append(texts, c.GetText().GetUntrusted())
		if c.GetPosition() != int32(i) {
			t.Errorf("criterion %d has position %d", i, c.GetPosition())
		}
	}
	if want := []string{"three", "one, edited", "four"}; !slices.Equal(texts, want) || edit.Msg.GetRevision() <= created.GetItem().GetRevision() {
		t.Errorf("edited = %v at %d, want %v at a later revision", texts, edit.Msg.GetRevision(), want)
	}
	_, err = h.Client.Items.EditCriteria(t.Context(), connect.NewRequest(&chainmailv1.EditCriteriaRequest{ItemId: id, Order: []string{c3}}))
	wantItemErr(t, "order missing an ID", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	_, err = h.Client.Items.EditCriteria(t.Context(), connect.NewRequest(&chainmailv1.EditCriteriaRequest{ItemId: id, Edit: []*chainmailv1.CriterionText{{Id: "cr_nope", Text: "x"}}}))
	wantItemErr(t, "unknown criterion", err, connect.CodeNotFound, api.ReasonNotFound, "")
	const critHint = "criterion IDs start with cr_; run: chainmail item show <id>"
	for name, req := range map[string]*chainmailv1.EditCriteriaRequest{
		"malformed remove ID": {ItemId: id, Remove: []string{"c2"}},
		"malformed edit ID":   {ItemId: id, Edit: []*chainmailv1.CriterionText{{Id: "CR_1", Text: "x"}}},
		"malformed order ID":  {ItemId: id, Order: []string{c3, "cr_a b"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.Client.Items.EditCriteria(t.Context(), connect.NewRequest(req))
			wantItemErr(t, name, err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, critHint)
		})
	}

	tick := func(criterion string, ticked bool, evidence string) (*chainmailv1.TickCriterionResponse, error) {
		resp, err := h.Client.Items.TickCriterion(t.Context(), connect.NewRequest(&chainmailv1.TickCriterionRequest{ItemId: id, CriterionId: criterion, Ticked: ticked, Evidence: evidence}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	_, err = tick(c3, true, "")
	wantItemErr(t, "tick without evidence", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	ticked, err := tick(c3, true, "merged in\n#42")
	if err != nil {
		t.Fatal(err)
	}
	if c := ticked.GetCriterion(); !c.GetTicked() || c.GetEvidence().GetUntrusted() != render.CleanText("merged in\n#42") || c.GetTickedBy() != itemActor() || c.GetTickedAt() == nil {
		t.Errorf("ticked = %v", c)
	}
	_, err = itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id, State: "done"}, "state")
	wantItemErr(t, "done with unticked criteria", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	unticked, err := tick(c3, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if c := unticked.GetCriterion(); c.GetTicked() || c.GetEvidence().GetUntrusted() != "" || c.GetTickedAt() != nil || unticked.GetRevision() <= ticked.GetRevision() {
		t.Errorf("unticked = %v at %d, want it cleared at a later revision", c, unticked.GetRevision())
	}
	_, err = tick("c3", true, "x")
	wantItemErr(t, "malformed criterion ID", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "criterion IDs start with cr_; run: chainmail item show <id>")

	got := itemAudit(t, h, "item.criteria", "item.tick")
	want := []daemontest.AuditRow{
		{Client: "test", Action: "item.criteria", Targets: itemTargets(t, "item="+id, "criteria=3"), Result: "ok"},
		{Client: "test", Action: "item.criteria", Targets: itemTargets(t, "item="+id), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.criteria", Targets: itemTargets(t, "item="+id), Result: "error", Reason: api.ReasonNotFound},
		{Client: "test", Action: "item.criteria", Targets: itemTargets(t, "item="+id), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.criteria", Targets: itemTargets(t, "item="+id), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.criteria", Targets: itemTargets(t, "item="+id), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.tick", Targets: itemTargets(t, "item="+id, "criterion="+c3), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.tick", Targets: itemTargets(t, "item="+id, "criterion="+c3), Result: "ok"},
		{Client: "test", Action: "item.tick", Targets: itemTargets(t, "item="+id, "criterion="+c3), Result: "ok"},
		{Client: "test", Action: "item.tick", Targets: itemTargets(t, "item="+id, "criterion=(invalid)"), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %+v, want %+v", got, want)
	}
}

// TestSetBlocker checks adding a blocker, a repeat that changes nothing at
// the same revision, a cycle and a self block refused, removing it, a bad
// ID, and the audit rows.
func TestSetBlocker(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	a, b := itemTask(t, h, "a"), itemTask(t, h, "b")
	set := func(item, by string, linked bool) (*chainmailv1.SetBlockerResponse, error) {
		resp, err := h.Client.Items.SetBlocker(t.Context(), connect.NewRequest(&chainmailv1.SetBlockerRequest{ItemId: item, BlockedById: by, Linked: linked}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	added, err := set(a, b, true)
	if err != nil || !added.GetChanged() || added.GetRevision() <= 0 {
		t.Fatalf("add = %v, %v; want changed at a revision", added, err)
	}
	again, err := set(a, b, true)
	if err != nil || again.GetChanged() || again.GetRevision() != added.GetRevision() {
		t.Errorf("repeat = %v, %v; want unchanged at %d", again, err, added.GetRevision())
	}
	_, err = set(b, a, true)
	wantItemErr(t, "cycle", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	_, err = set(a, a, true)
	wantItemErr(t, "self", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	removed, err := set(a, b, false)
	if err != nil || !removed.GetChanged() || removed.GetRevision() <= added.GetRevision() {
		t.Errorf("remove = %v, %v; want changed at a later revision", removed, err)
	}
	_, err = set(a, "b", true)
	wantItemErr(t, "malformed blocker", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "item IDs start with it_; run: chainmail item list")

	got := itemAudit(t, h, "item.block")
	ok := daemontest.AuditRow{Client: "test", Action: "item.block", Targets: itemTargets(t, "item="+a, "blocked_by="+b), Result: "ok"}
	want := []daemontest.AuditRow{
		ok, ok,
		{Client: "test", Action: "item.block", Targets: itemTargets(t, "item="+b, "blocked_by="+a), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.block", Targets: itemTargets(t, "item="+a, "blocked_by="+a), Result: "error", Reason: api.ReasonInvalidArgument},
		ok,
		{Client: "test", Action: "item.block", Targets: itemTargets(t, "item="+a, "blocked_by=(invalid)"), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %+v, want %+v", got, want)
	}
}
