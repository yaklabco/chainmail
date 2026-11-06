package mcpserver

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// itemAt is the time every faked item carries, and itemAtText how the tools
// show it.
var itemAt = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

const itemAtText = "2026-11-06T10:00:00Z"

// itemAnswer is a faked story under the work Admin item, with text as its
// title and description and every other field set.
func itemAnswer(text string) *chainmailv1.WorkItem {
	ts := timestamppb.New(itemAt)
	return &chainmailv1.WorkItem{
		Id: "it_01jc0story", Type: "story", Title: readU(text), Description: readU(text),
		State: "in-progress", Priority: "p1", EffectiveArea: "work", EffectivePriority: "p1",
		StartDate: "2026-11-01", DueDate: "2026-11-30", Estimate: proto.Float64(1.5), ParentId: "it_admin_work",
		Revision: 12, CreatedAt: ts, UpdatedAt: ts, CreatedBy: "1000/mcp", UpdatedBy: "1000/cli",
	}
}

// itemOut is itemAnswer(text) as the tools shape it. serverName and
// serverNamePtr build the same untrusted envelope item text uses.
func itemOut(text string) workItemOut {
	return workItemOut{
		ID: "it_01jc0story", Type: "story", Title: serverName(text), Description: serverNamePtr(text),
		State: "in-progress", Priority: "p1", EffectiveArea: "work", EffectivePriority: "p1",
		StartDate: "2026-11-01", DueDate: "2026-11-30", Estimate: proto.Float64(1.5), ParentID: "it_admin_work",
		Revision: 12, CreatedAt: itemAtText, UpdatedAt: itemAtText, CreatedBy: "1000/mcp", UpdatedBy: "1000/cli",
	}
}

// criteriaAnswer is a ticked criterion with text and evidence, then an
// unticked one whose evidence is empty.
func criteriaAnswer(text, evidence string) []*chainmailv1.Criterion {
	return []*chainmailv1.Criterion{
		{Id: "cr_01a", Text: readU(text), Ticked: true, Evidence: readU(evidence), TickedAt: timestamppb.New(itemAt), TickedBy: "1000/mcp"},
		{Id: "cr_01b", Position: 1, Text: readU("Receipt filed"), Evidence: readU("")},
	}
}

// criteriaOut is criteriaAnswer as the tools shape it: empty evidence is left out.
func criteriaOut(text, evidence string) []criterionOut {
	return []criterionOut{
		{ID: "cr_01a", Text: serverName(text), Ticked: true, Evidence: serverNamePtr(evidence), TickedAt: itemAtText, TickedBy: "1000/mcp"},
		{ID: "cr_01b", Position: 1, Text: serverName("Receipt filed")},
	}
}

func TestCreateItemRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args map[string]any
		want *chainmailv1.CreateItemRequest
	}{
		{
			name: "every field",
			args: map[string]any{
				"type": "task", "title": "Pay invoice", "description": "From the **Acme** thread", "state": "in-progress",
				"area": "work", "priority": "p1", "start_date": "2026-11-01", "due_date": "2026-11-30", "estimate": 1.5,
				"parent_id": "it_admin_work", "criteria": []string{"Paid", "Receipt filed"}, "idempotency_key": "triage:42",
			},
			want: &chainmailv1.CreateItemRequest{
				Type: "task", Title: "Pay invoice", Description: "From the **Acme** thread", State: "in-progress",
				Area: "work", Priority: "p1", StartDate: "2026-11-01", DueDate: "2026-11-30", Estimate: proto.Float64(1.5),
				ParentId: "it_admin_work", Criteria: []string{"Paid", "Receipt filed"}, IdempotencyKey: "triage:42",
			},
		},
		{
			name: "an inbox task",
			args: map[string]any{"type": "task", "title": "Call Ann", "area": "personal"},
			want: &chainmailv1.CreateItemRequest{Type: "task", Title: "Call Ann", Area: "personal"},
		},
		{
			name: "estimate 0 is sent",
			args: map[string]any{"type": "epic", "title": "Q4", "area": "work", "estimate": 0},
			want: &chainmailv1.CreateItemRequest{Type: "epic", Title: "Q4", Area: "work", Estimate: proto.Float64(0)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest) // unfaked RPCs answer Unimplemented; the request is still recorded
			e.call(t, "create_item", tt.args)
			if got := e.fake.lastCall(t, chainmailv1connect.ItemsServiceCreateItemProcedure).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCreateItemOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		duplicate bool
		summary   string
	}{
		{name: "new", summary: "created story it_01jc0story at revision 12\n"},
		{name: "duplicate", duplicate: true, summary: "story it_01jc0story was made by an earlier call with this idempotency_key; nothing new was created\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeDaemon{CreateItem: func(context.Context, *chainmailv1.CreateItemRequest) (*chainmailv1.CreateItemResponse, error) {
				return &chainmailv1.CreateItemResponse{Item: itemAnswer("Pay invoice"), Criteria: criteriaAnswer("Paid", "Bank ref 991"), Duplicate: tt.duplicate}, nil
			}}
			e := newTestEnv(t, f, latest)
			res := e.call(t, "create_item", map[string]any{"type": "story", "title": "Pay invoice", "idempotency_key": "k"})
			wantOK(t, res)
			var out createItemOut
			decode(t, res, &out)
			want := createItemOut{Item: itemOut("Pay invoice"), Criteria: criteriaOut("Paid", "Bank ref 991"), Duplicate: tt.duplicate}
			if !reflect.DeepEqual(out, want) {
				t.Errorf("create_item =\n%+v\nwant\n%+v", out, want)
			}
			text := textOf(res)
			if !strings.HasPrefix(text, tt.summary) {
				t.Errorf("text = %q, want the summary %q first", text, tt.summary)
			}
			if !strings.Contains(text, untrustedPreamble) || strings.Count(text, "<<<end chainmail-untrusted ") != 1 {
				t.Errorf("text is not wrapped once: %q", text)
			}
		})
	}
}

