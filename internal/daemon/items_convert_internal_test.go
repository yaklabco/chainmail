package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/render"
)

var convertAt = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// TestWorkItemRowProto checks the compact row, with deleted from DeletedAt.
func TestWorkItemRowProto(t *testing.T) {
	t.Parallel()
	r := items.Row{
		Item: items.Item{
			ID: "it_01jbx", Type: items.TypeTask, Title: "Pay\tthe invoice", State: items.StateDone,
			EffectiveArea: items.AreaPersonal, EffectivePriority: items.PriorityP2, DueDate: "2026-12-01",
			ParentID: "it_01jbw", DeletedAt: convertAt, Revision: 12,
		},
		Depth: 2, Children: 3, CriteriaTotal: 4, CriteriaTicked: 1, Links: 5,
	}
	want := &chainmailv1.WorkItemRow{
		Id: "it_01jbx", Type: "task", Title: &chainmailv1.Untrusted{Untrusted: render.CleanHeader("Pay\tthe invoice")},
		State: "done", EffectiveArea: "personal", EffectivePriority: "p2", DueDate: "2026-12-01", ParentId: "it_01jbw",
		Deleted: true, Depth: 2, Children: 3, CriteriaTotal: 4, CriteriaTicked: 1, Links: 5, Revision: 12,
	}
	if got := workItemRowProto(r); !proto.Equal(got, want) {
		t.Errorf("workItemRowProto = %v, want %v", got, want)
	}
	if got := workItemRows([]items.Row{r, r}); len(got) != 2 || !proto.Equal(got[1], want) {
		t.Errorf("workItemRows = %v, want two rows", got)
	}
}

// TestWorkItemProto checks every field of the API form of an item: the
// title wrapped on one line, the description with its line breaks, the
// estimate only when set, and deleted_at set for a deleted item and unset
// while it is live.
func TestWorkItemProto(t *testing.T) {
	t.Parallel()
	created, updated := convertAt.Add(-48*time.Hour), convertAt.Add(-time.Hour)
	deleted := items.Item{
		ID: "it_01jbx", Type: items.TypeStory, Title: "Plan\tthe\nlaunch", Description: "line one\nline\x07 two",
		State: items.StateInProgress, Area: items.AreaWork, Priority: items.PriorityP1,
		EffectiveArea: items.AreaWork, EffectivePriority: items.PriorityP1,
		StartDate: "2026-11-01", DueDate: "2026-12-01", Estimate: new(1.5), ParentID: "it_01jbw",
		DeletedAt: convertAt, Revision: 7, CreatedAt: created, UpdatedAt: updated, CreatedBy: "1000/cli", UpdatedBy: "1000/mcp",
	}
	live := items.Item{ID: "it_admin_work", Type: items.TypeEpic, Title: "Admin", State: items.StateBacklog, EffectiveArea: items.AreaWork, Ongoing: true, Revision: 1, CreatedAt: created, UpdatedAt: created, CreatedBy: "daemon", UpdatedBy: "daemon"}
	tests := []struct {
		name string
		it   items.Item
		want *chainmailv1.WorkItem
	}{
		{
			name: "deleted, every field set",
			it:   deleted,
			want: &chainmailv1.WorkItem{
				Id: "it_01jbx", Type: "story", Title: &chainmailv1.Untrusted{Untrusted: render.CleanHeader("Plan\tthe\nlaunch")},
				Description: &chainmailv1.Untrusted{Untrusted: render.CleanText("line one\nline\x07 two")},
				State:       "in-progress", Area: "work", Priority: "p1", EffectiveArea: "work", EffectivePriority: "p1",
				StartDate: "2026-11-01", DueDate: "2026-12-01", Estimate: proto.Float64(1.5), ParentId: "it_01jbw",
				DeletedAt: timestamppb.New(convertAt), Revision: 7, CreatedAt: timestamppb.New(created), UpdatedAt: timestamppb.New(updated),
				CreatedBy: "1000/cli", UpdatedBy: "1000/mcp",
			},
		},
		{
			name: "live, no estimate",
			it:   live,
			want: &chainmailv1.WorkItem{
				Id: "it_admin_work", Type: "epic", Title: &chainmailv1.Untrusted{Untrusted: "Admin"}, Description: &chainmailv1.Untrusted{},
				State: "backlog", EffectiveArea: "work", Ongoing: true, Revision: 1,
				CreatedAt: timestamppb.New(created), UpdatedAt: timestamppb.New(created), CreatedBy: "daemon", UpdatedBy: "daemon",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := workItemProto(tt.it)
			if !proto.Equal(got, tt.want) {
				t.Errorf("workItemProto = %v, want %v", got, tt.want)
			}
			if (got.Estimate != nil) != (tt.it.Estimate != nil) {
				t.Errorf("estimate set = %t, want %t", got.Estimate != nil, tt.it.Estimate != nil)
			}
			if (got.GetDeletedAt() != nil) != !tt.it.DeletedAt.IsZero() {
				t.Errorf("deleted_at = %v, want it set only for a deleted item", got.GetDeletedAt())
			}
		})
	}
}

