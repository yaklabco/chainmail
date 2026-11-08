package daemon_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/render"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// rpcMarker is rule text that no error, hint, event or audit row may hold.
const rpcMarker = "ZQX-RULE-TEXT-5512"

// rpcConfig has a Google account and a Hostinger one, with the rules time
// zone in UTC so schedules land on the times the tests name.
const rpcConfig = gmailConfig + `
[rules]
timezone = "UTC"
`

// rpcNow is the daemon's clock in the rule tests.
var rpcNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// rpcStart runs a daemon with rpcConfig on rpcNow. Watch and the cache
// stay off, so no account has a rule runner.
func rpcStart(t *testing.T) *daemontest.Harness {
	t.Helper()
	return daemontest.Start(t, rpcConfig, daemontest.Options{Now: func() time.Time { return rpcNow }})
}

func rpcAct(kind string) *chainmailv1.RuleAction { return &chainmailv1.RuleAction{Kind: kind} }

// rpcDef is a definition matching the inbox with the given actions.
func rpcDef(name string, actions ...*chainmailv1.RuleAction) *chainmailv1.RuleDefinition {
	return &chainmailv1.RuleDefinition{Name: name, Query: "in:inbox", Actions: actions}
}

func rpcCreate(t *testing.T, h *daemontest.Harness, d *chainmailv1.RuleDefinition) *chainmailv1.Rule {
	t.Helper()
	resp, err := h.Client.Rules.CreateRule(t.Context(), connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: d}))
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	return resp.Msg.GetRule()
}

// rpcUpdate sends req with a mask of paths.
func rpcUpdate(t *testing.T, h *daemontest.Harness, req *chainmailv1.UpdateRuleRequest, paths ...string) (*chainmailv1.Rule, error) {
	t.Helper()
	req.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
	resp, err := h.Client.Rules.UpdateRule(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRule(), nil
}

// rpcAudit returns the audit rows of action in order.
func rpcAudit(t *testing.T, h *daemontest.Harness, action string) []daemontest.AuditRow {
	t.Helper()
	var out []daemontest.AuditRow
	for _, r := range h.AuditRows(t) {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// rpcNoText fails when err's message or hint holds rpcMarker.
func rpcNoText(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), rpcMarker) || strings.Contains(api.Detail(err).GetHint(), rpcMarker)) {
		t.Errorf("%s: %v (hint %q) holds rule text", what, err, api.Detail(err).GetHint())
	}
}

// rpcMessage checks a Connect error's message, when want is not empty.
func rpcMessage(t *testing.T, what string, err error, want string) {
	t.Helper()
	var cerr *connect.Error
	if want != "" && (!errors.As(err, &cerr) || cerr.Message() != want) {
		t.Errorf("%s: message of %v, want %q", what, err, want)
	}
}

