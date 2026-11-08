package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

// ruleAt is the time the rule fixtures report.
var ruleAt = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

// The IDs of the two rules the fixtures hold.
const (
	ruleID  = "ru_01jcb7m2k9r8q3v4w5x6y7z8a9"
	ruleID2 = "ru_01jcb7m2k9r8q3v4w5x6y7z8b0"
)

// fakeRules is a RulesService for CLI tests. Each func answers its RPC; a
// nil func answers with an empty response. It records every request, and
// how long each had left before its deadline, 0 for none.
type fakeRules struct {
	chainmailv1connect.UnimplementedRulesServiceHandler

	create  func(*chainmailv1.CreateRuleRequest) (*chainmailv1.CreateRuleResponse, error)
	update  func(*chainmailv1.UpdateRuleRequest) (*chainmailv1.UpdateRuleResponse, error)
	del     func(*chainmailv1.DeleteRuleRequest) (*chainmailv1.DeleteRuleResponse, error)
	restore func(*chainmailv1.RestoreRuleRequest) (*chainmailv1.RestoreRuleResponse, error)
	get     func(*chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error)
	list    func(*chainmailv1.ListRulesRequest) (*chainmailv1.ListRulesResponse, error)
	dryRun  func(*chainmailv1.DryRunRuleRequest) (*chainmailv1.DryRunRuleResponse, error)
	run     func(*chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error)
	runs    func(*chainmailv1.ListRuleRunsRequest) (*chainmailv1.ListRuleRunsResponse, error)
	status  func(*chainmailv1.RulesStatusRequest) (*chainmailv1.RulesStatusResponse, error)

	mu   sync.Mutex
	reqs []proto.Message
	left []time.Duration
}

func (f *fakeRules) record(ctx context.Context, m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, m)
	var left time.Duration
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	f.left = append(f.left, left)
}

// requests returns every request so far, in order.
func (f *fakeRules) requests() []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// timeLeft returns, for every request so far, how long it had before its
// deadline when it arrived, 0 for a request with none.
func (f *fakeRules) timeLeft() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.left)
}

// answerRules records req and answers it with fn, or with an empty
// response when fn is nil.
func answerRules[Req proto.Message, Resp any](ctx context.Context, f *fakeRules, req Req, fn func(Req) (*Resp, error)) (*connect.Response[Resp], error) {
	f.record(ctx, req)
	if fn == nil {
		return connect.NewResponse(new(Resp)), nil
	}
	resp, err := fn(req)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeRules) CreateRule(ctx context.Context, req *connect.Request[chainmailv1.CreateRuleRequest]) (*connect.Response[chainmailv1.CreateRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.create)
}

func (f *fakeRules) UpdateRule(ctx context.Context, req *connect.Request[chainmailv1.UpdateRuleRequest]) (*connect.Response[chainmailv1.UpdateRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.update)
}

func (f *fakeRules) DeleteRule(ctx context.Context, req *connect.Request[chainmailv1.DeleteRuleRequest]) (*connect.Response[chainmailv1.DeleteRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.del)
}

func (f *fakeRules) RestoreRule(ctx context.Context, req *connect.Request[chainmailv1.RestoreRuleRequest]) (*connect.Response[chainmailv1.RestoreRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.restore)
}

func (f *fakeRules) GetRule(ctx context.Context, req *connect.Request[chainmailv1.GetRuleRequest]) (*connect.Response[chainmailv1.GetRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.get)
}

func (f *fakeRules) ListRules(ctx context.Context, req *connect.Request[chainmailv1.ListRulesRequest]) (*connect.Response[chainmailv1.ListRulesResponse], error) {
	return answerRules(ctx, f, req.Msg, f.list)
}

func (f *fakeRules) DryRunRule(ctx context.Context, req *connect.Request[chainmailv1.DryRunRuleRequest]) (*connect.Response[chainmailv1.DryRunRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.dryRun)
}

func (f *fakeRules) RunRule(ctx context.Context, req *connect.Request[chainmailv1.RunRuleRequest]) (*connect.Response[chainmailv1.RunRuleResponse], error) {
	return answerRules(ctx, f, req.Msg, f.run)
}

func (f *fakeRules) ListRuleRuns(ctx context.Context, req *connect.Request[chainmailv1.ListRuleRunsRequest]) (*connect.Response[chainmailv1.ListRuleRunsResponse], error) {
	return answerRules(ctx, f, req.Msg, f.runs)
}

