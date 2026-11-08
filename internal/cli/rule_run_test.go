package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
)

// ruleDryRunFixture is a dry run over two accounts: on gmail three
// matches, one already acted on, two of them listed (one subject holds an
// escape sequence, one sender has no name), and gwork, which the cache
// cannot answer yet.
func ruleDryRunFixture() *chainmailv1.DryRunRuleResponse {
	return &chainmailv1.DryRunRuleResponse{Accounts: []*chainmailv1.DryRunAccount{
		{
			Account: "gmail", Matched: 3, Skipped: 1,
			Matches: []*chainmailv1.MessageRow{
				{
					Ref: "cm1:gmail:INBOX:3:17", Account: "gmail", Folder: "INBOX", Date: timestamppb.New(ruleAt.Add(-2 * time.Hour)),
					From: []*chainmailv1.Address{{Name: ut("Stripe"), Email: ut("receipts@stripe.com")}}, Subject: ut("Your receipt\x1b[2J #1"),
				},
				{
					Ref: "cm1:gmail:INBOX:3:18", Account: "gmail", Folder: "INBOX", Date: timestamppb.New(ruleAt.Add(-time.Hour)),
					From: []*chainmailv1.Address{{Name: ut(""), Email: ut("receipts@stripe.com")}}, Subject: ut("Your receipt #2"),
				},
			},
			Actions: []*chainmailv1.RuleAction{{Kind: "label", Label: "Receipts"}, {Kind: "archive"}},
		},
		{Account: "gwork", Reason: "backfilling"},
	}}
}

// ruleRunFixture is a manual run on two accounts: one acted on two
// messages, the other has no runner, with a hint.
func ruleRunFixture() *chainmailv1.RunRuleResponse {
	at := timestamppb.New(ruleAt.Add(2 * time.Hour))
	return &chainmailv1.RunRuleResponse{Runs: []*chainmailv1.RuleRun{
		{Id: 15, RuleId: ruleID, Account: "gmail", Trigger: "manual", StartedAt: at, FinishedAt: at, Matched: 2, Applied: 2, Outcome: "ok"},
		{
			Id: 16, RuleId: ruleID, Account: "gwork", Trigger: "manual", StartedAt: at, FinishedAt: at,
			Outcome: "cache_not_ready", Reason: "cache_disabled", Hint: "turn on the account's cache in config.toml",
		},
	}}
}

// ruleRunsFixture is a page of two runs of two rules, with a next page.
func ruleRunsFixture() *chainmailv1.ListRuleRunsResponse {
	return &chainmailv1.ListRuleRunsResponse{
		Runs: []*chainmailv1.RuleRun{
			{
				Id: 14, RuleId: ruleID, Account: "gmail", Trigger: "manual", StartedAt: timestamppb.New(ruleAt.Add(2 * time.Hour)),
				Matched: 120, Applied: 100, Skipped: 20, Outcome: "capped",
			},
			{
				Id: 13, RuleId: ruleID2, Account: "work", Trigger: "schedule", StartedAt: timestamppb.New(ruleAt.Add(time.Hour)),
				Matched: 3, Failed: 3, Outcome: "failed", Reason: "NOT_FOUND", Hint: "run: chainmail folders --account work",
			},
		},
		NextBeforeId: 13,
	}
}

// ruleStatusFixture has a runner in each of three states and both next times.
func ruleStatusFixture() *chainmailv1.RulesStatusResponse {
	return &chainmailv1.RulesStatusResponse{
		Runners: []*chainmailv1.RulesRunnerStatus{
			{Account: "gmail", State: "running", AckedId: 120},
			{Account: "work", State: "waiting_for_cache", AckedId: 118, Lag: 2},
			{Account: "yaklab", State: "cache_disabled", Reason: "cache_disabled"},
		},
		NextScheduledAt: timestamppb.New(ruleAt.Add(6 * time.Hour)),
		NextDueCheckAt:  timestamppb.New(ruleAt.Add(23 * time.Hour)),
	}
}

// ruleRunFake answers dry-run, run, runs and status with the fixtures.
func ruleRunFake() *fakeRules {
	return &fakeRules{
		dryRun: func(*chainmailv1.DryRunRuleRequest) (*chainmailv1.DryRunRuleResponse, error) {
			return ruleDryRunFixture(), nil
		},
		run: func(*chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error) { return ruleRunFixture(), nil },
		runs: func(*chainmailv1.ListRuleRunsRequest) (*chainmailv1.ListRuleRunsResponse, error) {
			return ruleRunsFixture(), nil
		},
		status: func(*chainmailv1.RulesStatusRequest) (*chainmailv1.RulesStatusResponse, error) {
			return ruleStatusFixture(), nil
		},
	}
}

