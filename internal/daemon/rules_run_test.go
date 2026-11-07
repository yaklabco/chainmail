package daemon_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// rpcCacheDisabledHint is the hint of a run on an account with no runner.
const rpcCacheDisabledHint = "the account's cache is off or the account is not synced; run: chainmail cache status"

func rpcRun(t *testing.T, h *daemontest.Harness, req *chainmailv1.RunRuleRequest) []*chainmailv1.RuleRun {
	t.Helper()
	resp, err := h.Client.Rules.RunRule(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}
	return resp.Msg.GetRuns()
}

// TestRunRuleWithoutRunners checks that a manual run on accounts with no
// runner records one cache_not_ready run per covered account, that an
// account filter runs on that one, and the audit rows.
func TestRunRuleWithoutRunners(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	r := rpcCreate(t, h, rpcDef("sweep", rpcAct("archive")))
	var got []string
	for _, run := range rpcRun(t, h, &chainmailv1.RunRuleRequest{Rule: "sweep"}) {
		got = append(got, fmt.Sprintf("%s %s %s %s %s", run.GetRuleId(), run.GetAccount(), run.GetTrigger(), run.GetOutcome(), run.GetReason()))
		if run.GetId() <= 0 || run.GetHint() != rpcCacheDisabledHint || !run.GetStartedAt().AsTime().Equal(rpcNow) || run.GetMatched() != 0 {
			t.Errorf("run = %v", run)
		}
	}
	id := r.GetId()
	if want := []string{id + " gm manual cache_not_ready cache_disabled", id + " host manual cache_not_ready cache_disabled"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("runs = %q, want %q", got, want)
	}
	if runs := rpcRun(t, h, &chainmailv1.RunRuleRequest{Rule: id, Account: "host", Max: 5000}); len(runs) != 1 || runs[0].GetAccount() != "host" {
		t.Errorf("host only = %v", runs)
	}
	rows := rpcAudit(t, h, "rule.run")
	if len(rows) != 2 || rows[0].Targets != itemTargets(t, "rule="+id, "max=0", "runs=2") ||
		rows[1].Account != "host" || rows[1].Targets != itemTargets(t, "rule="+id, "max=5000", "runs=1") {
		t.Errorf("audit = %+v", rows)
	}
	resp, err := h.Client.Rules.GetRule(t.Context(), connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: id}))
	if err != nil {
		t.Fatal(err)
	}
	if runs := resp.Msg.GetRuns(); len(runs) != 3 || runs[0].GetId() <= runs[1].GetId() || !proto.Equal(resp.Msg.GetCounts(), &chainmailv1.RuleCounts{Runs: 3}) {
		t.Errorf("GetRule runs %v counts %v, want 3 runs newest first", runs, resp.Msg.GetCounts())
	}
}

// TestRunRuleRefusals checks RunRule's refusals and their fixed text.
func TestRunRuleRefusals(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	rpcCreate(t, h, rpcDef("sweep", rpcAct("archive")))
	gm := rpcDef("gmonly", rpcAct("flag"))
	gm.Accounts = []string{"gm"}
	rpcCreate(t, h, gm)
	gone := rpcCreate(t, h, rpcDef("gone", rpcAct("flag")))
	if _, err := h.Client.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: "gone"})); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		req    *chainmailv1.RunRuleRequest
		code   connect.Code
		reason string
		msg    string
	}{
		{name: "no rule", req: &chainmailv1.RunRuleRequest{}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "rule must be a rule ID or name"},
		{name: "max below", req: &chainmailv1.RunRuleRequest{Rule: "sweep", Max: -1}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "max must be 0 to 5000"},
		{name: "max above", req: &chainmailv1.RunRuleRequest{Rule: "sweep", Max: 5001}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "max must be 0 to 5000"},
		{name: "unknown account", req: &chainmailv1.RunRuleRequest{Rule: "sweep", Account: "nope"}, code: connect.CodeNotFound, reason: api.ReasonNotFound},
		{name: "not covered", req: &chainmailv1.RunRuleRequest{Rule: "gmonly", Account: "host"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "the rule does not cover that account"},
		{name: "deleted", req: &chainmailv1.RunRuleRequest{Rule: gone.GetId()}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, msg: "the rule is deleted"},
		{name: "unknown rule", req: &chainmailv1.RunRuleRequest{Rule: "nope"}, code: connect.CodeNotFound, reason: api.ReasonNotFound},
	}
	for _, tt := range tests {
		_, err := h.Client.Rules.RunRule(t.Context(), connect.NewRequest(tt.req))
		wantItemErr(t, tt.name, err, tt.code, tt.reason, "")
		rpcMessage(t, tt.name, err, tt.msg)
	}
}

