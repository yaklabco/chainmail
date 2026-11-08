//go:build live

package live

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
)

// liveRunTimeout is the client deadline of a rule run, as the CLI and MCP
// set it (amendment 18).
const liveRunTimeout = 10 * time.Minute

// TestGoogleLabelRule is the live rule test of spec section 10. On the
// first Google account it sends a message to the account's own address,
// creates a manual-only rule that labels exactly that message with a fresh
// chainmail-test-<random> label, dry-runs it until the cache holds the
// message, runs it with max 1, checks the label and the audit row, runs it
// again to see the act-once table skip the message, and deletes the rule.
// Cleanup expunges the message and deletes the label. Like the other write
// tests it logs IDs, refs and counts only.
//
// Gmail files a message sent to the account's own address with the \Sent
// label, and a rule with no in: term never matches \Sent mail (amendment
// 6), so the rule's query names in:sent.
func TestGoogleLabelRule(t *testing.T) {
	requireWrites(t)
	c := liveClient(t)
	accounts := liveAccounts(t, c)
	i := slices.IndexFunc(accounts, isGoogle)
	if i < 0 {
		t.Skip("no Google account is configured; the label rule test needs one")
	}
	a := accounts[i]
	name := a.GetName()
	waitBudget(t, 0)
	generated := testFolderPrefix + randomToken(12)
	label := generated
	token := "cmlive" + randomToken(16)
	t.Logf("account %s, label %s, token %s", name, label, token)

	// The label must be free before the test claims it, so cleanup can
	// only ever delete the test's own.
	roles := folderRoles(t, c, name)
	if nameTaken(roles, label) {
		t.Fatalf("%s already exists; run the test again for another name", label)
	}
	base := context.WithoutCancel(t.Context())
	var sw *sweeper // set below; the label cleanup re-sweeps with it
	var serverName string
	// Registered before the sweeper, so it runs after the sweep has emptied
	// the label.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(base, cleanupTimeout)
		defer cancel()
		resweep := func(ctx context.Context) error {
			if sw == nil {
				return nil
			}
			_, _, err := sw.expunge(ctx)
			return err
		}
		if err := removeTestFolder(ctx, c, name, generated, serverName, resweep); err != nil {
			t.Errorf("cleanup on %s: %v; delete the label by hand", name, err)
		}
	})
	sw = newSweeper(t, c, a, roles, token)
	created, err := c.Organize.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: name, Name: label}))
	if err != nil {
		t.Fatalf("CreateFolder %s: %v (%s)", label, err, hint(err))
	}
	if got := created.Msg.GetName(); got != "" {
		serverName, label = got, got
	}

	// 1. A message to the account's own address.
	sw.sending(t)
	resp, err := c.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{
		Spec:           selfSpec(a, "chainmail live rule "+token, "A chainmail live rule test. It deletes itself."),
		IdempotencyKey: liveKey(token, "rule"),
	}))
	if err != nil {
		t.Fatalf("Send: %v (%s)", err, hint(err))
	}
	m := sw.waitDelivered(t, sw.sentOf(t, "Send", resp.Msg.GetResult()).GetMessageId())

	// 2. The rule, which acts only on that message, and only when run.
	rule, remove := liveRule(t, c, name, label, token)

	// 3. Dry-run until the cache holds the message.
	waitRuleMatch(t, c, rule.GetId(), name)

	// 4. Run it with max 1.
	run := runLiveRule(t, c, rule.GetId(), name, 1)
	if run.GetMatched() != 1 || run.GetApplied() != 1 || run.GetFailed() != 0 || run.GetOutcome() != "ok" {
		t.Errorf("run %d: outcome %s, matched %d, applied %d, failed %d; want ok with the one message applied", run.GetId(), run.GetOutcome(), run.GetMatched(), run.GetApplied(), run.GetFailed())
	}
	if !slices.Contains(getOne(t, c, m, false).GetLabels(), label) {
		t.Errorf("%s lacks the label %s after the run", m, label)
	}
	checkLiveRuleAudit(t, c, rule.GetId(), run.GetId())

	// 5. A second run skips the message: it is in the act-once table.
	again := runLiveRule(t, c, rule.GetId(), name, 1)
	if again.GetApplied() != 0 || again.GetSkipped() != 1 || again.GetFailed() != 0 || again.GetOutcome() != "ok" {
		t.Errorf("second run %d: outcome %s, applied %d, skipped %d, failed %d; want the message skipped", again.GetId(), again.GetOutcome(), again.GetApplied(), again.GetSkipped(), again.GetFailed())
	}

	// 6. Delete the rule.
	remove(t)
}

