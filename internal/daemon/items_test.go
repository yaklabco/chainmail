package daemon_test

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/render"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// itemMarker is item text the tests send so they can prove that no error,
// hint, audit row or log line repeats it.
const itemMarker = "ZQX-ITEM-TEXT-7731"

// itemCreate creates an item and fails the test on an error.
func itemCreate(t *testing.T, h *daemontest.Harness, req *chainmailv1.CreateItemRequest) *chainmailv1.CreateItemResponse {
	t.Helper()
	resp, err := h.Client.Items.CreateItem(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return resp.Msg
}

// itemTask creates a task in the work inbox and returns its ID.
func itemTask(t *testing.T, h *daemontest.Harness, title string) string {
	t.Helper()
	return itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: title, Area: "work"}).GetItem().GetId()
}

// itemAudit returns the audit rows of item calls in order: every item.
// action, or only those named.
func itemAudit(t *testing.T, h *daemontest.Harness, actions ...string) []daemontest.AuditRow {
	t.Helper()
	var out []daemontest.AuditRow
	for _, r := range h.AuditRows(t) {
		if (len(actions) == 0 && strings.HasPrefix(r.Action, "item.")) || slices.Contains(actions, r.Action) {
			out = append(out, r)
		}
	}
	return out
}

// itemTargets is targets as an audit row stores them.
func itemTargets(t *testing.T, targets ...string) string {
	t.Helper()
	b, err := json.Marshal(targets)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantItemErr checks err's Connect code and API reason, and its hint when
// hint is not empty.
func wantItemErr(t *testing.T, what string, err error, code connect.Code, reason, hint string) {
	t.Helper()
	if connect.CodeOf(err) != code || reasonOf(err) != reason {
		t.Errorf("%s: %v (%s), want %v %s", what, err, reasonOf(err), code, reason)
		return
	}
	if hint != "" && api.Detail(err).GetHint() != hint {
		t.Errorf("%s: hint %q, want %q", what, api.Detail(err).GetHint(), hint)
	}
}

// itemActor is created_by and updated_by for a change the test client made.
func itemActor() string { return fmt.Sprintf("%d/test", os.Getuid()) }

// TestCreateItem checks a root epic with every field and two criteria, a
// task under it that inherits area and priority, and both audit rows.
func TestCreateItem(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	title, desc := "Plan\tthe launch", "Line one\n\tLine two"
	resp := itemCreate(t, h, &chainmailv1.CreateItemRequest{
		Type: "epic", Title: title, Description: desc, Area: "work", Priority: "p1",
		StartDate: "2026-11-09", DueDate: "2026-11-30", Estimate: proto.Float64(3.5),
		Criteria: []string{"Spec\tagreed", "Shipped"},
	})
	epic := resp.GetItem()
	if !strings.HasPrefix(epic.GetId(), "it_") || epic.GetType() != "epic" || resp.GetDuplicate() {
		t.Fatalf("epic = %v, duplicate %v", epic, resp.GetDuplicate())
	}
	if epic.GetTitle().GetUntrusted() != render.CleanHeader(title) || epic.GetDescription().GetUntrusted() != render.CleanText(desc) {
		t.Errorf("title %q description %q, want them wrapped and cleaned", epic.GetTitle().GetUntrusted(), epic.GetDescription().GetUntrusted())
	}
	if epic.GetState() != "backlog" || epic.GetArea() != "work" || epic.GetPriority() != "p1" ||
		epic.GetEffectiveArea() != "work" || epic.GetEffectivePriority() != "p1" ||
		epic.GetStartDate() != "2026-11-09" || epic.GetDueDate() != "2026-11-30" || epic.GetEstimate() != 3.5 ||
		epic.GetParentId() != "" || epic.GetOngoing() || epic.GetDeletedAt() != nil || epic.GetRevision() <= 0 ||
		epic.GetCreatedAt() == nil || epic.GetCreatedBy() != itemActor() || epic.GetUpdatedBy() != itemActor() {
		t.Errorf("epic = %v", epic)
	}
	crit := resp.GetCriteria()
	if len(crit) != 2 || crit[0].GetPosition() != 0 || crit[1].GetPosition() != 1 ||
		crit[0].GetText().GetUntrusted() != render.CleanHeader("Spec\tagreed") || crit[0].GetTicked() || !strings.HasPrefix(crit[0].GetId(), "cr_") {
		t.Errorf("criteria = %v", crit)
	}
	task := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Write the spec", ParentId: epic.GetId()}).GetItem()
	if task.GetParentId() != epic.GetId() || task.GetArea() != "" || task.GetEffectiveArea() != "work" ||
		task.GetPriority() != "" || task.GetEffectivePriority() != "p1" || task.GetRevision() <= epic.GetRevision() {
		t.Errorf("task = %v, want it under the epic, inheriting work and p1, at a later revision", task)
	}
	want := []daemontest.AuditRow{
		{Client: "test", Action: "item.create", Targets: itemTargets(t, "item="+epic.GetId(), "type=epic", "criteria=2"), Result: "ok"},
		{Client: "test", Action: "item.create", Targets: itemTargets(t, "item="+task.GetId(), "type=task", "criteria=0"), Result: "ok"},
	}
	if got := itemAudit(t, h); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("audit = %+v, want %+v", got, want)
	}
}