// TestCriterionProto checks the API form of a criterion, ticked and not:
// its text on one line, its evidence with its line breaks, and ticked_at
// unset until it is ticked; criteriaProto keeps the order.
func TestCriterionProto(t *testing.T) {
	t.Parallel()
	ticked := items.Criterion{ID: "cr_1", Position: 0, Text: "Tests\npass", Ticked: true, Evidence: "merged in\n#42\x1b[0m", TickedAt: convertAt, TickedBy: "1000/cli"}
	open := items.Criterion{ID: "cr_2", Position: 1, Text: "Docs updated"}
	tests := []struct {
		name string
		c    items.Criterion
		want *chainmailv1.Criterion
	}{
		{
			name: "ticked",
			c:    ticked,
			want: &chainmailv1.Criterion{
				Id: "cr_1", Text: &chainmailv1.Untrusted{Untrusted: render.CleanHeader("Tests\npass")}, Ticked: true,
				Evidence: &chainmailv1.Untrusted{Untrusted: render.CleanText("merged in\n#42\x1b[0m")}, TickedAt: timestamppb.New(convertAt), TickedBy: "1000/cli",
			},
		},
		{
			name: "not ticked",
			c:    open,
			want: &chainmailv1.Criterion{Id: "cr_2", Position: 1, Text: &chainmailv1.Untrusted{Untrusted: "Docs updated"}, Evidence: &chainmailv1.Untrusted{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := criterionProto(tt.c); !proto.Equal(got, tt.want) {
				t.Errorf("criterionProto = %v, want %v", got, tt.want)
			}
		})
	}
	got := criteriaProto([]items.Criterion{open, ticked})
	if len(got) != 2 || got[0].GetId() != "cr_2" || got[1].GetId() != "cr_1" {
		t.Errorf("criteriaProto = %v, want cr_2 then cr_1", got)
	}
}

// TestToConnectErrorItems checks the three item arms: a store refusal is
// INVALID_ARGUMENT with its own hint, a reused key INVALID_ARGUMENT with a
// fixed hint, and a missing item NOT_FOUND pointing at the item list.
func TestToConnectErrorItems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   connect.Code
		reason string
		hint   string
	}{
		{name: "refusal", err: &items.InvalidError{Msg: "a task cannot have children", Hint: "file it under a story or above"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "file it under a story or above"},
		{name: "wrapped refusal", err: fmt.Errorf("updating: %w", &items.InvalidError{Msg: "m", Hint: "h"}), code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "h"},
		{name: "key reused", err: items.ErrKeyMismatch, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "use a new idempotency_key for a different item"},
		{name: "missing", err: fmt.Errorf("%w: item", items.ErrNotFound), code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail item list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toConnectError(tt.err)
			if got.Code() != tt.code || api.Detail(got).GetReason() != tt.reason || api.Detail(got).GetHint() != tt.hint {
				t.Errorf("%v %s %q, want %v %s %q", got.Code(), api.Detail(got).GetReason(), api.Detail(got).GetHint(), tt.code, tt.reason, tt.hint)
			}
		})
	}
}

// TestItemsErrFixesUnexpectedText checks that the store's own refusals pass
// unchanged and anything else reads as fixed text with its cause's reason.
func TestItemsErrFixesUnexpectedText(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	refusal := &items.InvalidError{Msg: "m", Hint: "h"}
	missing := fmt.Errorf("%w: parent", items.ErrNotFound)
	unchanged := []struct {
		name string
		err  error
	}{{"nil", nil}, {"refusal", refusal}, {"missing", missing}, {"key reused", items.ErrKeyMismatch}}
	for _, tt := range unchanged {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := s.itemsErr(tt.err); !errors.Is(got, tt.err) || (tt.err != nil && got.Error() != tt.err.Error()) {
				t.Errorf("itemsErr(%v) = %v, want it unchanged", tt.err, got)
			}
		})
	}
	tests := []struct {
		name   string
		err    error
		reason string
	}{
		{name: "store text", err: errors.New("sqlite: near " + "\"Secret plan\": syntax error"), reason: api.ReasonInternal},
		{name: "timeout", err: fmt.Errorf("listing: %w", context.DeadlineExceeded), reason: api.ReasonTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := s.itemsErr(tt.err)
			if got.Error() != "the item store failed" || api.Detail(toConnectError(got)).GetReason() != tt.reason {
				t.Errorf("itemsErr(%v) = %q reason %s, want fixed text and %s", tt.err, got, api.Detail(toConnectError(got)).GetReason(), tt.reason)
			}
		})
	}
}

// TestItemsErrLogsNoCauseText checks that an unexpected store error is
// logged with fixed text and its type only: item text the cause carries
// never reaches the daemon's log.
func TestItemsErrLogsNoCauseText(t *testing.T) {
	t.Parallel()
	const marker = "Secret plan"
	var logs bytes.Buffer
	s, _ := newTestServerOptions(t, passwordConfig, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	cause := fmt.Errorf("creating: %w", errors.New("sqlite: near \""+marker+"\": syntax error"))
	if got := s.itemsErr(cause); !errors.Is(got, cause) {
		t.Errorf("itemsErr(%v) = %v, want it to keep its cause", cause, got)
	}
	out := logs.String()
	if !strings.Contains(out, "an items store call failed") || !strings.Contains(out, "*fmt.wrapError") {
		t.Errorf("log = %q, want the fixed message and the error type", out)
	}
	if strings.Contains(out, marker) || strings.Contains(out, "syntax error") {
		t.Errorf("log = %q, want no text from the cause", out)
	}
}
