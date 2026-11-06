package daemon_test

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// TestDeleteAndRestoreItems checks that a parent with a live child needs
// cascade, that a cascade deletes and restores the subtree with one
// revision each, that a child of a deleted parent and a live item cannot
// be restored, that the ongoing Admin item cannot be deleted, and the
// audit rows.
func TestDeleteAndRestoreItems(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	epic := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "epic", Title: "Launch", Area: "work"}).GetItem().GetId()
	story := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "story", Title: "Signup", ParentId: epic}).GetItem().GetId()
	both := slices.Sorted(slices.Values([]string{epic, story}))

	_, err := h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: epic}))
	wantItemErr(t, "delete with a live child", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	del, err := h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: epic, Cascade: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(del.Msg.GetIds())); !slices.Equal(got, both) || del.Msg.GetRevision() <= 0 {
		t.Errorf("delete = %v, want both IDs and a revision", del.Msg)
	}
	if ids := itemListIDs(t, h, &chainmailv1.ListItemsRequest{}); slices.Contains(ids, epic) || slices.Contains(ids, story) {
		t.Errorf("live list = %v, want the deleted items left out", ids)
	}
	if ids := itemListIDs(t, h, &chainmailv1.ListItemsRequest{IncludeDeleted: true}); !slices.Contains(ids, epic) || !slices.Contains(ids, story) {
		t.Errorf("list with deleted = %v, want both deleted items", ids)
	}
	_, err = h.Client.Items.RestoreItem(t.Context(), connect.NewRequest(&chainmailv1.RestoreItemRequest{Id: story}))
	wantItemErr(t, "restore under a deleted parent", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	res, err := h.Client.Items.RestoreItem(t.Context(), connect.NewRequest(&chainmailv1.RestoreItemRequest{Id: epic, Cascade: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(res.Msg.GetIds())); !slices.Equal(got, both) || res.Msg.GetRevision() <= del.Msg.GetRevision() {
		t.Errorf("restore = %v, want both IDs at a later revision than %d", res.Msg, del.Msg.GetRevision())
	}
	_, err = h.Client.Items.RestoreItem(t.Context(), connect.NewRequest(&chainmailv1.RestoreItemRequest{Id: epic}))
	wantItemErr(t, "restore a live item", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	_, err = h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: "it_admin_work"}))
	wantItemErr(t, "delete Admin", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	_, err = h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: "Launch", Cascade: true}))
	wantItemErr(t, "malformed ID", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "item IDs start with it_; run: chainmail item list")

	deletes, restores := itemAudit(t, h, "item.delete"), itemAudit(t, h, "item.restore")
	wantDeletes := []daemontest.AuditRow{
		{Client: "test", Action: "item.delete", Targets: itemTargets(t, "item="+epic), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.delete", Targets: itemTargets(t, "item="+epic, "cascade=1", "ids=2"), Result: "ok"},
		{Client: "test", Action: "item.delete", Targets: itemTargets(t, "item=it_admin_work"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.delete", Targets: itemTargets(t, "item=(invalid)", "cascade=1"), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	wantRestores := []daemontest.AuditRow{
		{Client: "test", Action: "item.restore", Targets: itemTargets(t, "item="+story), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.restore", Targets: itemTargets(t, "item="+epic, "cascade=1", "ids=2"), Result: "ok"},
		{Client: "test", Action: "item.restore", Targets: itemTargets(t, "item="+epic), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	if !slices.Equal(deletes, wantDeletes) {
		t.Errorf("delete audit = %+v, want %+v", deletes, wantDeletes)
	}
	if !slices.Equal(restores, wantRestores) {
		t.Errorf("restore audit = %+v, want %+v", restores, wantRestores)
	}
}