func (f *fakeRules) RulesStatus(ctx context.Context, req *connect.Request[chainmailv1.RulesStatusRequest]) (*connect.Response[chainmailv1.RulesStatusResponse], error) {
	return answerRules(ctx, f, req.Msg, f.status)
}

// startFakeRules serves f on a Unix socket until the test ends and returns the socket.
func startFakeRules(t *testing.T, f *fakeRules) string {
	t.Helper()
	path, handler := chainmailv1connect.NewRulesServiceHandler(f)
	return startFakeServer(t, path, handler)
}

// ruleFixture is a rule on two Google accounts that runs on new mail and
// every six hours: it labels, files the thread against the work Admin
// item, forwards with a note and archives. Its description holds an escape
// sequence a table must not print.
func ruleFixture() *chainmailv1.Rule {
	return &chainmailv1.Rule{
		Id: ruleID, Name: "receipts", Description: ut("Stripe receipts.\n\nFile them under \x1b[31mAdmin.\n"),
		Accounts: []string{"gmail", "gwork"}, Query: ut("from:stripe.com subject:receipt"),
		OnNew: true, Schedule: "every:6h", Priority: 50,
		Actions: []*chainmailv1.RuleAction{
			{Kind: "label", Label: "Receipts"},
			{Kind: "link_item", ItemId: "it_admin_work", Anchor: true},
			{Kind: "forward", To: []string{"books@example.com"}, Note: "for the books"},
			{Kind: "archive"},
		},
		MaxPerRun: 500, Enabled: true, Revision: 3,
		CreatedAt: timestamppb.New(ruleAt), UpdatedAt: timestamppb.New(ruleAt.Add(time.Hour)),
		CreatedBy: "1000/mcp", UpdatedBy: "1000/cli",
		NextRunAt: timestamppb.New(ruleAt.Add(6 * time.Hour)), LastRunAt: timestamppb.New(ruleAt),
	}
}

// ruleRunFixtures are two runs of ruleFixture, newest first: a new-mail run
// that applied one match and held a forward, and a scheduled run the cache
// could not answer, with a hint.
func ruleRunFixtures() []*chainmailv1.RuleRun {
	return []*chainmailv1.RuleRun{
		{
			Id: 12, RuleId: ruleID, Account: "gmail", Trigger: "new",
			StartedAt: timestamppb.New(ruleAt.Add(30 * time.Minute)), FinishedAt: timestamppb.New(ruleAt.Add(30*time.Minute + 2*time.Second)),
			Matched: 2, Applied: 1, Held: 1, Outcome: "ok",
		},
		{
			Id: 11, RuleId: ruleID, Account: "gwork", Trigger: "schedule",
			StartedAt: timestamppb.New(ruleAt), FinishedAt: timestamppb.New(ruleAt.Add(time.Minute)),
			Outcome: "cache_not_ready", Reason: "CACHE_NOT_READY", Hint: "the cache has not caught up; run: chainmail cache status",
		},
	}
}

// ruleShowFixture is rule show's view of ruleFixture: the rule, its two
// runs and its 30-day totals, without revisions.
func ruleShowFixture() *chainmailv1.GetRuleResponse {
	return &chainmailv1.GetRuleResponse{
		Rule: ruleFixture(), Runs: ruleRunFixtures(),
		Counts: &chainmailv1.RuleCounts{Runs: 14, Matched: 40, Applied: 37, Held: 2, Failed: 1},
	}
}

// ruleBareFixture is a deleted, manual-only rule on every account with one
// action, no description and no runs.
func ruleBareFixture() *chainmailv1.Rule {
	return &chainmailv1.Rule{
		Id: "ru_01jcb7m2k9r8q3v4w5x6y7z8c2", Name: "inbox-zero", Query: ut("in:inbox is:read"), Priority: 100,
		Actions: []*chainmailv1.RuleAction{{Kind: "archive"}}, MaxPerRun: 500, Revision: 2,
		CreatedAt: timestamppb.New(ruleAt), UpdatedAt: timestamppb.New(ruleAt), DeletedAt: timestamppb.New(ruleAt.Add(time.Hour)),
		CreatedBy: "1000/cli", UpdatedBy: "1000/cli",
	}
}

// ruleCase is a rule command, run with the arguments after "rule" (or, for
// completion, the whole command line), and what a test expects of it.
type ruleCase struct {
	name  string
	args  []string
	want  proto.Message // the one request it sends
	out   string        // what it prints with table output
	usage string        // text its usage error mentions
	cands []string      // the completion candidates it offers
}