func TestRuleRunRequestsAndUsage(t *testing.T) {
	t.Parallel()
	full := ruleTOML(t, ruleFileBody)
	testRuleRequests(t, []ruleCase{
		{name: "dry-run", args: []string{"dry-run", "receipts"}, want: &chainmailv1.DryRunRuleRequest{Rule: "receipts"}},
		{
			name: "dry-run new mail on one account",
			args: []string{"dry-run", "receipts", "--trigger", "new", "--account", "gmail"},
			want: &chainmailv1.DryRunRuleRequest{Rule: "receipts", Trigger: "new", Account: "gmail"},
		},
		{
			name: "dry-run an unsaved rule",
			args: []string{"dry-run", "-f", full, "--trigger", "schedule"},
			want: &chainmailv1.DryRunRuleRequest{Definition: ruleFileDefinition(), Trigger: "schedule"},
		},
		{name: "run", args: []string{"run", "receipts"}, want: &chainmailv1.RunRuleRequest{Rule: "receipts"}},
		{
			name: "run capped on one account",
			args: []string{"run", "inbox-cleanup", "--max", "100", "--account", "work"},
			want: &chainmailv1.RunRuleRequest{Rule: "inbox-cleanup", Max: 100, Account: "work"},
		},
		{name: "runs", args: []string{"runs"}, want: &chainmailv1.ListRuleRunsRequest{}},
		{
			name: "runs every filter",
			args: []string{"runs", "receipts", "--account", "gmail", "--trigger", "new", "--outcome", "failed", "--limit", "20", "--before-id", "99"},
			want: &chainmailv1.ListRuleRunsRequest{Rule: "receipts", Account: "gmail", Trigger: "new", Outcome: "failed", Limit: 20, BeforeId: 99},
		},
		{name: "status", args: []string{"status"}, want: &chainmailv1.RulesStatusRequest{}},
	})
	testRuleUsage(t, []ruleCase{
		{name: "dry-run nothing", args: []string{"dry-run"}, usage: "give a rule, or an unsaved rule with --file"},
		{name: "dry-run a rule and a file", args: []string{"dry-run", "receipts", "-f", full}, usage: "give a rule or --file, not both"},
		{name: "dry-run two rules", args: []string{"dry-run", "a", "b"}, usage: "accepts between 0 and 1 arg(s)"},
		{name: "dry-run a missing file", args: []string{"dry-run", "-f", "/nonexistent/rule.toml"}, usage: "no such file"},
		{name: "run nothing", args: []string{"run"}, usage: "accepts 1 arg(s)"},
		{name: "run a negative max", args: []string{"run", "receipts", "--max", "-1"}, usage: "--max must not be negative"},
		{name: "runs two rules", args: []string{"runs", "a", "b"}, usage: "accepts between 0 and 1 arg(s)"},
		{name: "status argument", args: []string{"status", "extra"}, usage: "accepts 0 arg(s)"},
	})
}

func TestRuleRunTables(t *testing.T) {
	t.Parallel()
	testRuleTable(t, ruleRunFake, []ruleCase{
		{
			name: "dry-run",
			args: []string{"dry-run", "receipts"},
			out: "gmail: 3 matched, 1 already acted on\n" +
				"actions: label=Receipts archive\n" +
				"matches: 2 of 3\n" +
				"  DATE              FROM                 SUBJECT             REF\n" +
				"  2026-11-07 07:00  Stripe               Your receipt[2J #1  cm1:gmail:INBOX:3:17\n" +
				"  2026-11-07 08:00  receipts@stripe.com  Your receipt #2     cm1:gmail:INBOX:3:18\n" +
				"\n" +
				"gwork: the cache cannot answer completely yet: backfilling\n",
		},
		{
			name: "run",
			args: []string{"run", "receipts"},
			out: "ID  ACCOUNT  TRIGGER  STARTED           OUTCOME          MATCHED  APPLIED  HELD  SKIPPED  FAILED  REASON\n" +
				"15  gmail    manual   2026-11-07 11:00  ok               2        2        0     0        0       -\n" +
				"16  gwork    manual   2026-11-07 11:00  cache_not_ready  0        0        0     0        0       cache_disabled\n" +
				"run 16 hint: turn on the account's cache in config.toml\n",
		},
		{
			name: "runs",
			args: []string{"runs"},
			out: "ID  RULE                           ACCOUNT  TRIGGER   STARTED           OUTCOME  MATCHED  APPLIED  HELD  SKIPPED  FAILED  REASON\n" +
				"14  ru_01jcb7m2k9r8q3v4w5x6y7z8a9  gmail    manual    2026-11-07 11:00  capped   120      100      0     20       0       -\n" +
				"13  ru_01jcb7m2k9r8q3v4w5x6y7z8b0  work     schedule  2026-11-07 10:00  failed   3        0        0     0        3       NOT_FOUND\n" +
				"run 13 hint: run: chainmail folders --account work\n" +
				"older runs: pass --before-id 13\n",
		},
		{
			name: "status",
			args: []string{"status"},
			out: "ACCOUNT  STATE              ACKED  LAG  REASON\n" +
				"gmail    running            120    0    -\n" +
				"work     waiting_for_cache  118    2    -\n" +
				"yaklab   cache_disabled     0      0    cache_disabled\n" +
				"next scheduled run: 2026-11-07 15:00\n" +
				"next due check: 2026-11-08 08:00\n",
		},
	})
}