// TestCreateRule checks a rule with every field, one with the defaults,
// one created disabled, and the audit rows.
func TestCreateRule(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	item := itemTask(t, h, "Receipts")
	desc := "Line one\n\tLine two"
	actions := []*chainmailv1.RuleAction{
		{Kind: "label", Label: "Receipts"}, {Kind: "link_item", ItemId: item, Anchor: true}, {Kind: "archive"},
	}
	full := rpcCreate(t, h, &chainmailv1.RuleDefinition{
		Name: "stripe-receipts", Description: desc, Accounts: []string{"gm"}, Query: "from:receipts@stripe.com",
		OnNew: true, Schedule: "every:6h", Priority: proto.Int32(10), Stop: true, MaxPerRun: 50, Actions: actions,
	})
	want := &chainmailv1.Rule{
		Id: full.GetId(), Name: "stripe-receipts", Description: &chainmailv1.Untrusted{Untrusted: render.CleanText(desc)},
		Accounts: []string{"gm"}, Query: &chainmailv1.Untrusted{Untrusted: "from:receipts@stripe.com"},
		OnNew: true, Schedule: "every:6h", Priority: 10, Stop: true, Actions: actions, MaxPerRun: 50, Enabled: true, Revision: 1,
		CreatedAt: timestamppb.New(rpcNow), UpdatedAt: timestamppb.New(rpcNow), CreatedBy: itemActor(), UpdatedBy: itemActor(),
		NextRunAt: timestamppb.New(rpcNow.Add(6 * time.Hour)),
	}
	if !strings.HasPrefix(full.GetId(), "ru_") || !proto.Equal(full, want) {
		t.Errorf("rule = %v, want %v", full, want)
	}
	plain := rpcCreate(t, h, rpcDef("plain", rpcAct("flag")))
	if plain.GetPriority() != 100 || plain.GetMaxPerRun() != 500 || !plain.GetEnabled() || plain.GetSchedule() != "" ||
		plain.GetNextRunAt() != nil || len(plain.GetAccounts()) != 0 || plain.GetOnNew() {
		t.Errorf("defaults = %v, want priority 100, max_per_run 500, enabled, no schedule and every account", plain)
	}
	off := rpcCreate(t, h, &chainmailv1.RuleDefinition{
		Name: "off", Query: "in:inbox", Schedule: "at:07:00", Enabled: proto.Bool(false), Actions: []*chainmailv1.RuleAction{rpcAct("read")},
	})
	if off.GetEnabled() || off.GetNextRunAt() != nil || off.GetSchedule() != "at:07:00" {
		t.Errorf("disabled rule = %v, want enabled false and no next run", off)
	}
	rows := rpcAudit(t, h, "rule.create")
	wantRows := []daemontest.AuditRow{
		{Client: "test", Action: "rule.create", Targets: itemTargets(t, "rule="+full.GetId(), "actions=3"), Result: "ok"},
		{Client: "test", Action: "rule.create", Targets: itemTargets(t, "rule="+plain.GetId(), "actions=1"), Result: "ok"},
		{Client: "test", Action: "rule.create", Targets: itemTargets(t, "rule="+off.GetId(), "actions=1"), Result: "ok"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(wantRows) {
		t.Errorf("audit = %+v, want %+v", rows, wantRows)
	}
}

// TestCreateRuleRefusals checks each refusal's code, its fixed text where
// the daemon writes it, and that no error, hint or audit row holds rule
// text.
func TestCreateRuleRefusals(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	gone := itemTask(t, h, "Gone")
	if _, err := h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: gone})); err != nil {
		t.Fatal(err)
	}
	rpcCreate(t, h, rpcDef("taken", rpcAct("flag")))
	marked := func(d *chainmailv1.RuleDefinition) *chainmailv1.RuleDefinition {
		d.Description = rpcMarker
		return d
	}
	tests := []struct {
		name, msg, hint string
		def             *chainmailv1.RuleDefinition
	}{
		{name: "no definition"},
		{name: "bad name", def: marked(rpcDef("Bad Name", rpcAct("flag")))},
		{
			name: "unknown kind", def: marked(rpcDef("k1", &chainmailv1.RuleAction{Kind: "explode"})), msg: "unknown action kind",
			hint: "action kinds are label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive and trash",
		},
		{name: "bad schedule", def: marked(&chainmailv1.RuleDefinition{Name: "k2", Query: "in:inbox", Schedule: "every:1m", Actions: []*chainmailv1.RuleAction{rpcAct("flag")}})},
		{name: "label off Google", def: marked(&chainmailv1.RuleDefinition{Name: "k3", Accounts: []string{"host"}, Query: "in:inbox", Actions: []*chainmailv1.RuleAction{{Kind: "label", Label: rpcMarker}}})},
		{name: "unknown account", def: marked(&chainmailv1.RuleDefinition{Name: "k4", Accounts: []string{"nope"}, Query: "in:inbox", Actions: []*chainmailv1.RuleAction{rpcAct("flag")}})},
		{
			name: "unknown item", def: marked(rpcDef("k5", &chainmailv1.RuleAction{Kind: "link_item", ItemId: "it_nope"})),
			msg: "a link_item action names an unknown item", hint: "run: chainmail item list",
		},
		{
			name: "deleted item", def: marked(rpcDef("k6", &chainmailv1.RuleAction{Kind: "link_item", ItemId: gone})),
			msg: "a link_item action names a deleted item", hint: "restore the item or name another; run: chainmail item list",
		},
		{name: "duplicate name", def: marked(rpcDef("taken", rpcAct("flag")))},
		{name: "query syntax", def: marked(&chainmailv1.RuleDefinition{Name: "k7", Query: "subject:(" + rpcMarker, Actions: []*chainmailv1.RuleAction{rpcAct("flag")}})},
		{name: "move not last", def: marked(rpcDef("k8", &chainmailv1.RuleAction{Kind: "move", Folder: rpcMarker}, rpcAct("flag")))},
	}
	for _, tt := range tests {
		_, err := h.Client.Rules.CreateRule(t.Context(), connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: tt.def}))
		wantItemErr(t, tt.name, err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, tt.hint)
		rpcMessage(t, tt.name, err, tt.msg)
		rpcNoText(t, tt.name, err)
	}
	rows := rpcAudit(t, h, "rule.create")
	if len(rows) != len(tests)+1 {
		t.Fatalf("%d rule.create rows, want %d", len(rows), len(tests)+1)
	}
	for i, r := range rows[1:] {
		if r.Result != "error" || r.Reason != api.ReasonInvalidArgument || strings.Contains(r.Targets, rpcMarker) {
			t.Errorf("%s: audit row %+v, want an INVALID_ARGUMENT error with no rule text", tests[i].name, r)
		}
	}
}

