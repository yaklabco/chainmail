package daemon_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// TestItemTextStaysOutOfErrorsAuditAndLogs sends itemMarker in every text
// field of calls that fail, at each layer that can refuse them, and checks
// that no error message, hint, audit row or daemon log line repeats it.
// The one successful create returns the marker, wrapped, in its response.
func TestItemTextStaysOutOfErrorsAuditAndLogs(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	created := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: itemMarker, Description: itemMarker, Area: "work", Criteria: []string{itemMarker}})
	if created.GetItem().GetTitle().GetUntrusted() != itemMarker {
		t.Fatalf("created title = %q, want the marker back, wrapped", created.GetItem().GetTitle().GetUntrusted())
	}
	id, crit := created.GetItem().GetId(), created.GetCriteria()[0].GetId()
	ctx := t.Context()
	calls := map[string]func() error{
		"create with a bad type": func() error {
			_, err := h.Client.Items.CreateItem(ctx, connect.NewRequest(&chainmailv1.CreateItemRequest{Type: itemMarker, Title: itemMarker, Area: "work"}))
			return err
		},
		"create with an unknown parent": func() error {
			_, err := h.Client.Items.CreateItem(ctx, connect.NewRequest(&chainmailv1.CreateItemRequest{Type: "task", Title: itemMarker, Description: itemMarker, ParentId: "it_missing"}))
			return err
		},
		"update with an unknown path": func() error {
			_, err := h.Client.Items.UpdateItem(ctx, connect.NewRequest(&chainmailv1.UpdateItemRequest{Id: id, Title: itemMarker, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", itemMarker}}}))
			return err
		},
		"done with an unticked criterion": func() error {
			_, err := h.Client.Items.UpdateItem(ctx, connect.NewRequest(&chainmailv1.UpdateItemRequest{Id: id, State: "done", Title: itemMarker, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state", "title"}}}))
			return err
		},
		"edit an unknown criterion": func() error {
			_, err := h.Client.Items.EditCriteria(ctx, connect.NewRequest(&chainmailv1.EditCriteriaRequest{ItemId: id, Edit: []*chainmailv1.CriterionText{{Id: "cr_missing", Text: itemMarker}}, Add: []string{itemMarker}}))
			return err
		},
		"tick with evidence too long": func() error {
			_, err := h.Client.Items.TickCriterion(ctx, connect.NewRequest(&chainmailv1.TickCriterionRequest{ItemId: id, CriterionId: crit, Ticked: true, Evidence: strings.Repeat("e", 1001) + itemMarker}))
			return err
		},
		"list with a bad filter": func() error {
			_, err := h.Client.Items.ListItems(ctx, connect.NewRequest(&chainmailv1.ListItemsRequest{Type: itemMarker, Under: itemMarker, PageToken: itemMarker}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Errorf("%s: no error", name)
				return
			}
			if text := err.Error() + " " + api.Detail(err).GetHint(); strings.Contains(text, itemMarker) {
				t.Errorf("%s: the error repeats item text: %s", name, text)
			}
		})
	}
	for _, r := range h.AuditRows(t) {
		if strings.Contains(r.Targets, itemMarker) || strings.Contains(r.Reason, itemMarker) {
			t.Errorf("audit row %+v repeats item text", r)
		}
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), itemMarker) {
		t.Errorf("the daemon logged item text:\n%s", logs.String())
	}
}