// itemNoCall fails the test if the fake daemon received any ItemsService call.
func itemNoCall(t *testing.T, f *fakeDaemon) {
	t.Helper()
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.Procedure, "/chainmail.v1.ItemsService/") {
			t.Errorf("the request reached the daemon: %s %v", c.Procedure, c.Msg)
		}
	}
}

func TestUpdateItemRequests(t *testing.T) {
	t.Parallel()
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	tests := []struct {
		name string
		args map[string]any
		want *chainmailv1.UpdateItemRequest
	}{
		{
			name: "title and state",
			args: map[string]any{"id": "it_a", "state": "done", "title": "Paid"},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("title", "state"), Title: "Paid", State: "done"},
		},
		{
			name: "every field, in mask order",
			args: map[string]any{
				"id": "it_a", "parent_id": "it_b", "estimate": 2.5, "due_date": "2026-12-01", "start_date": "2026-11-02",
				"priority": "p0", "area": "personal", "state": "in-review", "description": "d", "title": "t", "type": "feature",
			},
			want: &chainmailv1.UpdateItemRequest{
				Id:         "it_a",
				UpdateMask: mask("type", "title", "description", "state", "area", "priority", "start_date", "due_date", "estimate", "parent_id"),
				Type:       "feature", Title: "t", Description: "d", State: "in-review", Area: "personal", Priority: "p0",
				StartDate: "2026-11-02", DueDate: "2026-12-01", Estimate: proto.Float64(2.5), ParentId: "it_b",
			},
		},
		{
			name: "clear sends the path with no value",
			args: map[string]any{"id": "it_a", "clear": []string{"due_date", "area", "description"}},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("description", "area", "due_date")},
		},
		{
			name: "clearing parent_id moves to the root",
			args: map[string]any{"id": "it_a", "clear": []string{"parent_id"}},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("parent_id")},
		},
		{
			name: "parent_id moves",
			args: map[string]any{"id": "it_a", "parent_id": "it_admin_personal"},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("parent_id"), ParentId: "it_admin_personal"},
		},
		{
			name: "given and cleared fields together",
			args: map[string]any{"id": "it_a", "priority": "p0", "clear": []string{"estimate"}},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("priority", "estimate"), Priority: "p0"},
		},
		{
			name: "estimate 0 is a value",
			args: map[string]any{"id": "it_a", "estimate": 0},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("estimate"), Estimate: proto.Float64(0)},
		},
		{
			name: "an empty string is sent as given",
			args: map[string]any{"id": "it_a", "description": ""},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("description")},
		},
		{
			name: "a repeated clear is named once",
			args: map[string]any{"id": "it_a", "clear": []string{"area", "area"}},
			want: &chainmailv1.UpdateItemRequest{Id: "it_a", UpdateMask: mask("area")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			e.call(t, "update_item", tt.args)
			if got := e.fake.lastCall(t, chainmailv1connect.ItemsServiceUpdateItemProcedure).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateItemRejectsBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      map[string]any
		inMessage string
	}{
		{name: "nothing to change", args: map[string]any{"id": "it_a"}, inMessage: "give at least one field"},
		{name: "an empty clear", args: map[string]any{"id": "it_a", "clear": []string{}}, inMessage: "give at least one field"},
		{name: "an unknown field", args: map[string]any{"id": "it_a", "clear": []string{"colour"}}, inMessage: `clear cannot name "colour"`},
		{name: "title is never cleared", args: map[string]any{"id": "it_a", "clear": []string{"title"}}, inMessage: `clear cannot name "title"`},
		{name: "state is never cleared", args: map[string]any{"id": "it_a", "clear": []string{"state"}}, inMessage: `clear cannot name "state"`},
		{name: "type is never cleared", args: map[string]any{"id": "it_a", "clear": []string{"type"}}, inMessage: `clear cannot name "type"`},
		{name: "given and cleared", args: map[string]any{"id": "it_a", "area": "work", "clear": []string{"area"}}, inMessage: "area is both given and cleared"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			body := wantError(t, e.call(t, "update_item", tt.args), api.ReasonInvalidArgument)
			if !strings.Contains(body.Message, tt.inMessage) {
				t.Errorf("message %q lacks %q", body.Message, tt.inMessage)
			}
			itemNoCall(t, e.fake)
		})
	}
}

func TestUpdateItemOutput(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{UpdateItem: func(context.Context, *chainmailv1.UpdateItemRequest) (*chainmailv1.UpdateItemResponse, error) {
		return &chainmailv1.UpdateItemResponse{Item: itemAnswer("Paid")}, nil
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "update_item", map[string]any{"id": "it_01jc0story", "title": "Paid"})
	wantOK(t, res)
	var out updateItemOut
	decode(t, res, &out)
	if want := (updateItemOut{Item: itemOut("Paid")}); !reflect.DeepEqual(out, want) {
		t.Errorf("update_item =\n%+v\nwant\n%+v", out, want)
	}
	if text := textOf(res); !strings.HasPrefix(text, "updated story it_01jc0story at revision 12\n") {
		t.Errorf("text = %q", text)
	}
}