// testRuleRequests runs each case against a fresh fake with JSON output
// and checks that it sent exactly the case's request.
func testRuleRequests(t *testing.T, cases []ruleCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRules{}
			args := append([]string{"--socket", startFakeRules(t, f), "--output", "json", "rule"}, tt.args...)
			got := runCLI(t, Env{}, args...)
			if got.code != 0 {
				t.Fatalf("rule %v: %+v", tt.args, got)
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{tt.want}, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, tt.want)
			}
		})
	}
}

// testRuleUsage runs each case with no daemon and checks that it fails
// with a usage error mentioning the case's usage text.
func testRuleUsage(t *testing.T, cases []ruleCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, append([]string{"--socket", "/nonexistent.sock", "rule"}, tt.args...)...)
			if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
				t.Fatalf("rule %v: %+v, want a usage error", tt.args, got)
			}
			if !strings.Contains(got.stderr, tt.usage) {
				t.Errorf("stderr = %s, want it to mention %q", got.stderr, tt.usage)
			}
		})
	}
}

// testRuleCompletion completes each case's arguments against the fake
// completionRules returns and checks the candidates' values, in order.
func testRuleCompletion(t *testing.T, cases []ruleCase) {
	t.Helper()
	env := sockEnv(startFakeRules(t, completionRules()))
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cands, _ := completeArgs(t, env, tt.args...)
			if got := values(cands); !slices.Equal(got, tt.cands) {
				t.Errorf("__complete %q: %q, want %q", tt.args, got, tt.cands)
			}
		})
	}
}

// completionRules answers ListRules with two live rules, or one deleted
// rule when deleted rules are asked for.
func completionRules() *fakeRules {
	return &fakeRules{list: func(r *chainmailv1.ListRulesRequest) (*chainmailv1.ListRulesResponse, error) {
		if r.GetDeleted() {
			return &chainmailv1.ListRulesResponse{Rules: []*chainmailv1.Rule{
				{Id: "ru_gone", Name: "old-news", DeletedAt: timestamppb.New(ruleAt)},
			}}, nil
		}
		return &chainmailv1.ListRulesResponse{Rules: []*chainmailv1.Rule{
			{Id: ruleID, Name: "receipts", Description: ut("Stripe\x1b[2J receipts\nfiled")},
			{Id: ruleID2, Name: "inbox-zero"},
		}}, nil
	}}
}

func TestRuleShowGolden(t *testing.T) {
	t.Parallel()
	f := &fakeRules{get: func(*chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error) { return ruleShowFixture(), nil }}
	socket := startFakeRules(t, f)
	for _, tt := range []struct {
		golden string
		args   []string
	}{
		{golden: "rule_show_json", args: []string{"--output", "json", "rule", "show", "receipts"}},
		{golden: "rule_show_table", args: []string{"--output", "table", "rule", "show", "receipts"}},
		// --toml writes a rule file whatever --output says.
		{golden: "rule_show_toml", args: []string{"--output", "json", "rule", "show", "receipts", "--toml"}},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, append([]string{"--socket", socket}, tt.args...)...)
			if got.code != 0 {
				t.Fatalf("rule show: %+v", got)
			}
			golden(t, tt.golden, got.stdout)
		})
	}
}

// ruleBareView is ruleViewText's output for ruleBareFixture with no runs.
const ruleBareView = "id           ru_01jcb7m2k9r8q3v4w5x6y7z8c2\n" +
	"name         inbox-zero\n" +
	"state        deleted\n" +
	"accounts     all\n" +
	"query        in:inbox is:read\n" +
	"triggers     manual\n" +
	"priority     100\n" +
	"stop         false\n" +
	"max per run  500\n" +
	"revision     2\n" +
	"failed runs  0\n" +
	"next run     -\n" +
	"last run     -\n" +
	"created      2026-11-07 09:00 by 1000/cli\n" +
	"updated      2026-11-07 09:00 by 1000/cli\n" +
	"deleted      2026-11-07 10:00\n" +
	"\nactions:\n  archive\n" +
	"\nlast 30 days: 0 runs, 0 matched, 0 applied, 0 held, 0 failed\n"

// A rule with no description and no runs prints its fields, its actions
// and zero totals. A rule on every account with no trigger reads "all"
// and "manual".
func TestRuleShowBareRule(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := ruleViewText(&b, time.UTC, &chainmailv1.GetRuleResponse{Rule: ruleBareFixture()}); err != nil {
		t.Fatal(err)
	}
	if b.String() != ruleBareView {
		t.Errorf("ruleViewText =\n%q\nwant\n%q", b.String(), ruleBareView)
	}
}

