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

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/render"
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
		got := toConnectError(tt.err)
		if got.Code() != tt.code || api.Detail(got).GetReason() != tt.reason || api.Detail(got).GetHint() != tt.hint {
			t.Errorf("%s: %v %s %q, want %v %s %q", tt.name, got.Code(), api.Detail(got).GetReason(), api.Detail(got).GetHint(), tt.code, tt.reason, tt.hint)
		}
	}
}

// TestItemsErrFixesUnexpectedText checks that the store's own refusals pass
// unchanged and anything else reads as fixed text with its cause's reason.
func TestItemsErrFixesUnexpectedText(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	refusal := &items.InvalidError{Msg: "m", Hint: "h"}
	missing := fmt.Errorf("%w: parent", items.ErrNotFound)
	for _, err := range []error{nil, refusal, missing, items.ErrKeyMismatch} {
		if got := s.itemsErr(err); !errors.Is(got, err) || (err != nil && got.Error() != err.Error()) {
			t.Errorf("itemsErr(%v) = %v, want it unchanged", err, got)
		}
	}
	tests := []struct {
		err    error
		reason string
	}{
		{err: errors.New("sqlite: near " + "\"Secret plan\": syntax error"), reason: api.ReasonInternal},
		{err: fmt.Errorf("listing: %w", context.DeadlineExceeded), reason: api.ReasonTimeout},
	}
	for _, tt := range tests {
		got := s.itemsErr(tt.err)
		if got.Error() != "the item store failed" || api.Detail(toConnectError(got)).GetReason() != tt.reason {
			t.Errorf("itemsErr(%v) = %q reason %s, want fixed text and %s", tt.err, got, api.Detail(toConnectError(got)).GetReason(), tt.reason)
		}
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
