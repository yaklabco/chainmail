package daemon_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
)

// TestGetRule checks a rule by name with no runs, by ID with history
// newest first, the refusals, and the audit rows.
func TestGetRule(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	r := rpcCreate(t, h, &chainmailv1.RuleDefinition{Name: "tidy", Query: "in:inbox", Schedule: "every:6h", Actions: []*chainmailv1.RuleAction{rpcAct("archive")}})
	if _, err := rpcUpdate(t, h, &chainmailv1.UpdateRuleRequest{Rule: "tidy", Patch: &chainmailv1.RuleDefinition{Description: "Tidy up"}}, "description"); err != nil {
		t.Fatal(err)
	}
	resp, err := h.Client.Rules.GetRule(t.Context(), connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: "tidy"}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg
	if got.GetRule().GetRevision() != 2 || got.GetRule().GetDescription().GetUntrusted() != "Tidy up" || len(got.GetRuns()) != 0 ||
		!proto.Equal(got.GetCounts(), &chainmailv1.RuleCounts{}) || len(got.GetRevisions()) != 0 {
		t.Errorf("GetRule = %v", got)
	}
	hist, err := h.Client.Rules.GetRule(t.Context(), connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: r.GetId(), History: true}))
	if err != nil {
		t.Fatal(err)
	}
	revs := hist.Msg.GetRevisions()
	if len(revs) != 2 || revs[0].GetRevision() != 2 || revs[0].GetChange() != "updated" || revs[1].GetRevision() != 1 ||
		revs[1].GetChange() != "created" || revs[1].GetRule().GetDescription().GetUntrusted() != "" || revs[0].GetActor() != itemActor() {
		t.Errorf("revisions = %v, want updated 2 then created 1", revs)
	}
	_, err = h.Client.Rules.GetRule(t.Context(), connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: "Bad Ref!"}))
	wantItemErr(t, "bad ref", err, connect.CodeInvalidArgument, api.ReasonInvalidArgument, "rule IDs start with ru_; run: chainmail rule list")
	_, err = h.Client.Rules.GetRule(t.Context(), connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: "nope"}))
	wantItemErr(t, "unknown", err, connect.CodeNotFound, api.ReasonNotFound, "run: chainmail rule list")
	rows := rpcAudit(t, h, "rule.get")
	if len(rows) != 4 || rows[0].Targets != itemTargets(t, "rule="+r.GetId()) || rows[1].Targets != itemTargets(t, "rule="+r.GetId(), "history=1") ||
		rows[2].Targets != itemTargets(t, "rule=(invalid)") || rows[3].Targets != itemTargets(t, "rule=nope") {
		t.Errorf("audit = %+v", rows)
	}
}

// TestListRules checks the order and each filter, an unknown account, and
// the audit rows.
func TestListRules(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	def := func(name string, priority int32, accounts ...string) *chainmailv1.RuleDefinition {
		d := rpcDef(name, rpcAct("flag"))
		d.Priority, d.Accounts = proto.Int32(priority), accounts
		return d
	}
	rpcCreate(t, h, def("a", 1, "gm"))
	rpcCreate(t, h, def("b", 2))
	c := def("c", 3)
	c.Enabled = proto.Bool(false)
	rpcCreate(t, h, c)
	rpcCreate(t, h, def("d", 4))
	if _, err := h.Client.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: "d"})); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		req  *chainmailv1.ListRulesRequest
		want string
	}{
		{name: "live", req: &chainmailv1.ListRulesRequest{}, want: "a b c"},
		{name: "host", req: &chainmailv1.ListRulesRequest{Account: "host"}, want: "b c"},
		{name: "gm", req: &chainmailv1.ListRulesRequest{Account: "gm"}, want: "a b c"},
		{name: "disabled", req: &chainmailv1.ListRulesRequest{Enabled: proto.Bool(false)}, want: "c"},
		{name: "deleted", req: &chainmailv1.ListRulesRequest{Deleted: true}, want: "d"},
	}
	for _, tt := range tests {
		resp, err := h.Client.Rules.ListRules(t.Context(), connect.NewRequest(tt.req))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		var names []string
		for _, r := range resp.Msg.GetRules() {
			names = append(names, r.GetName())
		}
		if got := strings.Join(names, " "); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
	_, err := h.Client.Rules.ListRules(t.Context(), connect.NewRequest(&chainmailv1.ListRulesRequest{Account: "nope"}))
	wantItemErr(t, "unknown account", err, connect.CodeNotFound, api.ReasonNotFound, "run: chainmail accounts list")
	rows := rpcAudit(t, h, "rule.list")
	if len(rows) != 6 || rows[0].Targets != itemTargets(t, "rows=3") || rows[1].Account != "host" ||
		rows[3].Targets != itemTargets(t, "enabled=false", "rows=1") || rows[4].Targets != itemTargets(t, "deleted=1", "rows=1") || rows[5].Account != "" {
		t.Errorf("audit = %+v", rows)
	}
}