// liveRule creates the test's label rule on account, named token, and
// returns it with a function that deletes it and checks the delete. A
// cleanup deletes the rule when the test ends before that function did.
func liveRule(t *testing.T, c *client.Client, account, label, token string) (*chainmailv1.Rule, func(*testing.T)) {
	t.Helper()
	resp, err := c.Rules.CreateRule(t.Context(), connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
		Name: token, Description: "chainmail live rule test " + token, Accounts: []string{account},
		Query:   "in:sent subject:" + token,
		Actions: []*chainmailv1.RuleAction{{Kind: "label", Label: label}},
	}}))
	if err != nil {
		t.Fatalf("CreateRule: %v (%s)", err, hint(err))
	}
	r := resp.Msg.GetRule()
	if r.GetId() == "" || r.GetOnNew() || r.GetSchedule() != "" || !r.GetEnabled() {
		t.Fatalf("CreateRule: id %q, on_new %v, schedule %q, enabled %v; want an enabled manual-only rule", r.GetId(), r.GetOnNew(), r.GetSchedule(), r.GetEnabled())
	}
	id := r.GetId()
	t.Logf("rule %s", id)
	deleted := false
	// t.Context is cancelled before cleanups run, so the cleanup keeps its
	// values but drops the cancellation.
	base := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		if deleted {
			return
		}
		ctx, cancel := context.WithTimeout(base, cleanupTimeout)
		defer cancel()
		if _, err := c.Rules.DeleteRule(ctx, connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: id})); err != nil {
			t.Errorf("cleanup: deleting rule %s: %v (%s); run: chainmail rule delete %s", id, err, hint(err), id)
		}
	})
	remove := func(t *testing.T) {
		t.Helper()
		del, err := c.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: id}))
		if err != nil {
			t.Fatalf("DeleteRule %s: %v (%s)", id, err, hint(err))
		}
		deleted = true
		if del.Msg.GetRule().GetDeletedAt() == nil {
			t.Errorf("rule %s has no deleted_at after DeleteRule", id)
		}
		list, err := c.Rules.ListRules(t.Context(), connect.NewRequest(&chainmailv1.ListRulesRequest{Account: account}))
		if err != nil {
			t.Fatalf("ListRules: %v (%s)", err, hint(err))
		}
		if slices.ContainsFunc(list.Msg.GetRules(), func(r *chainmailv1.Rule) bool { return r.GetId() == id }) {
			t.Errorf("rule %s is still listed after DeleteRule", id)
		}
	}
	return r, remove
}

// waitRuleMatch dry-runs ruleID on account until it matches exactly the
// test's one message with nothing skipped, up to searchTimeout. The cache
// must hold the message and answer completely first. With the account's
// cache off rules cannot run, and the test skips.
func waitRuleMatch(t *testing.T, c *client.Client, ruleID, account string) {
	t.Helper()
	within := waitBudget(t, searchTimeout)
	// The deadline bounds the calls and pauses too, so a slow call cannot
	// carry the wait into the cleanup reserve.
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	last := "no dry run answered"
	for n := 0; ; n++ {
		resp, err := c.Rules.DryRunRule(ctx, connect.NewRequest(&chainmailv1.DryRunRuleRequest{Rule: ruleID, Account: account}))
		if err != nil && ctx.Err() == nil {
			t.Fatalf("DryRunRule %s: %v (%s)", ruleID, err, hint(err))
		}
		if err == nil {
			accts := resp.Msg.GetAccounts()
			if len(accts) != 1 || accts[0].GetAccount() != account {
				t.Fatalf("DryRunRule %s: %d accounts, want only %s", ruleID, len(accts), account)
			}
			d := accts[0]
			if d.GetReason() == "cache_disabled" {
				t.Skipf("the cache is off on %s, and rules match only the cache", account)
			}
			last = fmt.Sprintf("matched %d, skipped %d, %d rows, reason %q", d.GetMatched(), d.GetSkipped(), len(d.GetMatches()), d.GetReason())
			if d.GetReason() == "" && d.GetMatched() == 1 && d.GetSkipped() == 0 && len(d.GetMatches()) == 1 {
				t.Logf("dry run after %d tries: %s", n+1, last)
				return
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("the rule did not match the test's message within %v: %s", within, last)
		}
		if err := pause(ctx, n); err != nil && ctx.Err() == nil {
			t.Fatalf("waiting for the dry run: %v", err)
		}
	}
}

// runLiveRule runs ruleID once on account with at most limit actions, and
// returns the one run it reports.
func runLiveRule(t *testing.T, c *client.Client, ruleID, account string, limit int32) *chainmailv1.RuleRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBudget(t, liveRunTimeout))
	defer cancel()
	resp, err := c.Rules.RunRule(ctx, connect.NewRequest(&chainmailv1.RunRuleRequest{Rule: ruleID, Max: limit, Account: account}))
	if err != nil {
		t.Fatalf("RunRule %s: %v (%s)", ruleID, err, hint(err))
	}
	runs := resp.Msg.GetRuns()
	if len(runs) != 1 || runs[0].GetAccount() != account || runs[0].GetTrigger() != "manual" || runs[0].GetRuleId() != ruleID {
		t.Fatalf("RunRule %s: %d runs, want one manual run on %s", ruleID, len(runs), account)
	}
	r := runs[0]
	t.Logf("run %d: outcome %s, matched %d, applied %d, skipped %d, failed %d, reason %q",
		r.GetId(), r.GetOutcome(), r.GetMatched(), r.GetApplied(), r.GetSkipped(), r.GetFailed(), r.GetReason())
	return r
}

// checkLiveRuleAudit finds the labels row of the run under client kind
// rule, as chainmail audit --client rule shows it, naming the rule and the
// run.
func checkLiveRuleAudit(t *testing.T, c *client.Client, ruleID string, runID int64) {
	t.Helper()
	resp, err := c.Admin.ReadAudit(t.Context(), connect.NewRequest(&chainmailv1.ReadAuditRequest{Client: "rule", Action: "labels", Limit: 1000}))
	if err != nil {
		t.Fatalf("ReadAudit client rule: %v (%s)", err, hint(err))
	}
	for _, e := range resp.Msg.GetEntries() {
		if slices.Contains(e.GetTargets(), "rule="+ruleID) && slices.Contains(e.GetTargets(), "run="+strconv.FormatInt(runID, 10)) {
			if e.GetClient() != "rule" || e.GetResult() != "ok" {
				t.Errorf("labels row %d: client %q, result %q; want rule and ok", e.GetId(), e.GetClient(), e.GetResult())
			}
			return
		}
	}
	t.Errorf("no labels row under client rule names rule %s and run %d", ruleID, runID)
}
