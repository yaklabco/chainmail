package daemon_test

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// itemGet returns an item's view and fails the test on an error.
func itemGet(t *testing.T, h *daemontest.Harness, id string) *chainmailv1.GetItemResponse {
	t.Helper()
	resp, err := h.Client.Items.GetItem(t.Context(), connect.NewRequest(&chainmailv1.GetItemRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetItem(%s): %v", id, err)
	}
	return resp.Msg
}

// itemRowIDs returns the IDs of rows, in order.
func itemRowIDs(rows []*chainmailv1.WorkItemRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.GetId()
	}
	return out
}

// TestGetItemView checks the view of an item with no links: its own and
// inherited fields, criteria, ancestors from the root, children capped at
// 50 with their total, blockers both ways and live only, a deleted item's
// view, the refusals, and the item.get audit rows.
func TestGetItemView(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	ctx := t.Context()
	epic := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "epic", Title: "Launch", Area: "work", Priority: "p1"}).GetItem().GetId()
	story := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "story", Title: "Signup", ParentId: epic}).GetItem().GetId()
	task := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Form", ParentId: story, Criteria: []string{"validates"}}).GetItem().GetId()
	blocker := itemTask(t, h, "Legal review")
	if _, err := h.Client.Items.SetBlocker(ctx, connect.NewRequest(&chainmailv1.SetBlockerRequest{ItemId: task, BlockedById: blocker, Linked: true})); err != nil {
		t.Fatal(err)
	}

	view := itemGet(t, h, task)
	it := view.GetItem()
	if it.GetId() != task || it.GetType() != "task" || it.GetEffectiveArea() != "work" || it.GetEffectivePriority() != "p1" || it.GetDeletedAt() != nil {
		t.Errorf("item = %v", it)
	}
	if len(view.GetCriteria()) != 1 || view.GetCriteria()[0].GetText().GetUntrusted() != "validates" {
		t.Errorf("criteria = %v", view.GetCriteria())
	}
	if got := itemRowIDs(view.GetAncestors()); !slices.Equal(got, []string{epic, story}) {
		t.Errorf("ancestors = %v, want the epic then the story", got)
	}
	if len(view.GetChildren()) != 0 || view.GetChildrenTotal() != 0 || len(view.GetBlocks()) != 0 || len(view.GetThreads()) != 0 || view.GetThreadsTotal() != 0 {
		t.Errorf("view = %v, want no children, blocks or threads", view)
	}
	if got := itemRowIDs(view.GetBlockedBy()); !slices.Equal(got, []string{blocker}) {
		t.Errorf("blocked_by = %v, want [%s]", got, blocker)
	}
	if got := itemRowIDs(itemGet(t, h, blocker).GetBlocks()); !slices.Equal(got, []string{task}) {
		t.Errorf("the blocker blocks %v, want [%s]", got, task)
	}
	if got := itemGet(t, h, story); !slices.Equal(itemRowIDs(got.GetChildren()), []string{task}) || got.GetChildrenTotal() != 1 {
		t.Errorf("story children = %v of %d, want [%s] of 1", itemRowIDs(got.GetChildren()), got.GetChildrenTotal(), task)
	}
	for range 51 {
		itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Chore", ParentId: epic})
	}
	if got := itemGet(t, h, epic); len(got.GetChildren()) != 50 || got.GetChildrenTotal() != 52 {
		t.Errorf("epic children = %d of %d, want 50 of 52", len(got.GetChildren()), got.GetChildrenTotal())
	}
	if _, err := h.Client.Items.DeleteItem(ctx, connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: blocker})); err != nil {
		t.Fatal(err)
	}
	if got := itemGet(t, h, blocker).GetItem(); got.GetDeletedAt() == nil {
		t.Errorf("deleted blocker = %v, want deleted_at set", got)
	}
	if got := itemGet(t, h, task).GetBlockedBy(); len(got) != 0 {
		t.Errorf("blocked_by after the blocker's delete = %v, want live blockers only", got)
	}
	_, err := h.Client.Items.GetItem(ctx, connect.NewRequest(&chainmailv1.GetItemRequest{Id: "it_nope"}))
	wantItemErr(t, "unknown item", err, connect.CodeNotFound, api.ReasonNotFound, "run: chainmail item list")
	_, err = h.Client.Items.GetItem(ctx, connect.NewRequest(&chainmailv1.GetItemRequest{Id: "Form"}))
	wantItemErr(t, "malformed ID", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "item IDs start with it_; run: chainmail item list")

	gets := itemAudit(t, h, "item.get")
	if len(gets) != 8 {
		t.Fatalf("item.get rows = %+v, want 8", gets)
	}
	want := []daemontest.AuditRow{
		{Client: "test", Action: "item.get", Targets: itemTargets(t, "item="+task, "type=task"), Result: "ok"},
		{Client: "test", Action: "item.get", Targets: itemTargets(t, "item=it_nope"), Result: "error", Reason: api.ReasonNotFound},
		{Client: "test", Action: "item.get", Targets: itemTargets(t, "item=(invalid)"), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	if got := []daemontest.AuditRow{gets[0], gets[6], gets[7]}; !slices.Equal(got, want) {
		t.Errorf("audit = %+v, want %+v", got, want)
	}
}
