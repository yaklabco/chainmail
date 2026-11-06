package daemon_test

import (
	"fmt"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// itemList lists items with req and fails the test on an error.
func itemList(t *testing.T, h *daemontest.Harness, req *chainmailv1.ListItemsRequest) *chainmailv1.ListItemsResponse {
	t.Helper()
	resp, err := h.Client.Items.ListItems(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	return resp.Msg
}

// itemListIDs lists items with req and returns their IDs in order.
func itemListIDs(t *testing.T, h *daemontest.Harness, req *chainmailv1.ListItemsRequest) []string {
	t.Helper()
	resp, err := h.Client.Items.ListItems(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var out []string
	for _, r := range resp.Msg.GetItems() {
		out = append(out, r.GetId())
	}
	return out
}

// TestListItems checks a subtree in tree order with depths and counts,
// the inbox, type and area filters, paging, changed_since order, the
// refusals, and the audit targets of each.
func TestListItems(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	epic := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "epic", Title: "Launch", Area: "work"}).GetItem()
	story := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "story", Title: "Signup", ParentId: epic.GetId()}).GetItem()
	task := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Form", ParentId: story.GetId(), Criteria: []string{"a", "b"}}).GetItem()
	inbox := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Call the bank", Area: "personal"}).GetItem()

	under := itemList(t, h, &chainmailv1.ListItemsRequest{Under: epic.GetId()})
	rows := under.GetItems()
	if len(rows) != 2 || rows[0].GetId() != story.GetId() || rows[1].GetId() != task.GetId() ||
		rows[0].GetDepth() != 0 || rows[1].GetDepth() != 1 || rows[0].GetChildren() != 1 ||
		rows[1].GetCriteriaTotal() != 2 || rows[1].GetCriteriaTicked() != 0 || under.GetRevision() < inbox.GetRevision() {
		t.Errorf("under the epic = %v, want the story at depth 0 with one child, then the task at depth 1 with two criteria", under)
	}
	if got := itemListIDs(t, h, &chainmailv1.ListItemsRequest{Inbox: true}); !slices.Equal(got, []string{inbox.GetId()}) {
		t.Errorf("inbox = %v, want [%s]", got, inbox.GetId())
	}
	if got := itemListIDs(t, h, &chainmailv1.ListItemsRequest{Type: "task", Area: "work"}); !slices.Equal(got, []string{task.GetId()}) {
		t.Errorf("work tasks = %v, want [%s]", got, task.GetId())
	}
	first := itemList(t, h, &chainmailv1.ListItemsRequest{Under: epic.GetId(), Limit: 1})
	if len(first.GetItems()) != 1 || first.GetItems()[0].GetId() != story.GetId() || first.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, want the story and a token", first)
	}
	second := itemList(t, h, &chainmailv1.ListItemsRequest{Under: epic.GetId(), Limit: 1, PageToken: first.GetNextPageToken()})
	if len(second.GetItems()) != 1 || second.GetItems()[0].GetId() != task.GetId() || second.GetNextPageToken() != "" {
		t.Errorf("second page = %v, want the task and no token", second)
	}
	var changed []string
	var revs []int64
	for _, r := range itemList(t, h, &chainmailv1.ListItemsRequest{ChangedSince: proto.Int64(0)}).GetItems() {
		if r.GetId() == "it_admin_work" || r.GetId() == "it_admin_personal" {
			continue
		}
		changed, revs = append(changed, r.GetId()), append(revs, r.GetRevision())
	}
	if want := []string{epic.GetId(), story.GetId(), task.GetId(), inbox.GetId()}; !slices.Equal(changed, want) || !slices.IsSorted(revs) {
		t.Errorf("changed since 0 = %v at %v, want %v in revision order", changed, revs, want)
	}

	refusals := []struct {
		name string
		req  *chainmailv1.ListItemsRequest
		code connect.Code
		hint string
	}{
		{name: "limit too big", req: &chainmailv1.ListItemsRequest{Limit: 501}, code: connect.CodeInvalidArgument, hint: "0 means 100"},
		{name: "negative limit", req: &chainmailv1.ListItemsRequest{Limit: -1}, code: connect.CodeInvalidArgument, hint: "0 means 100"},
		{name: "negative changed_since", req: &chainmailv1.ListItemsRequest{ChangedSince: proto.Int64(-1)}, code: connect.CodeInvalidArgument, hint: "pass the revision of an earlier ListItems response"},
		{name: "unknown type", req: &chainmailv1.ListItemsRequest{Type: "todo"}, code: connect.CodeInvalidArgument},
		{name: "malformed under", req: &chainmailv1.ListItemsRequest{Under: "launch"}, code: connect.CodeInvalidArgument, hint: "item IDs start with it_; run: chainmail item list"},
		{name: "malformed token", req: &chainmailv1.ListItemsRequest{PageToken: "!!"}, code: connect.CodeInvalidArgument},
	}
	for _, tt := range refusals {
		_, err := h.Client.Items.ListItems(t.Context(), connect.NewRequest(tt.req))
		wantItemErr(t, tt.name, err, tt.code, api.ReasonInvalidArgument, tt.hint)
	}

	lists := itemAudit(t, h, "item.list")
	want := []daemontest.AuditRow{
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "under="+epic.GetId(), "rows=2"), Result: "ok"},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "inbox=1", "rows=1"), Result: "ok"},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "type=task", "area=work", "rows=1"), Result: "ok"},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "under="+epic.GetId(), "rows=1"), Result: "ok"},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "under="+epic.GetId(), "rows=1"), Result: "ok"},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "changed_since=0", fmt.Sprintf("rows=%d", len(changed)+2)), Result: "ok"},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "rows=0"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "rows=0"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "changed_since=-1", "rows=0"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "type=(invalid)", "rows=0"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "under=(invalid)", "rows=0"), Result: "error", Reason: api.ReasonInvalidArgument},
		{Client: "test", Action: "item.list", Targets: itemTargets(t, "rows=0"), Result: "error", Reason: api.ReasonInvalidArgument},
	}
	if !slices.Equal(lists, want) {
		t.Errorf("list audit = %+v, want %+v", lists, want)
	}
	ongoing := slices.Sorted(slices.Values(itemListIDs(t, h, &chainmailv1.ListItemsRequest{Ongoing: proto.Bool(true)})))
	if !slices.Equal(ongoing, []string{"it_admin_personal", "it_admin_work"}) {
		t.Errorf("ongoing items = %v, want the two Admin items", ongoing)
	}
}