// TestListRuleRuns checks the filters, paging and refusals of
// ListRuleRuns.
func TestListRuleRuns(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	a := rpcCreate(t, h, rpcDef("a", rpcAct("flag")))
	b := rpcDef("b", rpcAct("flag"))
	b.Accounts = []string{"gm"}
	rpcCreate(t, h, b)
	rpcRun(t, h, &chainmailv1.RunRuleRequest{Rule: "a"})
	rpcRun(t, h, &chainmailv1.RunRuleRequest{Rule: "a"})
	rpcRun(t, h, &chainmailv1.RunRuleRequest{Rule: "b"})
	list := func(req *chainmailv1.ListRuleRunsRequest) *chainmailv1.ListRuleRunsResponse {
		t.Helper()
		resp, err := h.Client.Rules.ListRuleRuns(t.Context(), connect.NewRequest(req))
		if err != nil {
			t.Fatalf("ListRuleRuns(%v): %v", req, err)
		}
		return resp.Msg
	}
	if runs := list(&chainmailv1.ListRuleRunsRequest{Rule: "a"}).GetRuns(); len(runs) != 4 || runs[0].GetId() <= runs[3].GetId() {
		t.Errorf("rule a runs = %v, want 4 newest first", runs)
	}
	page := list(&chainmailv1.ListRuleRunsRequest{Rule: a.GetId(), Limit: 3})
	if len(page.GetRuns()) != 3 || page.GetNextBeforeId() == 0 {
		t.Fatalf("first page = %v", page)
	}
	if rest := list(&chainmailv1.ListRuleRunsRequest{Rule: a.GetId(), Limit: 3, BeforeId: page.GetNextBeforeId()}); len(rest.GetRuns()) != 1 || rest.GetNextBeforeId() != 0 {
		t.Errorf("second page = %v", rest)
	}
	counts := []struct {
		name string
		req  *chainmailv1.ListRuleRunsRequest
		want int
	}{
		{name: "gm", req: &chainmailv1.ListRuleRunsRequest{Account: "gm"}, want: 3},
		{name: "ok", req: &chainmailv1.ListRuleRunsRequest{Outcome: "ok"}, want: 0},
		{name: "manual cache_not_ready", req: &chainmailv1.ListRuleRunsRequest{Trigger: "manual", Outcome: "cache_not_ready"}, want: 5},
		{name: "schedule", req: &chainmailv1.ListRuleRunsRequest{Trigger: "schedule"}, want: 0},
	}
	for _, tt := range counts {
		if n := len(list(tt.req).GetRuns()); n != tt.want {
			t.Errorf("%s: %d runs, want %d", tt.name, n, tt.want)
		}
	}
	refusals := []struct {
		name string
		req  *chainmailv1.ListRuleRunsRequest
		code connect.Code
	}{
		{name: "trigger", req: &chainmailv1.ListRuleRunsRequest{Trigger: "later"}, code: connect.CodeInvalidArgument},
		{name: "outcome", req: &chainmailv1.ListRuleRunsRequest{Outcome: "fine"}, code: connect.CodeInvalidArgument},
		{name: "limit above", req: &chainmailv1.ListRuleRunsRequest{Limit: 501}, code: connect.CodeInvalidArgument},
		{name: "limit below", req: &chainmailv1.ListRuleRunsRequest{Limit: -1}, code: connect.CodeInvalidArgument},
		{name: "before_id", req: &chainmailv1.ListRuleRunsRequest{BeforeId: -1}, code: connect.CodeInvalidArgument},
		{name: "bad ref", req: &chainmailv1.ListRuleRunsRequest{Rule: "Bad Ref!"}, code: connect.CodeInvalidArgument},
		{name: "unknown rule", req: &chainmailv1.ListRuleRunsRequest{Rule: "nope"}, code: connect.CodeNotFound},
		{name: "unknown account", req: &chainmailv1.ListRuleRunsRequest{Account: "nope"}, code: connect.CodeNotFound},
	}
	for _, tt := range refusals {
		_, err := h.Client.Rules.ListRuleRuns(t.Context(), connect.NewRequest(tt.req))
		if connect.CodeOf(err) != tt.code {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.code)
		}
	}
	rows := rpcAudit(t, h, "rule.runs")
	if len(rows) < 2 || rows[1].Targets != itemTargets(t, "rule="+a.GetId(), "rows=3") {
		t.Errorf("audit = %+v", rows)
	}
}