// TestCreateItemIsIdempotent checks that the same key and request return
// the first item as a duplicate with no new revision, that the same key
// with another request is refused, and that a malformed key is refused.
func TestCreateItemIsIdempotent(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	req := &chainmailv1.CreateItemRequest{Type: "task", Title: "Pay the invoice", Area: "personal", IdempotencyKey: "triage:msg-1"}
	first := itemCreate(t, h, req)
	second := itemCreate(t, h, req)
	if !second.GetDuplicate() || first.GetDuplicate() || second.GetItem().GetId() != first.GetItem().GetId() ||
		second.GetItem().GetRevision() != first.GetItem().GetRevision() {
		t.Errorf("first %v, second %v: want the second a duplicate of the first", first, second)
	}
	other := proto.Clone(req).(*chainmailv1.CreateItemRequest)
	other.Title = "Pay another invoice"
	_, err := h.Client.Items.CreateItem(t.Context(), connect.NewRequest(other))
	wantItemErr(t, "reused key", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "use a new idempotency_key for a different item")
	bad := proto.Clone(req).(*chainmailv1.CreateItemRequest)
	bad.IdempotencyKey = "bad key!"
	_, err = h.Client.Items.CreateItem(t.Context(), connect.NewRequest(bad))
	wantItemErr(t, "malformed key", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "pass idempotency_key: 1 to 128 of A-Z, a-z, 0-9 and . _ : -")
	got := itemStrings(t, h, "SELECT decision || '/' || result FROM audit WHERE action = 'item.create' ORDER BY id")
	if want := []string{"/ok", "duplicate/ok", "/error", "/error"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("decisions = %v, want %v", got, want)
	}
}

// TestCreateItemRefusals checks each refusal's code, reason and, where the
// daemon writes it, hint, and that a refused type is audited as invalid.
func TestCreateItemRefusals(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	parentTask := itemTask(t, h, "A task")
	tests := []struct {
		name   string
		req    *chainmailv1.CreateItemRequest
		code   connect.Code
		reason string
		hint   string
	}{
		{name: "unknown type", req: &chainmailv1.CreateItemRequest{Type: "todo", Title: "x", Area: "work"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "no type", req: &chainmailv1.CreateItemRequest{Title: "x", Area: "work"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "unknown state", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", Area: "work", State: "finished"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "unknown area", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", Area: "home"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "unknown priority", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", Area: "work", Priority: "p9"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "not a date", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", Area: "work", DueDate: "2026-02-30"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "negative estimate", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", Area: "work", Estimate: proto.Float64(-1)}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "pass an estimate such as 1.5, or clear it"},
		{name: "malformed parent", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", ParentId: "parent-1"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "item IDs start with it_; run: chainmail item list"},
		{name: "unknown parent", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", ParentId: "it_doesnotexist"}, code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list"},
		{name: "root without an area", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "task under a task", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "x", ParentId: parentTask}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
		{name: "empty title", req: &chainmailv1.CreateItemRequest{Type: "task", Title: "  ", Area: "work"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.Client.Items.CreateItem(t.Context(), connect.NewRequest(tt.req))
			wantItemErr(t, tt.name, err, tt.code, tt.reason, tt.hint)
		})
	}
	rows := itemAudit(t, h)
	if len(rows) != len(tests)+1 {
		t.Fatalf("audit rows = %+v, want one per call", rows)
	}
	for i, tt := range tests {
		r := rows[i+1]
		if r.Action != "item.create" || r.Result != "error" || r.Reason != tt.reason {
			t.Errorf("%s: audit = %+v", tt.name, r)
		}
	}
	if want := itemTargets(t, "type=(invalid)"); rows[1].Targets != want || rows[2].Targets != want {
		t.Errorf("refused types audited as %s and %s, want %s", rows[1].Targets, rows[2].Targets, want)
	}
}
