package daemon_test

import (
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// updateMaskHint is the daemon's hint for a bad update_mask.
const updateMaskHint = "paths are type, title, description, state, area, priority, start_date, due_date, estimate and parent_id"

// itemUpdate sends an UpdateItem with the given mask paths.
func itemUpdate(t *testing.T, h *daemontest.Harness, req *chainmailv1.UpdateItemRequest, paths ...string) (*chainmailv1.WorkItem, error) {
	t.Helper()
	if paths != nil {
		req.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
	}
	resp, err := h.Client.Items.UpdateItem(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetItem(), nil
}

// TestUpdateItem checks a change of three fields that ignores a field the
// mask leaves out, clears that fall back to inheritance and the p2
// default, a move, and the audit rows.
func TestUpdateItem(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	id := itemTask(t, h, "Draft")
	before := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "epic", Title: "Launch", Area: "work"}).GetItem()
	it, err := itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id, Title: "Draft the memo", Priority: "p0", DueDate: "2026-12-01", State: "done"}, "title", "priority", "due_date")
	if err != nil {
		t.Fatal(err)
	}
	if it.GetTitle().GetUntrusted() != "Draft the memo" || it.GetPriority() != "p0" || it.GetEffectivePriority() != "p0" ||
		it.GetDueDate() != "2026-12-01" || it.GetState() != "backlog" || it.GetRevision() <= before.GetRevision() || it.GetUpdatedBy() != itemActor() {
		t.Errorf("updated = %v, want the three named fields changed and state left alone", it)
	}
	it, err = itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id}, "priority", "due_date")
	if err != nil {
		t.Fatal(err)
	}
	if it.GetPriority() != "" || it.GetEffectivePriority() != "p2" || it.GetDueDate() != "" {
		t.Errorf("cleared = %v, want no priority (effective p2) and no due date", it)
	}
	it, err = itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id, ParentId: before.GetId()}, "parent_id")
	if err != nil {
		t.Fatal(err)
	}
	if it.GetParentId() != before.GetId() {
		t.Errorf("moved = %v, want it under %s", it, before.GetId())
	}
	if _, err := itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id, Title: "Draft it", Priority: "p9"}, "title"); err != nil {
		t.Errorf("a bad value in a field the mask leaves out: %v, want it ignored", err)
	}
	rows := itemAudit(t, h, "item.update")
	var want []daemontest.AuditRow
	for _, fields := range []string{"title,priority,due_date", "priority,due_date", "parent_id", "title"} {
		want = append(want, daemontest.AuditRow{Client: "test", Action: "item.update", Targets: itemTargets(t, "item="+id, "fields="+fields, "type=task"), Result: "ok"})
	}
	if !slices.Equal(rows, want) {
		t.Errorf("audit = %+v, want %+v", rows, want)
	}
}

// TestUpdateItemRefusals checks a missing or empty mask, an unknown path,
// a repeated path, a bad ID, an unknown item, a clear the rules refuse and
// a bad value in a named field, with the audit form of each.
func TestUpdateItemRefusals(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	id := itemTask(t, h, "Draft")
	tests := []struct {
		name    string
		req     *chainmailv1.UpdateItemRequest
		paths   []string
		code    connect.Code
		reason  string
		hint    string
		targets []string
	}{
		{name: "no mask", req: &chainmailv1.UpdateItemRequest{Id: id}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: updateMaskHint, targets: []string{"item=" + id, "fields="}},
		{name: "empty mask", req: &chainmailv1.UpdateItemRequest{Id: id}, paths: []string{}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: updateMaskHint, targets: []string{"item=" + id, "fields="}},
		{name: "unknown path", req: &chainmailv1.UpdateItemRequest{Id: id, Title: "x"}, paths: []string{"title", "owner"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: updateMaskHint, targets: []string{"item=" + id, "fields=(invalid)"}},
		{name: "malformed ID", req: &chainmailv1.UpdateItemRequest{Id: "draft", Title: "x"}, paths: []string{"title"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "item IDs start with it_; run: chainmail item list", targets: []string{"item=(invalid)", "fields=title"}},
		{name: "unknown item", req: &chainmailv1.UpdateItemRequest{Id: "it_nope", Title: "x"}, paths: []string{"title"}, code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list", targets: []string{"item=it_nope", "fields=title"}},
		{name: "clear the title", req: &chainmailv1.UpdateItemRequest{Id: id}, paths: []string{"title"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, targets: []string{"item=" + id, "fields=title"}},
		{name: "clear the root's area", req: &chainmailv1.UpdateItemRequest{Id: id}, paths: []string{"area", "area"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, targets: []string{"item=" + id, "fields=area"}},
		{name: "bad named value", req: &chainmailv1.UpdateItemRequest{Id: id, Priority: "urgent"}, paths: []string{"priority"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, targets: []string{"item=" + id, "fields=priority"}},
		{name: "done on the Admin item", req: &chainmailv1.UpdateItemRequest{Id: "it_admin_work", State: "done"}, paths: []string{"state"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, targets: []string{"item=it_admin_work", "fields=state"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := itemUpdate(t, h, tt.req, tt.paths...)
			wantItemErr(t, tt.name, err, tt.code, tt.reason, tt.hint)
		})
	}
	rows := itemAudit(t, h, "item.update")
	if len(rows) != len(tests) {
		t.Fatalf("audit rows = %+v, want one per call", rows)
	}
	for i, tt := range tests {
		want := daemontest.AuditRow{Client: "test", Action: "item.update", Targets: itemTargets(t, tt.targets...), Result: "error", Reason: tt.reason}
		if rows[i] != want {
			t.Errorf("%s: audit = %+v, want %+v", tt.name, rows[i], want)
		}
	}
}