// TestDryRunRule checks a saved rule and an unsaved one on accounts the
// cache cannot answer: one entry per account with a reason and no
// matches, and the audit rows.
func TestDryRunRule(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	r := rpcCreate(t, h, rpcDef("preview", rpcAct("flag")))
	resp, err := h.Client.Rules.DryRunRule(t.Context(), connect.NewRequest(&chainmailv1.DryRunRuleRequest{Rule: "preview"}))
	if err != nil {
		t.Fatal(err)
	}
	var accounts []string
	for _, a := range resp.Msg.GetAccounts() {
		accounts = append(accounts, a.GetAccount())
		if a.GetReason() == "" || len(a.GetMatches()) != 0 || a.GetMatched() != 0 {
			t.Errorf("account %v, want a reason and no matches without a cache", a)
		}
	}
	if strings.Join(accounts, " ") != "gm host" {
		t.Errorf("accounts = %v, want gm host", accounts)
	}
	unsaved, err := h.Client.Rules.DryRunRule(t.Context(), connect.NewRequest(&chainmailv1.DryRunRuleRequest{
		Definition: rpcDef("unsaved", rpcAct("flag")), Trigger: "new", Account: "host",
	}))
	if err != nil || len(unsaved.Msg.GetAccounts()) != 1 || unsaved.Msg.GetAccounts()[0].GetAccount() != "host" {
		t.Fatalf("unsaved dry run = %v, %v", unsaved, err)
	}
	rows := rpcAudit(t, h, "rule.dry_run")
	if len(rows) != 2 || rows[0].Targets != itemTargets(t, "rule="+r.GetId(), "trigger=manual", "accounts=2", "matched=0") ||
		rows[1].Account != "host" || rows[1].Targets != itemTargets(t, "rule=unsaved", "trigger=new", "accounts=1", "matched=0") {
		t.Errorf("audit = %+v", rows)
	}
}

// TestDryRunRuleRefusals checks DryRunRule's refusals, and that an invalid
// unsaved definition's text stays out of the error.
func TestDryRunRuleRefusals(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	rpcCreate(t, h, rpcDef("preview", rpcAct("flag")))
	gm := rpcDef("gmonly", rpcAct("flag"))
	gm.Accounts = []string{"gm"}
	rpcCreate(t, h, gm)
	bad := rpcDef("Bad Name", rpcAct("flag"))
	bad.Description = rpcMarker
	tests := []struct {
		name string
		req  *chainmailv1.DryRunRuleRequest
		code connect.Code
		msg  string
	}{
		{name: "neither", req: &chainmailv1.DryRunRuleRequest{}, code: connect.CodeInvalidArgument, msg: "set exactly one of rule and definition"},
		{name: "both", req: &chainmailv1.DryRunRuleRequest{Rule: "preview", Definition: rpcDef("x", rpcAct("flag"))}, code: connect.CodeInvalidArgument, msg: "set exactly one of rule and definition"},
		{name: "trigger", req: &chainmailv1.DryRunRuleRequest{Rule: "preview", Trigger: "later"}, code: connect.CodeInvalidArgument, msg: "trigger must be new, schedule or manual"},
		{name: "unknown rule", req: &chainmailv1.DryRunRuleRequest{Rule: "nope"}, code: connect.CodeNotFound},
		{name: "invalid definition", req: &chainmailv1.DryRunRuleRequest{Definition: bad}, code: connect.CodeInvalidArgument},
		{name: "not covered", req: &chainmailv1.DryRunRuleRequest{Rule: "gmonly", Account: "host"}, code: connect.CodeInvalidArgument, msg: "the rule does not cover that account"},
		{name: "unknown account", req: &chainmailv1.DryRunRuleRequest{Rule: "preview", Account: "nope"}, code: connect.CodeNotFound},
	}
	for _, tt := range tests {
		_, err := h.Client.Rules.DryRunRule(t.Context(), connect.NewRequest(tt.req))
		if connect.CodeOf(err) != tt.code {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.code)
		}
		rpcMessage(t, tt.name, err, tt.msg)
		rpcNoText(t, tt.name, err)
	}
}

// TestRulesStatus checks that every account without a runner reports
// cache_disabled, and the next scheduled run.
func TestRulesStatus(t *testing.T) {
	t.Parallel()
	h := rpcStart(t)
	status := func() *chainmailv1.RulesStatusResponse {
		t.Helper()
		resp, err := h.Client.Rules.RulesStatus(t.Context(), connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg
	}
	first := status()
	want := []*chainmailv1.RulesRunnerStatus{{Account: "gm", State: "cache_disabled"}, {Account: "host", State: "cache_disabled"}}
	if len(first.GetRunners()) != len(want) || !proto.Equal(first.GetRunners()[0], want[0]) || !proto.Equal(first.GetRunners()[1], want[1]) {
		t.Errorf("runners = %v, want %v", first.GetRunners(), want)
	}
	if first.GetNextScheduledAt() != nil {
		t.Errorf("next scheduled = %v with no scheduled rule", first.GetNextScheduledAt())
	}
	rpcCreate(t, h, &chainmailv1.RuleDefinition{Name: "tidy", Query: "in:inbox", Schedule: "every:6h", Actions: []*chainmailv1.RuleAction{rpcAct("archive")}})
	if next := status().GetNextScheduledAt(); next == nil || !next.AsTime().Equal(rpcNow.Add(6*time.Hour)) {
		t.Errorf("next scheduled = %v, want six hours on", next)
	}
	if rows := rpcAudit(t, h, "rule.status"); len(rows) != 2 || rows[0].Targets != itemTargets(t, "runners=2") {
		t.Errorf("audit = %+v", rows)
	}
}