// With nothing scheduled and no runner, status says so.
func TestRuleStatusWithNothingScheduled(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := ruleStatusTable(&b, time.UTC, &chainmailv1.RulesStatusResponse{}); err != nil {
		t.Fatal(err)
	}
	want := "ACCOUNT  STATE  ACKED  LAG  REASON\nnext scheduled run: none\nnext due check: none\n"
	if b.String() != want {
		t.Errorf("ruleStatusTable = %q, want %q", b.String(), want)
	}
}

// JSON output is the daemon's response as it came.
func TestRuleRunJSONIsTheResponse(t *testing.T) {
	t.Parallel()
	socket := startFakeRules(t, ruleRunFake())
	for _, tt := range []struct {
		args []string
		want proto.Message
	}{
		{args: []string{"dry-run", "receipts"}, want: ruleDryRunFixture()},
		{args: []string{"run", "receipts"}, want: ruleRunFixture()},
		{args: []string{"runs"}, want: ruleRunsFixture()},
		{args: []string{"status"}, want: ruleStatusFixture()},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, append([]string{"--socket", socket, "--output", "json", "rule"}, tt.args...)...)
			if got.code != 0 {
				t.Fatalf("rule %v: %+v", tt.args, got)
			}
			msg := tt.want.ProtoReflect().New().Interface()
			if err := protojson.Unmarshal([]byte(got.stdout), msg); err != nil {
				t.Fatalf("stdout %q: %v", got.stdout, err)
			}
			if !proto.Equal(msg, tt.want) {
				t.Errorf("rule %v printed %v, want %v", tt.args, msg, tt.want)
			}
		})
	}
}

// rule run and rule dry-run give the daemon ten minutes, the time a run may
// take. Other rule commands leave the deadline to the daemon.
func TestRuleRunAndDryRunSetADeadline(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		args     []string
		deadline bool
	}{
		{args: []string{"run", "receipts"}, deadline: true},
		{args: []string{"dry-run", "receipts"}, deadline: true},
		{args: []string{"status"}, deadline: false},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			t.Parallel()
			f := &fakeRules{}
			got := runCLI(t, Env{}, append([]string{"--socket", startFakeRules(t, f), "--output", "json", "rule"}, tt.args...)...)
			if got.code != 0 {
				t.Fatalf("rule %v: %+v", tt.args, got)
			}
			left := f.timeLeft()
			if len(left) != 1 {
				t.Fatalf("%d calls, want 1", len(left))
			}
			switch {
			case tt.deadline && (left[0] <= ruleRunTimeout-time.Minute || left[0] > ruleRunTimeout):
				t.Errorf("rule %v: %v left, want just under %v", tt.args, left[0], ruleRunTimeout)
			case !tt.deadline && left[0] != 0:
				t.Errorf("rule %v: deadline %v away, want none", tt.args, left[0])
			}
		})
	}
}

// A run the cache cannot answer exits 7 with CACHE_NOT_READY.
func TestRuleRunReportsCacheNotReady(t *testing.T) {
	t.Parallel()
	f := &fakeRules{run: func(*chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error) {
		return nil, api.New(connect.CodeFailedPrecondition, api.ReasonCacheNotReady, "the cache has not caught up; run: chainmail cache status", errors.New("not ready"))
	}}
	got := runCLI(t, Env{}, "--socket", startFakeRules(t, f), "rule", "run", "receipts")
	if got.code != api.ExitUnavailable || errorReason(t, got.stderr) != api.ReasonCacheNotReady {
		t.Errorf("rule run: %+v, want exit %d %s", got, api.ExitUnavailable, api.ReasonCacheNotReady)
	}
}

func TestCompleteRuleRun(t *testing.T) {
	t.Parallel()
	testRuleCompletion(t, []ruleCase{
		{name: "dry-run rule", args: []string{"rule", "dry-run", ""}, cands: []string{"receipts", "inbox-zero"}},
		{name: "triggers", args: []string{"rule", "dry-run", "--trigger", ""}, cands: ruleTriggers},
		{name: "outcomes", args: []string{"rule", "runs", "--outcome", "c"}, cands: []string{"capped", "cache_not_ready"}},
		{name: "run rule", args: []string{"rule", "run", "r"}, cands: []string{"receipts"}},
		{name: "runs rule", args: []string{"rule", "runs", "in"}, cands: []string{"inbox-zero"}},
		{name: "status", args: []string{"rule", "status", ""}},
	})
}