// TestUpdateRule checks a two field change that ignores a field the mask
// leaves out, a rename with a matching expected revision, a stale one,
// priority back to its default, and the schedule following enabled.
func TestUpdateRule(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	r := rpcCreate(t, h, rpcDef("base", rpcAct("flag")))
	got, err := rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "base", Patch: &chainmailv1.RuleDefinition{
		Name: "ignored", Description: "Newer", Priority: proto.Int32(5),
	}}, "description", "priority")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetName() != "base" || got.GetDescription().GetUntrusted() != "Newer" || got.GetPriority() != 5 || got.GetRevision() != 2 {
		t.Errorf("after update: %v", got)
	}
	got, err = rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: r.GetId(), Patch: &chainmailv1.RuleDefinition{Name: "renamed"}, ExpectedRevision: proto.Int64(2)}, "name")
	if err != nil || got.GetName() != "renamed" || got.GetRevision() != 3 {
		t.Fatalf("rename = %v, %v", got, err)
	}
	_, err = rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "renamed", Patch: &chainmailv1.RuleDefinition{Description: "x"}, ExpectedRevision: proto.Int64(2)}, "description")
	wantItemErr(t, "stale revision", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	if got, err = rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "renamed", Patch: &chainmailv1.RuleDefinition{}}, "priority"); err != nil || got.GetPriority() != 100 {
		t.Errorf("priority reset = %v, %v; want 100", got, err)
	}
	if got, err = rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "renamed", Patch: &chainmailv1.RuleDefinition{Schedule: "every:1h"}}, "schedule"); err != nil ||
		!got.GetNextRunAt().AsTime().Equal(rpcNow.Add(time.Hour)) {
		t.Errorf("schedule = %v, %v; want the next run an hour on", got, err)
	}
	if got, err = rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "renamed", Patch: &chainmailv1.RuleDefinition{}}, "enabled"); err != nil ||
		got.GetEnabled() || got.GetNextRunAt() != nil {
		t.Errorf("disable = %v, %v; want disabled with no next run", got, err)
	}
	if got, err = rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "renamed", Patch: &chainmailv1.RuleDefinition{Enabled: proto.Bool(true)}}, "enabled"); err != nil ||
		!got.GetEnabled() || got.GetDisabledReason() != "" || got.GetNextRunAt() == nil {
		t.Errorf("enable = %v, %v; want enabled with a next run", got, err)
	}
	rows := rpcAudit(t, h, "rule.update")
	if len(rows) != 7 || rows[0].Targets != itemTargets(t, "rule="+r.GetId(), "fields=description,priority", "revision=2") ||
		rows[2].Result != "error" || rows[2].Targets != itemTargets(t, "rule=renamed", "fields=description") {
		t.Errorf("audit = %+v", rows)
	}
}