// --history adds the revisions, newest first, after everything else.
func TestRuleShowHistory(t *testing.T) {
	t.Parallel()
	view := &chainmailv1.GetRuleResponse{Rule: ruleBareFixture(), Revisions: []*chainmailv1.RuleRevision{
		{Revision: 2, Change: "deleted", At: timestamppb.New(ruleAt.Add(time.Hour)), Actor: "1000/cli"},
		{Revision: 1, Change: "created", At: timestamppb.New(ruleAt), Actor: "1000/mcp"},
	}}
	var b strings.Builder
	if err := ruleViewText(&b, time.UTC, view); err != nil {
		t.Fatal(err)
	}
	want := ruleBareView + "\nrevisions:\n" +
		"REVISION  CHANGE   AT                ACTOR\n" +
		"2         deleted  2026-11-07 10:00  1000/cli\n" +
		"1         created  2026-11-07 09:00  1000/mcp\n"
	if b.String() != want {
		t.Errorf("ruleViewText =\n%q\nwant\n%q", b.String(), want)
	}
}

func TestRuleState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rule *chainmailv1.Rule
		want string
	}{
		{name: "enabled", rule: &chainmailv1.Rule{Enabled: true}, want: "enabled"},
		{name: "disabled by hand", rule: &chainmailv1.Rule{}, want: "disabled"},
		{name: "disabled by the runner", rule: &chainmailv1.Rule{DisabledReason: "all_failed"}, want: "disabled (all_failed)"},
		{name: "deleted", rule: &chainmailv1.Rule{Enabled: true, DeletedAt: timestamppb.New(ruleAt)}, want: "deleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ruleState(tt.rule); got != tt.want {
				t.Errorf("ruleState = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuleShowRequestsAndUsage(t *testing.T) {
	t.Parallel()
	testRuleRequests(t, []ruleCase{
		{name: "show by name", args: []string{"show", "receipts"}, want: &chainmailv1.GetRuleRequest{Rule: "receipts"}},
		{name: "show history by ID", args: []string{"show", ruleID, "--history"}, want: &chainmailv1.GetRuleRequest{Rule: ruleID, History: true}},
		{name: "show as a file", args: []string{"show", "receipts", "--toml"}, want: &chainmailv1.GetRuleRequest{Rule: "receipts"}},
	})
	testRuleUsage(t, []ruleCase{
		{name: "show nothing", args: []string{"show"}, usage: "accepts 1 arg(s)"},
		{name: "show as a file with history", args: []string{"show", "receipts", "--toml", "--history"}, usage: "--toml and --history cannot be used together"},
	})
}

// Daemon refusals keep their reason, and so their exit code.
func TestRuleReportsDaemonErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   int
		reason string
	}{
		{name: "not found", err: api.New(connect.CodeNotFound, api.ReasonNotFound, "run: chainmail rule list", errors.New("rule not found")), code: api.ExitNotFound, reason: api.ReasonNotFound},
		{name: "invalid", err: api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "", errors.New("bad rule")), code: api.ExitUsage, reason: api.ReasonInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRules{get: func(*chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error) { return nil, tt.err }}
			got := runCLI(t, Env{}, "--socket", startFakeRules(t, f), "rule", "show", "nope")
			if got.code != tt.code || errorReason(t, got.stderr) != tt.reason {
				t.Errorf("rule show: %+v, want exit %d %s", got, tt.code, tt.reason)
			}
		})
	}
}

func TestCompleteRuleRefs(t *testing.T) {
	t.Parallel()
	testRuleCompletion(t, []ruleCase{
		{name: "show", args: []string{"rule", "show", ""}, cands: []string{"receipts", "inbox-zero"}},
		{name: "show prefix", args: []string{"rule", "show", "in"}, cands: []string{"inbox-zero"}},
		{name: "show takes one", args: []string{"rule", "show", "receipts", ""}},
	})
}

// A rule completion offers live names, each described by its ID and its
// description cleaned to one line, from one ListRules call.
func TestCompleteRuleRefsDescribeEachRule(t *testing.T) {
	t.Parallel()
	f := completionRules()
	cands, _ := completeArgs(t, sockEnv(startFakeRules(t, f)), "rule", "show", "")
	want := []string{"receipts\t" + ruleID + " · Stripe [2J receipts filed", "inbox-zero\t" + ruleID2}
	if !slices.Equal(cands, want) {
		t.Errorf("candidates %q, want %q", cands, want)
	}
	if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{&chainmailv1.ListRulesRequest{}}, proto.Equal) {
		t.Errorf("requests = %v, want one ListRules for live rules", reqs)
	}
}