// TestUpdateRuleRefusals checks the refusals UpdateRule makes before the
// store, and the merged definition's validation.
func TestUpdateRuleRefusals(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	rpcCreate(t, h, rpcDef("base", rpcAct("flag")))
	gone := itemTask(t, h, "Gone")
	if _, err := h.Client.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: gone})); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		req         *chainmailv1.UpdateRuleRequest
		paths       []string
		code        connect.Code
		reason, msg string
		hint        string
		targets     []string
	}{
		{
			name: "bad ref", req: &chainmailv1.UpdateRuleRequest{Rule: "Bad Ref!"}, paths: []string{"description"},
			code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "rule must be a rule ID or name",
			hint: "rule IDs start with ru_; run: chainmail rule list", targets: []string{"rule=(invalid)", "fields=description"},
		},
		{
			name: "unknown rule", req: &chainmailv1.UpdateRuleRequest{Rule: "nope"}, paths: []string{"description"},
			code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: "run: chainmail rule list", targets: []string{"rule=nope", "fields=description"},
		},
		{
			name: "empty mask", req: &chainmailv1.UpdateRuleRequest{Rule: "base"},
			code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "update_mask must name one or more fields", targets: []string{"rule=base", "fields="},
		},
		{
			name: "unknown path", req: &chainmailv1.UpdateRuleRequest{Rule: "base"}, paths: []string{"rule.query"},
			code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "update_mask names an unknown field", targets: []string{"rule=base", "fields=(invalid)"},
		},
		{
			name: "deleted item", req: &chainmailv1.UpdateRuleRequest{Rule: "base", Patch: rpcDef("", &chainmailv1.RuleAction{Kind: "link_item", ItemId: gone})}, paths: []string{"actions"},
			code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "a link_item action names a deleted item", targets: []string{"rule=base", "fields=actions"},
		},
		{
			name: "no actions", req: &chainmailv1.UpdateRuleRequest{Rule: "base", Patch: &chainmailv1.RuleDefinition{}}, paths: []string{"actions"},
			code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, targets: []string{"rule=base", "fields=actions"},
		},
		{
			name: "bad query", req: &chainmailv1.UpdateRuleRequest{Rule: "base", Patch: &chainmailv1.RuleDefinition{Query: "from:(" + rpcMarker}}, paths: []string{"query"},
			code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, targets: []string{"rule=base", "fields=query"},
		},
	}
	for _, tt := range tests {
		_, err := rpcUpdate(t, h, tt.req, tt.paths...)
		wantItemErr(t, tt.name, err, tt.code, tt.reason, tt.hint)
		rpcMessage(t, tt.name, err, tt.msg)
		rpcNoText(t, tt.name, err)
	}
	rows := rpcAudit(t, h, "rule.update")
	for i, tt := range tests {
		if i >= len(rows) || rows[i].Targets != itemTargets(t, tt.targets...) || rows[i].Result != "error" {
			t.Errorf("%s: audit rows %+v, want targets %v", tt.name, rows, tt.targets)
		}
	}
}

// TestDeleteAndRestoreRule checks soft delete by name, a repeat by ID that
// changes nothing, lookups of a deleted rule, restore, a restore of a live
// rule, and the audit rows.
func TestDeleteAndRestoreRule(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	r := rpcCreate(t, h, rpcDef("doomed", rpcAct("trash")))
	del, err := h.Client.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: "doomed"}))
	if err != nil || del.Msg.GetRule().GetDeletedAt() == nil || del.Msg.GetRule().GetRevision() != 2 {
		t.Fatalf("delete = %v, %v", del, err)
	}
	again, err := h.Client.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: r.GetId()}))
	if err != nil || again.Msg.GetRule().GetRevision() != 2 {
		t.Errorf("second delete = %v, %v; want a no-op at revision 2", again, err)
	}
	_, err = h.Client.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: "doomed"}))
	wantItemErr(t, "delete by the deleted name", err, connect.CodeNotFound, api.ReasonNotFound, "run: chainmail rule list")
	res, err := h.Client.Rules.RestoreRule(t.Context(), connect.NewRequest(&chainmailv1.RestoreRuleRequest{Rule: r.GetId()}))
	if err != nil || res.Msg.GetRule().GetDeletedAt() != nil || res.Msg.GetRule().GetRevision() != 3 {
		t.Fatalf("restore = %v, %v", res, err)
	}
	_, err = h.Client.Rules.RestoreRule(t.Context(), connect.NewRequest(&chainmailv1.RestoreRuleRequest{Rule: r.GetId()}))
	wantItemErr(t, "restore a live rule", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "")
	_, err = h.Client.Rules.RestoreRule(t.Context(), connect.NewRequest(&chainmailv1.RestoreRuleRequest{Rule: ""}))
	wantItemErr(t, "restore without a rule", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "rule IDs start with ru_; run: chainmail rule list")
	dels, ress := rpcAudit(t, h, "rule.delete"), rpcAudit(t, h, "rule.restore")
	if len(dels) != 3 || dels[0].Targets != itemTargets(t, "rule="+r.GetId(), "revision=2") ||
		dels[2].Targets != itemTargets(t, "rule=doomed") || dels[2].Reason != api.ReasonNotFound {
		t.Errorf("delete audit = %+v", dels)
	}
	if len(ress) != 3 || ress[0].Targets != itemTargets(t, "rule="+r.GetId(), "revision=3") || ress[2].Targets != itemTargets(t, "rule=(invalid)") {
		t.Errorf("restore audit = %+v", ress)
	}
}

// TestRuleEventsReachWatchers checks that create, update and delete each
// write one rule event a reader naming the kind gets, by ReadEvents and
// Subscribe, with no account and no rule text, and that a reader naming
// no kinds gets none.
func TestRuleEventsReachWatchers(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	r := rpcCreate(t, h, rpcDef("watched", rpcAct("flag")))
	if _, err := rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "watched", Patch: &chainmailv1.RuleDefinition{Description: rpcMarker}}, "description"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Client.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: "watched"})); err != nil {
		t.Fatal(err)
	}
	filter := &chainmailv1.WatchFilter{Kinds: []string{"rule"}}
	var got []string
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Filter: filter}).GetEvents() {
		c := e.GetRule()
		got = append(got, fmt.Sprintf("%s %s %s %d %s account=%q", c.GetRuleId(), c.GetName(), c.GetChange(), c.GetRevision(), c.GetActor(), e.GetAccount()))
		if strings.Contains(e.String(), rpcMarker) {
			t.Errorf("event %v holds rule text", e)
		}
	}
	id, by := r.GetId(), itemActor()
	want := []string{
		id + " watched created 1 " + by + ` account=""`,
		id + " watched updated 2 " + by + ` account=""`,
		id + " watched deleted 3 " + by + ` account=""`,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rule events = %q, want %q", got, want)
	}
	stream := subscribe(t, h, &chainmailv1.SubscribeRequest{Since: proto.Int64(0), Filter: filter})
	if n := len(untilHeartbeat(t, stream).events); n != 3 {
		t.Errorf("Subscribe sent %d rule events, want 3", n)
	}
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0)}).GetEvents() {
		if e.GetKind() == "rule" {
			t.Errorf("a reader with no kinds got %v; rule events are opt-in", e)
		}
	}
}
