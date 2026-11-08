//go:build integration

package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/dovecot"
	"github.com/yaklabco/chainmail/internal/testutil/fakegmail"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
)

// itDoveConfig is one account on Dovecot with the default sync set: INBOX
// watched with IDLE and the rest of a default search's folders polled
// every 30 seconds, so the cache can answer a rule completely. The cache
// backfills in small batches without pausing. %[3]d is the SMTP port, 1
// for a step that sends nothing. Only *@allowed.test, and the account's
// own address, may be sent to.
const itDoveConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = %[3]d, security = "starttls" }
sync = { poll = "30s" }

[policy]
send_allow = ["*@allowed.test"]

[cache]
backfill_batch = 10
backfill_pause = "0s"
`

// itGmailConfig adds an account on the fake Gmail server, logging in with
// a password as the gmail preset allows. It watches All Mail, as every
// Gmail preset does. Its SMTP endpoint is never dialled.
const itGmailConfig = `
[accounts.gm]
provider = "gmail"
address = %[1]q
auth = "password"
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = 1, security = "starttls" }
`

const (
	// itGmailPassword is the fake Gmail account's password.
	itGmailPassword = "pw"
	// itRunWithin bounds a rule's run on new mail: the new event, the
	// cache's header fetch, the wait for it and the actions.
	itRunWithin = 30 * time.Second
	// itPollWithin covers a polled folder's next pass, 30 seconds apart,
	// and what follows it: a reconnect, the builder's apply, a runner's
	// batch.
	itPollWithin = 75 * time.Second
	// itCacheWithin bounds the backfill of a test mailbox, including the
	// first pass of each polled folder.
	itCacheWithin = 90 * time.Second
	// itQuiet is how long a check that nothing more happens watches.
	itQuiet = 3 * time.Second
	// itHoldRelease is how long the restart step lets a stop wait on a held
	// SMTP reply before it lets the reply go.
	itHoldRelease = 2 * time.Second
	// itPoll spaces the polls of the wait helpers.
	itPoll = 50 * time.Millisecond
)

// itDoveFolders are the folders the cache tracks on Dovecot with the
// default sync set, sorted: every folder a default search covers.
var itDoveFolders = []string{"Archive", "Drafts", "INBOX", "Sent"}

// itGmailFolders are the folders the cache tracks on a Gmail preset.
var itGmailFolders = []string{"[Gmail]/All Mail"}

// itClock is the daemon's wall clock in the rule tests: real time plus an
// offset the test moves. Timers and ages run as usual, and a jump stands
// for time that passed while nothing happened.
type itClock struct{ offset atomic.Int64 }

// Now is the clock's time.
func (c *itClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

// Set makes the clock read at now, running on from there.
func (c *itClock) Set(at time.Time) { c.offset.Store(int64(time.Until(at))) }

// Advance moves the clock forward by d.
func (c *itClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

// itMail is a message from from to to with subject and Message-ID
// <id@example.org>, dated date.
func itMail(from, to, subject, id string, date time.Time) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@example.org>\r\nDate: %s\r\n\r\nRule test mail.\r\n",
		from, to, subject, id, date.Format(time.RFC1123Z))
}

// itNear says a and b are within a second of each other. A store write
// can read the clock more than once.
func itNear(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Second && d < time.Second
}

// itPause waits itPoll, or reports false when ctx ends first.
func itPause(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(itPoll):
		return true
	}
}

// itDoveOptions are the harness options of a step's daemon: watching, with
// the cache, trusting Dovecot's CA (the fake Gmail server and the SMTP sink
// use it too), with the dove account's password and any more secrets. A
// nil now keeps the real clock.
func itDoveOptions(dove *dovecot.Server, now func() time.Time, secrets map[string]string) daemontest.Options {
	all := map[string]string{"accounts/dove": dovecot.Password}
	maps.Copy(all, secrets)
	return daemontest.Options{
		RootCAs: dove.CA.Pool, Watch: true, Cache: true, WatchHeartbeat: doveHeartbeat, Now: now, Secrets: all,
	}
}

// TestRulesAgainstDovecot runs rules end to end (spec section 10,
// integration) on one Dovecot container. Each step has a Dovecot user and
// a daemon of its own, so a failing step leaves the others valid.
func TestRulesAgainstDovecot(t *testing.T) {
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	t.Run("on new mail", func(t *testing.T) { checkItOnNew(t, dove) })
	t.Run("held forward", func(t *testing.T) { checkItHeldForward(t, dove) })
	t.Run("restart mid batch", func(t *testing.T) { checkItRestart(t, dove) })
	t.Run("schedule", func(t *testing.T) { checkItSchedule(t, dove) })
}

// itWaitCache polls GetCacheStatus until account's cache is ready and
// tracks exactly folders, each backfilled with its window fetched.
func itWaitCache(t *testing.T, h *daemontest.Harness, account string, folders []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), itCacheWithin)
	defer cancel()
	var last *chainmailv1.GetCacheStatusResponse
	for {
		resp, err := h.Client.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: []string{account}}))
		if err != nil {
			t.Fatalf("the cache of %s is not ready within %v: GetCacheStatus: %v; last status %v", account, itCacheWithin, err, last)
		}
		last = resp.Msg
		for _, a := range resp.Msg.GetAccounts() {
			if a.GetAccount() != account || a.GetState() != "ready" {
				continue
			}
			var names []string
			done := true
			for _, f := range a.GetFolders() {
				names = append(names, f.GetFolder())
				done = done && f.GetBackfillDone() && f.GetWindowDone()
			}
			slices.Sort(names)
			if done && slices.Equal(names, folders) {
				return
			}
		}
		if !itPause(ctx) {
			t.Fatalf("the cache of %s is not ready within %v: %v", account, itCacheWithin, last)
		}
	}
}

// itWaitCacheComplete polls a forced cache search of q until account's
// cache answers it completely, as a scheduled run needs (spec 5.3).
func itWaitCacheComplete(t *testing.T, h *daemontest.Harness, account, q string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), itPollWithin)
	defer cancel()
	reason := ""
	for {
		resp, err := h.Client.Message.Search(ctx, connect.NewRequest(&chainmailv1.SearchRequest{
			QueryString: q, Accounts: []string{account}, Limit: 1, Source: "cache",
		}))
		if err != nil {
			t.Fatalf("the cache of %s cannot answer %q within %v: %v; last reason %q", account, q, itPollWithin, err, reason)
		}
		for _, ac := range resp.Msg.GetAccountCounts() {
			if ac.GetAccount() != account {
				continue
			}
			if ac.GetComplete() {
				return
			}
			reason = ac.GetIncompleteReason()
		}
		if !itPause(ctx) {
			t.Fatalf("the cache of %s cannot answer %q within %v: last reason %q", account, q, itPollWithin, reason)
		}
	}
}

// itRulesStatus reads RulesStatus.
func itRulesStatus(t *testing.T, h *daemontest.Harness) *chainmailv1.RulesStatusResponse {
	t.Helper()
	resp, err := h.Client.Rules.RulesStatus(t.Context(), connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
	if err != nil {
		t.Fatalf("RulesStatus: %v", err)
	}
	return resp.Msg
}

// itWaitRunners polls RulesStatus until the runner of each account is idle
// with its group open, so mail delivered afterwards reaches it as new. A
// runner reports starting, with acked ID -1, until it has opened its group
// (amendment 8), so idle with an acked ID of 0 or more says it is open.
func itWaitRunners(t *testing.T, h *daemontest.Harness, accounts ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), doveReadyWithin)
	defer cancel()
	var last []*chainmailv1.RulesRunnerStatus
	for {
		resp, err := h.Client.Rules.RulesStatus(ctx, connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
		if err != nil {
			t.Fatalf("RulesStatus: %v (last runners %v)", err, last)
		}
		last = resp.Msg.GetRunners()
		idle := map[string]bool{}
		for _, r := range last {
			idle[r.GetAccount()] = r.GetState() == "idle" && r.GetAckedId() >= 0
		}
		if !slices.ContainsFunc(accounts, func(a string) bool { return !idle[a] }) {
			return
		}
		if !itPause(ctx) {
			t.Fatalf("the rule runners of %v are not idle within %v: %v", accounts, doveReadyWithin, last)
		}
	}
}

// itWaitAcked polls RulesStatus until account's runner is idle and has
// acknowledged event id, so every batch up to it has been handled.
func itWaitAcked(t *testing.T, h *daemontest.Harness, account string, id int64, within time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	var last []*chainmailv1.RulesRunnerStatus
	for {
		resp, err := h.Client.Rules.RulesStatus(ctx, connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
		if err != nil {
			t.Fatalf("RulesStatus: %v (last runners %v)", err, last)
		}
		last = resp.Msg.GetRunners()
		for _, r := range last {
			if r.GetAccount() == account && r.GetState() == "idle" && r.GetAckedId() >= id {
				return
			}
		}
		if !itPause(ctx) {
			t.Fatalf("the rule runner of %s has not acknowledged event %d within %v: %v", account, id, within, last)
		}
	}
}

// itCreateRule creates a rule and checks it comes back enabled at revision 1.
func itCreateRule(t *testing.T, h *daemontest.Harness, d *chainmailv1.RuleDefinition) *chainmailv1.Rule {
	t.Helper()
	resp, err := h.Client.Rules.CreateRule(t.Context(), connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: d}))
	if err != nil {
		t.Fatalf("CreateRule %s: %v", d.GetName(), err)
	}
	r := resp.Msg.GetRule()
	if !strings.HasPrefix(r.GetId(), "ru_") || r.GetName() != d.GetName() || !r.GetEnabled() || r.GetRevision() != 1 || r.GetCreatedBy() == "" {
		t.Fatalf("CreateRule %s = %v, want an enabled rule at revision 1", d.GetName(), r)
	}
	return r
}

// itUpdateRule applies patch to rule under the mask of paths.
func itUpdateRule(t *testing.T, h *daemontest.Harness, rule string, patch *chainmailv1.RuleDefinition, paths ...string) *chainmailv1.Rule {
	t.Helper()
	resp, err := h.Client.Rules.UpdateRule(t.Context(), connect.NewRequest(&chainmailv1.UpdateRuleRequest{
		Rule: rule, Patch: patch, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	}))
	if err != nil {
		t.Fatalf("UpdateRule %s %v: %v", rule, paths, err)
	}
	return resp.Msg.GetRule()
}

// itGetRule reads rule.
func itGetRule(t *testing.T, h *daemontest.Harness, rule string) *chainmailv1.Rule {
	t.Helper()
	resp, err := h.Client.Rules.GetRule(t.Context(), connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: rule}))
	if err != nil {
		t.Fatalf("GetRule %s: %v", rule, err)
	}
	return resp.Msg.GetRule()
}

// itRuns lists rule's runs on account for trigger, newest first, including
// one still under way, which reads as interrupted.
func itRuns(t *testing.T, h *daemontest.Harness, rule, account, trigger string) []*chainmailv1.RuleRun {
	t.Helper()
	resp, err := h.Client.Rules.ListRuleRuns(t.Context(), connect.NewRequest(&chainmailv1.ListRuleRunsRequest{
		Rule: rule, Account: account, Trigger: trigger, Limit: 500,
	}))
	if err != nil {
		t.Fatalf("ListRuleRuns %s: %v", rule, err)
	}
	return resp.Msg.GetRuns()
}

// itWaitRuns polls ListRuleRuns until rule has at least n finished runs on
// account for trigger and returns the finished ones, newest first. A run
// under way, or left by a daemon that stopped, reads as interrupted and is
// not counted; itQuietRuns counts it.
func itWaitRuns(t *testing.T, h *daemontest.Harness, rule, account, trigger string, n int, within time.Duration) []*chainmailv1.RuleRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	var last []*chainmailv1.RuleRun
	for {
		resp, err := h.Client.Rules.ListRuleRuns(ctx, connect.NewRequest(&chainmailv1.ListRuleRunsRequest{
			Rule: rule, Account: account, Trigger: trigger, Limit: 500,
		}))
		if err != nil {
			t.Fatalf("ListRuleRuns %s: %v (last runs %v)", rule, err, last)
		}
		last = resp.Msg.GetRuns()
		finished := slices.DeleteFunc(slices.Clone(last), func(r *chainmailv1.RuleRun) bool { return r.GetOutcome() == "interrupted" })
		if len(finished) >= n {
			return finished
		}
		if !itPause(ctx) {
			t.Fatalf("%d finished %s runs of %s on %s within %v, want %d: %v", len(finished), trigger, rule, account, within, n, last)
		}
	}
}

// itQuietRuns watches rule's runs on account for trigger for within and
// fails the test as soon as their number is not n. Unlike itWaitRuns, it
// counts every run, one under way included, so a run that has only just
// started is already one too many. The failure says how many of the runs
// read as interrupted, which is how a run under way reads.
func itQuietRuns(t *testing.T, h *daemontest.Harness, rule, account, trigger string, n int, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(itPoll) {
		if runs := itRuns(t, h, rule, account, trigger); len(runs) != n {
			under := 0
			for _, r := range runs {
				if r.GetOutcome() == "interrupted" {
					under++
				}
			}
			t.Fatalf("%s has %d %s runs on %s, %d of them under way or interrupted, want %d: %v",
				rule, len(runs), trigger, account, under, n, runs)
		}
	}
}

// itApplied reads rule_applied for ruleID straight from state.db, since no
// RPC returns it: each message key's outcome and note as "outcome/note".
func itApplied(t *testing.T, h *daemontest.Harness, ruleID string) map[string]string {
	t.Helper()
	rows, err := h.DB.QueryContext(t.Context(), "SELECT message_key, outcome, note FROM rule_applied WHERE rule_id = ?", ruleID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any iteration error
	out := map[string]string{}
	for rows.Next() {
		var key, outcome, note string
		if err := rows.Scan(&key, &outcome, &note); err != nil {
			t.Fatal(err)
		}
		out[key] = outcome + "/" + note
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// itWaitApplied polls rule_applied until ruleID has n rows, every one with
// outcome applied, and returns them.
func itWaitApplied(t *testing.T, h *daemontest.Harness, ruleID string, n int, within time.Duration) map[string]string {
	t.Helper()
	for deadline := time.Now().Add(within); ; time.Sleep(itPoll) {
		rows := itApplied(t, h, ruleID)
		done := len(rows) == n
		for _, o := range rows {
			done = done && strings.HasPrefix(o, "applied/")
		}
		if done {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("act-once rows of %s = %v after %v, want %d, every one applied", ruleID, rows, within, n)
		}
	}
}

// itRuleAudit returns the audit rows of action ("" for every action) under
// client kind rule, newest first, read through ReadAudit's client filter as
// chainmail audit --client rule reads them. Every row must be the rule
// client's.
func itRuleAudit(t *testing.T, h *daemontest.Harness, action string) []*chainmailv1.AuditEntry {
	t.Helper()
	resp, err := h.Client.Admin.ReadAudit(t.Context(), connect.NewRequest(&chainmailv1.ReadAuditRequest{Client: "rule", Action: action, Limit: 1000}))
	if err != nil {
		t.Fatalf("ReadAudit client rule, action %q: %v", action, err)
	}
	for _, e := range resp.Msg.GetEntries() {
		if e.GetClient() != "rule" {
			t.Errorf("ReadAudit with client rule returned a row of client %q: %v", e.GetClient(), e)
		}
	}
	return resp.Msg.GetEntries()
}

// itOfRule keeps the rows whose targets name ruleID.
func itOfRule(entries []*chainmailv1.AuditEntry, ruleID string) []*chainmailv1.AuditEntry {
	return slices.DeleteFunc(slices.Clone(entries), func(e *chainmailv1.AuditEntry) bool {
		return !slices.Contains(e.GetTargets(), "rule="+ruleID)
	})
}

// itCheckRuleRow fails the test unless e is a successful row of one of
// ruleID's actions in run runID on account: client rule, the daemon's own
// UID (amendment 7), and the rule and run IDs among its targets.
func itCheckRuleRow(t *testing.T, e *chainmailv1.AuditEntry, ruleID string, runID int64, account string) {
	t.Helper()
	if e.GetClient() != "rule" || e.GetCallerUid() != uint32(os.Getuid()) || e.GetAccount() != account || e.GetResult() != "ok" {
		t.Errorf("audit row %v, want client rule, caller uid %d, account %s and result ok", e, os.Getuid(), account)
	}
	for _, want := range []string{"rule=" + ruleID, "run=" + strconv.FormatInt(runID, 10)} {
		if !slices.Contains(e.GetTargets(), want) {
			t.Errorf("audit row %s targets %q, want %s among them", e.GetAction(), e.GetTargets(), want)
		}
	}
}

// itNoRuleText fails the test for every audit row that holds one of
// texts: rule descriptions, queries, forward addresses and notes never
// reach the audit log (Global Constraints).
func itNoRuleText(t *testing.T, h *daemontest.Harness, texts ...string) {
	t.Helper()
	for _, row := range h.AuditRows(t) {
		line := fmt.Sprintf("%+v", row)
		for _, s := range texts {
			if strings.Contains(line, s) {
				t.Errorf("audit row %s holds rule text %q", line, s)
			}
		}
	}
}

// itServerRows searches account on the mail server, never the cache, and
// returns the rows.
func itServerRows(t *testing.T, h *daemontest.Harness, account, q string) []*chainmailv1.MessageRow {
	t.Helper()
	resp, err := h.Client.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{
		QueryString: q, Accounts: []string{account}, Limit: 50, Source: "server",
	}))
	if err != nil {
		t.Fatalf("Search %q on %s: %v", q, account, err)
	}
	if p := resp.Msg.GetPartial(); len(p) > 0 {
		t.Fatalf("Search %q on %s could not read %v", q, account, p)
	}
	return resp.Msg.GetRows()
}

// itLogPosition returns the log's position now: every event up to it has
// been written.
func itLogPosition(t *testing.T, h *daemontest.Harness) int64 {
	t.Helper()
	w := subscribeDove(t, h, &chainmailv1.SubscribeRequest{Filter: &chainmailv1.WatchFilter{Kinds: []string{"new"}}})
	defer w.close()
	return w.heartbeat(doveReadyWithin).GetPosition()
}

// checkItOnNew is done when item 1 on Dovecot and the fake Gmail server. A
// receipts rule on each account acts on a delivered message within
// seconds: on Gmail it labels it, links its thread to one task and
// archives it; on Dovecot, where label is refused (amendment 4), it links
// and archives. Each action is audited under client kind rule with the rule
// and run IDs, and chainmail audit --client rule shows exactly those rows.
// The act-once keys follow amendment 3. The archived copy that later
// reaches the polled Archive folder is not acted on again, because the move
// carried the act-once row to its new key (amendment 6).
func checkItOnNew(t *testing.T, dove *dovecot.Server) {
	const (
		user        = "rules-new@example.test"
		gmUser      = "rules@gmail.example"
		query       = "from:receipts@stripe.example"
		description = "Stripe receipts go to the receipts task"
		subject     = "Your receipt from Stripe"
	)
	old := time.Now().Add(-24 * time.Hour)
	gm := fakegmail.Start(t, dove.CA, gmUser, itGmailPassword, fakegmail.Message{
		Raw: itMail("Ann <ann@example.org>", gmUser, "Lunch", "gmlunch", old), Date: old, Labels: []string{`\Inbox`},
	})
	if _, err := dove.Append(user, "INBOX", itMail("Ann <ann@example.org>", user, "Lunch", "dvlunch", old), old); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(itDoveConfig, user, dove.Endpoint.Port, 1) + fmt.Sprintf(itGmailConfig, gmUser, gm.Endpoint.Port)
	h := daemontest.Start(t, cfg, itDoveOptions(dove, nil, map[string]string{"accounts/gm": itGmailPassword}))
	itWaitCache(t, h, "dove", itDoveFolders)
	itWaitCache(t, h, "gm", itGmailFolders)
	task := createDoveTask(t, h, "File the Stripe receipts")

	// label is Google only, so a rule that would label on Dovecot is
	// refused, with fixed text that holds none of the rule's.
	_, err := h.Client.Rules.CreateRule(t.Context(), connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
		Name: "dove-labels", Accounts: []string{"dove"}, Query: query, OnNew: true,
		Actions: []*chainmailv1.RuleAction{{Kind: "label", Label: "Receipts"}},
	}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), "Receipts") || strings.Contains(err.Error(), "stripe") {
		t.Errorf("CreateRule labelling on Dovecot = %v, want INVALID_ARGUMENT with no rule text", err)
	}

	gmRule := itCreateRule(t, h, &chainmailv1.RuleDefinition{
		Name: "gm-receipts", Description: description, Accounts: []string{"gm"}, Query: query, OnNew: true,
		Actions: []*chainmailv1.RuleAction{
			{Kind: "label", Label: "Receipts"},
			{Kind: "link_item", ItemId: task, Anchor: true},
			{Kind: "archive"},
		},
	})
	doveRule := itCreateRule(t, h, &chainmailv1.RuleDefinition{
		Name: "dove-receipts", Description: description, Accounts: []string{"dove"}, Query: query, OnNew: true,
		Actions: []*chainmailv1.RuleAction{
			{Kind: "link_item", ItemId: task, Anchor: true},
			{Kind: "archive"},
		},
	})
	itWaitRunners(t, h, "dove", "gm")
	pos := itLogPosition(t, h)

	// 1. A receipt arrives on each account, from another mail client.
	at := time.Now()
	uid, err := dove.Append(user, "INBOX", itMail("Stripe <receipts@stripe.example>", user, subject, "dvreceipt", at), at)
	if err != nil {
		t.Fatal(err)
	}
	delivered := doveRef(t, dove, user, "INBOX", uint32(uid))
	gmIdx := gm.Deliver(itMail("Stripe <receipts@stripe.example>", gmUser, subject, "gmreceipt", at), at)

	// 2. Each rule runs once on new mail and acts on its message.
	runIDs := map[string]int64{}
	for account, rule := range map[string]*chainmailv1.Rule{"gm": gmRule, "dove": doveRule} {
		runs := itWaitRuns(t, h, rule.GetId(), account, "new", 1, itRunWithin)
		r := runs[0]
		if len(runs) != 1 || r.GetRuleId() != rule.GetId() || r.GetMatched() != 1 || r.GetApplied() != 1 ||
			r.GetSkipped() != 0 || r.GetFailed() != 0 || r.GetOutcome() != "ok" {
			t.Errorf("%s runs = %v, want one new-mail run that matched and applied 1", rule.GetName(), runs)
		}
		runIDs[account] = r.GetId()
	}

	// 3. The mail is where the rules put it.
	if labels := gm.Labels(gmIdx); !slices.Contains(labels, "Receipts") || slices.Contains(labels, `\Inbox`) {
		t.Errorf("the Gmail receipt's labels = %q, want Receipts and no \\Inbox", labels)
	}
	if rows := itServerRows(t, h, "dove", "in:inbox "+query); len(rows) != 0 {
		t.Errorf("Dovecot's INBOX still holds %d receipts, want 0", len(rows))
	}
	archived := itServerRows(t, h, "dove", "in:archive "+query)
	if len(archived) != 1 || archived[0].GetSubject().GetUntrusted() != subject {
		t.Fatalf("Dovecot's Archive holds %v, want the one receipt", archived)
	}
	view, err := h.Client.Items.GetItem(t.Context(), connect.NewRequest(&chainmailv1.GetItemRequest{Id: task}))
	if err != nil {
		t.Fatalf("GetItem %s: %v", task, err)
	}
	var linked []string
	for _, th := range view.Msg.GetThreads() {
		linked = append(linked, th.GetAccount())
		if th.GetThreadKey() == "" {
			t.Errorf("linked thread on %s has no key: %v", th.GetAccount(), th)
		}
	}
	slices.Sort(linked)
	if !slices.Equal(linked, []string{"dove", "gm"}) || view.Msg.GetThreadsTotal() != 2 {
		t.Errorf("the task's threads are on %q (total %d), want one on dove and one on gm", linked, view.Msg.GetThreadsTotal())
	}

	// 4. The act-once keys: g:<X-GM-MSGID> on Gmail, and on Dovecot the
	// INBOX key and the Archive alias the move's new ref gave.
	gmKey := "g:" + strconv.FormatUint(gm.MsgID(gmIdx), 10)
	if got := itApplied(t, h, gmRule.GetId()); !maps.Equal(got, map[string]string{gmKey: "applied/"}) {
		t.Errorf("gm-receipts act-once rows = %v, want only %s applied", got, gmKey)
	}
	in, out := parseRef(t, delivered), parseRef(t, archived[0].GetRef())
	inKey := fmt.Sprintf("i:%d:%d:%s", in.UIDValidity, in.UID, in.Folder)
	outKey := fmt.Sprintf("i:%d:%d:%s", out.UIDValidity, out.UID, out.Folder)
	doveRows := itApplied(t, h, doveRule.GetId())
	if keys := slices.Sorted(maps.Keys(doveRows)); !slices.Equal(keys, slices.Sorted(slices.Values([]string{inKey, outKey}))) {
		t.Errorf("dove-receipts act-once keys = %q, want %s and its alias %s", keys, inKey, outKey)
	}
	for k, o := range doveRows {
		if !strings.HasPrefix(o, "applied/") {
			t.Errorf("dove-receipts act-once row %s = %s, want applied", k, o)
		}
	}

	// 5. chainmail audit --client rule shows each action once, in order,
	// with the rule and run IDs, and only those rows.
	all := itRuleAudit(t, h, "")
	for account, c := range map[string]struct {
		rule    *chainmailv1.Rule
		actions []string
	}{
		"gm":   {gmRule, []string{"labels", "item.link", "archive"}},
		"dove": {doveRule, []string{"item.link", "archive"}},
	} {
		var got []string
		for _, e := range slices.Backward(itOfRule(all, c.rule.GetId())) {
			got = append(got, e.GetAction())
			itCheckRuleRow(t, e, c.rule.GetId(), runIDs[account], account)
		}
		if !slices.Equal(got, c.actions) {
			t.Errorf("%s audit actions under client rule = %q, want %q", c.rule.GetName(), got, c.actions)
		}
	}
	if len(all) != 5 {
		t.Errorf("%d audit rows under client rule, want the 5 actions of the two runs: %v", len(all), all)
	}
	creates := 0
	for _, row := range h.AuditRows(t) {
		if row.Action == "rule.create" && row.Client == "test" && row.Result == "ok" {
			creates++
		}
		if row.Client == "rule" && strings.HasPrefix(row.Action, "rule.") {
			t.Errorf("rule change %+v audited under client rule, want the caller's client", row)
		}
	}
	if creates != 2 {
		t.Errorf("%d successful rule.create rows under client test, want 2", creates)
	}
	itNoRuleText(t, h, description, "receipts@stripe.example", "Receipts")

	// 6. The archived copy reaches the polled Archive folder as a new
	// message. The runner takes it in a batch and does nothing: its key
	// already has the alias row.
	w := subscribeDove(t, h, &chainmailv1.SubscribeRequest{
		Since: &pos, Filter: &chainmailv1.WatchFilter{Accounts: []string{"dove"}, Kinds: []string{"new"}},
	})
	w.next("new", doveReadyWithin, func(ev *chainmailv1.WatchEvent) bool { return ev.GetRef() == delivered })
	moved := w.next("new", itPollWithin, func(ev *chainmailv1.WatchEvent) bool { return ev.GetRef() == archived[0].GetRef() })
	w.close()
	itWaitAcked(t, h, "dove", moved.GetId(), itRunWithin)
	if rows := itOfRule(itRuleAudit(t, h, ""), doveRule.GetId()); len(rows) != 2 {
		t.Errorf("after the archived copy synced, %d rule audit rows for dove-receipts, want the 2 of the first run: %v", len(rows), rows)
	}
	for _, r := range itRuns(t, h, doveRule.GetId(), "dove", "new") {
		if r.GetId() != runIDs["dove"] && r.GetApplied() != 0 {
			t.Errorf("dove-receipts run %d applied %d after the first run, want 0: %v", r.GetId(), r.GetApplied(), r)
		}
	}
}

// checkItHeldForward is done when item 3. A rule that forwards new mail to
// an address off the allowlist leaves a draft in Drafts and sends nothing.
// The run counts the message as held, the act-once row says held, and the
// forward's audit row under client rule has decision held. No row holds
// the address or the note.
func checkItHeldForward(t *testing.T, dove *dovecot.Server) {
	const (
		user    = "rules-held@example.test"
		note    = "Forwarded by the alerts rule"
		subject = "Disk almost full"
	)
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: user, Password: dovecot.Password})
	h := daemontest.Start(t, fmt.Sprintf(itDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port), itDoveOptions(dove, nil, nil))
	itWaitCache(t, h, "dove", itDoveFolders)
	rule := itCreateRule(t, h, &chainmailv1.RuleDefinition{
		Name: "dove-alerts", Accounts: []string{"dove"}, Query: "from:alerts@vendor.example", OnNew: true,
		Actions: []*chainmailv1.RuleAction{{Kind: "forward", To: []string{"ops@blocked.test"}, Note: note}},
	})
	itWaitRunners(t, h, "dove")

	at := time.Now()
	if _, err := dove.Append(user, "INBOX", itMail("Vendor <alerts@vendor.example>", user, subject, "alert1", at), at); err != nil {
		t.Fatal(err)
	}
	run := itWaitRuns(t, h, rule.GetId(), "dove", "new", 1, itRunWithin)[0]
	// A held forward counts only as held (amendment 9).
	if run.GetMatched() != 1 || run.GetHeld() != 1 || run.GetApplied() != 0 || run.GetFailed() != 0 || run.GetOutcome() != "ok" {
		t.Errorf("run = %v, want one match, held and not applied, none failed, outcome ok", run)
	}
	if msgs := sink.Messages(); len(msgs) != 0 {
		t.Fatalf("SMTP got %d messages, want none: the forward is held", len(msgs))
	}
	drafts := itServerRows(t, h, "dove", "in:drafts")
	if len(drafts) != 1 || !strings.Contains(drafts[0].GetSubject().GetUntrusted(), subject) {
		t.Errorf("Drafts holds %v, want the held forward of %q", drafts, subject)
	}
	applied := itApplied(t, h, rule.GetId())
	if len(applied) != 1 {
		t.Errorf("act-once rows = %v, want one", applied)
	}
	for key, o := range applied {
		if o != "held/" || !strings.HasPrefix(key, "i:") || !strings.HasSuffix(key, ":INBOX") {
			t.Errorf("act-once row %s = %s, want held for an INBOX key", key, o)
		}
	}
	fwd := itOfRule(itRuleAudit(t, h, "forward"), rule.GetId())
	if len(fwd) != 1 {
		t.Fatalf("%d forward rows under client rule, want 1: %v", len(fwd), fwd)
	}
	if fwd[0].GetDecision() != "held" {
		t.Errorf("forward row decision %q, want held", fwd[0].GetDecision())
	}
	itCheckRuleRow(t, fwd[0], rule.GetId(), run.GetId(), "dove")
	itNoRuleText(t, h, "ops@blocked.test", note, "alerts@vendor.example")
}

// itSMTPHold holds the reply to the first message the SMTP sink captures
// after it is armed, until free is called.
type itSMTPHold struct {
	armed    atomic.Bool
	captured chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newItSMTPHold() *itSMTPHold {
	return &itSMTPHold{captured: make(chan struct{}), release: make(chan struct{})}
}

// onMessage is the sink's OnMessage hook. It runs once the message is
// captured, before the client gets its reply.
func (h *itSMTPHold) onMessage(smtptest.Envelope) {
	if h.armed.CompareAndSwap(true, false) {
		close(h.captured)
		<-h.release
	}
}

// free lets a held reply go. Later calls do nothing.
func (h *itSMTPHold) free() { h.once.Do(func() { close(h.release) }) }

// itForwardKeys reads the idempotency store's keys of ruleID's forwards on
// account straight from state.db, since no RPC returns them: each key's
// state.
func itForwardKeys(t *testing.T, h *daemontest.Harness, account, ruleID string) map[string]string {
	t.Helper()
	rows, err := h.DB.QueryContext(t.Context(), "SELECT key, state FROM idempotency WHERE account = ? AND key LIKE ?",
		account, "rule."+ruleID+".%")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any iteration error
	out := map[string]string{}
	for rows.Next() {
		var key, state string
		if err := rows.Scan(&key, &state); err != nil {
			t.Fatal(err)
		}
		out[key] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// itForwardOf is the subject line of the page a captured forward carries:
// one of subjects, or "" when it carries none of them.
func itForwardOf(e smtptest.Envelope, subjects []string) string {
	for _, s := range subjects {
		if bytes.Contains(e.Data, []byte("Subject: Fwd: "+s+"\r\n")) {
			return s
		}
	}
	return ""
}

// checkItRestart is done when item 5 and spec amendment 8's forward crash
// safety. Two pages arrive while the daemon is down, so the first pass
// after a restart reports both and the runner takes them in one batch. The
// first forward reaches SMTP and the daemon is stopped while SMTP holds its
// reply: the message has gone, but the runner has not heard so. Its
// act-once row stays pending, which the stop leaves as it is, and the
// other page is never tried. After another restart the pending row is
// replayed under the same idempotency key: the store answers it as a
// duplicate, nothing is sent again, and the row is recorded applied with
// note replayed. The other page is forwarded afresh. The send is detached
// from the request, so the stop waits for it (see the plan's contract
// notes): the test lets the reply go once the stop is under way.
func checkItRestart(t *testing.T, dove *dovecot.Server) {
	const (
		user = "rules-restart@example.test"
		to   = "boss@allowed.test"
		note = "Escalated by the pager rule"
	)
	subjects := []string{"Page one", "Page two"}
	hold := newItSMTPHold()
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{OnMessage: hold.onMessage},
		smtptest.User{Username: user, Password: dovecot.Password})
	// Registered after the sink, so it runs first: a held reply is let go
	// before the sink closes.
	t.Cleanup(hold.free)
	cfg := fmt.Sprintf(itDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port)
	opts := itDoveOptions(dove, nil, nil)
	h1 := daemontest.Start(t, cfg, opts)
	itWaitCache(t, h1, "dove", itDoveFolders)
	rule := itCreateRule(t, h1, &chainmailv1.RuleDefinition{
		Name: "dove-escalate", Accounts: []string{"dove"}, Query: "from:pager@vendor.example", OnNew: true,
		Actions: []*chainmailv1.RuleAction{{Kind: "forward", To: []string{to}, Note: note}},
	})
	itWaitRunners(t, h1, "dove")
	if err := h1.Stop(); err != nil {
		t.Fatalf("stopping the first daemon: %v", err)
	}

	at := time.Now()
	for i, s := range subjects {
		date := at.Add(time.Duration(i) * time.Second)
		if _, err := dove.Append(user, "INBOX", itMail("Pager <pager@vendor.example>", user, s, fmt.Sprintf("page%d", i), date), date); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Restart with SMTP holding the first reply, and stop mid batch.
	hold.armed.Store(true)
	second := opts
	second.Reuse = h1
	h2 := daemontest.Start(t, cfg, second)
	select {
	case <-hold.captured:
	case <-time.After(itPollWithin):
		t.Fatalf("no forward reached SMTP within %v of the restart", itPollWithin)
	}
	// While SMTP holds the reply, the held page's act-once row is pending:
	// it was written before the send.
	pending := itApplied(t, h2, rule.GetId())
	if len(pending) != 1 {
		t.Fatalf("act-once rows while SMTP holds the first forward = %v, want one pending row", pending)
	}
	heldKey := slices.Collect(maps.Keys(pending))[0]
	if pending[heldKey] != "pending/" {
		t.Fatalf("act-once row of the held page = %q, want pending", pending[heldKey])
	}
	captured := sink.Messages()
	if len(captured) != 1 {
		t.Fatalf("SMTP captured %d forwards while holding the first, want 1", len(captured))
	}
	held := itForwardOf(captured[0], subjects)
	if held == "" {
		t.Fatalf("the held forward carries none of %q", subjects)
	}
	// A graceful stop waits for the send in flight, whose context is
	// detached from the request: it must not return while SMTP holds the
	// reply.
	stopped := make(chan error, 1)
	go func() { stopped <- h2.Stop() }()
	select {
	case err := <-stopped:
		t.Errorf("the stop returned (%v) while SMTP held a forward's reply, want it to wait for the send", err)
		stopped <- err
	case <-time.After(itHoldRelease):
	}
	hold.free()
	if stopErr := <-stopped; stopErr != nil {
		t.Fatalf("stopping the daemon mid batch: %v", stopErr)
	}
	// The send finished and settled its idempotency key, but the stopped
	// runner did not record it: the held page's row is still pending, and
	// the other page was never tried.
	if n := len(sink.Messages()); n != 1 {
		t.Fatalf("%d forwards on SMTP after the stop mid batch, want exactly the held one", n)
	}
	if got := itApplied(t, h2, rule.GetId()); !maps.Equal(got, map[string]string{heldKey: "pending/"}) {
		t.Fatalf("act-once rows after the stop mid batch = %v, want only %s pending", got, heldKey)
	}
	heldFwdKey := rules.ForwardKey(rule.GetId(), "dove", heldKey)
	if got := itForwardKeys(t, h2, "dove", rule.GetId()); !maps.Equal(got, map[string]string{heldFwdKey: "done"}) {
		t.Fatalf("forward idempotency keys after the stop mid batch = %v, want only %s done", got, heldFwdKey)
	}

	// 2. Restart again: the pending row is replayed under the same key,
	// and nothing is sent twice.
	third := opts
	third.Reuse = h2
	h3 := daemontest.Start(t, cfg, third)
	applied := itWaitApplied(t, h3, rule.GetId(), len(subjects), itPollWithin)
	itWaitRunners(t, h3, "dove")
	t.Logf("act-once rows after the replay: %v", applied)
	if got := applied[heldKey]; got != string(rules.AppliedDone)+"/"+rules.NoteReplayed {
		t.Errorf("act-once row of the held page after the replay = %q, want applied/%s", got, rules.NoteReplayed)
	}
	for key, got := range applied {
		if key != heldKey && got != "applied/" {
			t.Errorf("act-once row of the other page = %q, want applied with no note", got)
		}
	}
	keys := itForwardKeys(t, h3, "dove", rule.GetId())
	if len(keys) != len(subjects) || keys[heldFwdKey] != "done" {
		t.Errorf("forward idempotency keys after the replay = %v, want %s and one more, each done", keys, heldFwdKey)
	}
	for k, state := range keys {
		if state != "done" {
			t.Errorf("forward idempotency key %s = %s, want done", k, state)
		}
	}
	msgs := sink.Messages()
	if len(msgs) != len(subjects) {
		t.Errorf("SMTP got %d forwards in all, want %d, one per page", len(msgs), len(subjects))
	}
	if len(msgs) > 0 && itForwardOf(msgs[0], subjects) != held {
		t.Errorf("first forward on SMTP is of %q, want the held %q", itForwardOf(msgs[0], subjects), held)
	}
	for _, s := range subjects {
		n := 0
		for _, m := range msgs {
			if itForwardOf(m, subjects) == s {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q was forwarded %d times, want once", s, n)
		}
	}
	for _, m := range msgs {
		if !slices.Equal(m.To, []string{to}) {
			t.Errorf("forward went to %q, want only %s", m.To, to)
		}
	}
	// The replayed forward answers from the idempotency store and is
	// audited as a duplicate, so exactly one row per page is an allowed
	// send, and the held page has the one duplicate.
	allowed, duplicate := 0, 0
	for _, e := range itOfRule(itRuleAudit(t, h3, "forward"), rule.GetId()) {
		switch e.GetDecision() {
		case "allowed":
			allowed++
		case "duplicate":
			duplicate++
		}
	}
	if allowed != len(subjects) || duplicate != 1 {
		t.Errorf("forward rows under client rule: %d allowed and %d duplicate, want %d and 1", allowed, duplicate, len(subjects))
	}
	itNoRuleText(t, h3, to, note, "pager@vendor.example")
}

// checkItSchedule is spec section 10's scheduled rule on the fake clock
// and the downtime rule of section 6. A rule that flags the digests every
// hour has its next run an hour after it is made. The clock jumps an hour
// and a minute; the scheduler, asleep on a real timer, is woken by a rule
// change, finds the rule due on the moved clock and runs it once. Another
// change does not run it again. Then the daemon is down for five hours:
// the restart runs the rule once, not five times, and plans the next run
// an hour after it. A catch-up that meets the cache before its first sync
// pass is retried five minutes later (amendment 7), once.
func checkItSchedule(t *testing.T, dove *dovecot.Server) {
	const (
		user   = "rules-sched@example.test"
		digest = "Digest <digest@vendor.example>"
		query  = "in:inbox from:digest@vendor.example"
	)
	now := time.Now()
	day := 24 * time.Hour
	for i, m := range []struct{ from, subject string }{
		{digest, "Weekly digest 1"}, {digest, "Weekly digest 2"}, {"Ann <ann@example.org>", "Lunch"},
	} {
		date := now.Add(-time.Duration(3-i) * day)
		if _, err := dove.Append(user, "INBOX", itMail(m.from, user, m.subject, fmt.Sprintf("sched%d", i), date), date); err != nil {
			t.Fatal(err)
		}
	}
	clock := &itClock{}
	cfg := fmt.Sprintf(itDoveConfig, user, dove.Endpoint.Port, 1)
	opts := itDoveOptions(dove, clock.Now, nil)
	h := daemontest.Start(t, cfg, opts)
	itWaitCache(t, h, "dove", itDoveFolders)

	rule := itCreateRule(t, h, &chainmailv1.RuleDefinition{
		Name: "dove-digest", Accounts: []string{"dove"}, Query: query, Schedule: "every:1h",
		Actions: []*chainmailv1.RuleAction{{Kind: "flag"}},
	})
	if got, want := rule.GetNextRunAt().AsTime(), rule.GetCreatedAt().AsTime().Add(time.Hour); !itNear(got, want) {
		t.Errorf("next run %v, want an hour after the create, %v", got, want)
	}
	if at := itRulesStatus(t, h).GetNextScheduledAt(); at == nil || !itNear(at.AsTime(), rule.GetNextRunAt().AsTime()) {
		t.Errorf("RulesStatus next_scheduled_at = %v, want the rule's next run %v", at, rule.GetNextRunAt().AsTime())
	}
	if runs := itRuns(t, h, rule.GetId(), "dove", "schedule"); len(runs) != 0 {
		t.Fatalf("schedule runs before the hour passed: %v", runs)
	}

	// 1. An hour and a minute pass. A rule change wakes the scheduler.
	itWaitCacheComplete(t, h, "dove", "in:inbox")
	clock.Advance(61 * time.Minute)
	itWaitCacheComplete(t, h, "dove", "in:inbox")
	poke := itCreateRule(t, h, &chainmailv1.RuleDefinition{
		Name: "dove-poke", Accounts: []string{"dove"}, Query: "from:nobody@example.invalid",
		Actions: []*chainmailv1.RuleAction{{Kind: "flag"}},
	})
	run := itWaitRuns(t, h, rule.GetId(), "dove", "schedule", 1, itRunWithin)[0]
	if run.GetMatched() != 2 || run.GetApplied() != 2 || run.GetSkipped() != 0 || run.GetFailed() != 0 || run.GetOutcome() != "ok" {
		t.Errorf("scheduled run = %v, want both digests matched and applied, outcome ok", run)
	}
	starred := itServerRows(t, h, "dove", "in:inbox is:starred")
	if len(starred) != 2 || slices.ContainsFunc(starred, func(r *chainmailv1.MessageRow) bool {
		return !strings.HasPrefix(r.GetSubject().GetUntrusted(), "Weekly digest")
	}) {
		t.Errorf("starred in INBOX: %v, want the two digests", starred)
	}
	got := itGetRule(t, h, rule.GetId())
	finished := run.GetFinishedAt().AsTime()
	if !itNear(got.GetLastRunAt().AsTime(), finished) || !itNear(got.GetNextRunAt().AsTime(), finished.Add(time.Hour)) {
		t.Errorf("after the run: last run %v, next run %v, want %v and an hour later", got.GetLastRunAt().AsTime(), got.GetNextRunAt().AsTime(), finished)
	}

	// 2. Another change re-plans, and the rule, not due, does not run.
	itUpdateRule(t, h, poke.GetId(), &chainmailv1.RuleDefinition{Description: "Wakes the scheduler"}, "description")
	itQuietRuns(t, h, rule.GetId(), "dove", "schedule", 1, itQuiet)

	// 3. Five hours of downtime, with one more digest arriving meanwhile.
	if err := h.Stop(); err != nil {
		t.Fatalf("stopping the daemon: %v", err)
	}
	if _, err := dove.Append(user, "INBOX", itMail(digest, user, "Weekly digest 3", "sched3", time.Now()), time.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Hour)
	restarted := opts
	restarted.Reuse = h
	h2 := daemontest.Start(t, cfg, restarted)
	// The first run before the downtime, and the catch-up: two runs, not
	// six. A cache_not_ready catch-up adds its one retry.
	want := 2
	runs := itWaitRuns(t, h2, rule.GetId(), "dove", "schedule", want, itPollWithin)
	catchUp := runs[0]
	if catchUp.GetOutcome() == "cache_not_ready" {
		// The start-up run met the cache before its first sync pass. It
		// retries five minutes later instead of an interval out (amendment
		// 7), and is not repeated meanwhile.
		t.Logf("the catch-up run found the cache not ready (%s), as a run at start-up can", catchUp.GetReason())
		if len(runs) != want {
			t.Fatalf("%d finished schedule runs after the restart, want %d, the catch-up once: %v", len(runs), want, runs)
		}
		retry := catchUp.GetFinishedAt().AsTime().Add(5 * time.Minute)
		if next := itGetRule(t, h2, rule.GetId()).GetNextRunAt().AsTime(); !itNear(next, retry) {
			t.Errorf("next run after a catch-up the cache could not answer = %v, want five minutes after it, %v", next, retry)
		}
		itQuietRuns(t, h2, rule.GetId(), "dove", "schedule", want, itQuiet)
		itWaitCacheComplete(t, h2, "dove", "in:inbox")
		clock.Advance(5*time.Minute + time.Second)
		itWaitCacheComplete(t, h2, "dove", "in:inbox")
		itUpdateRule(t, h2, poke.GetId(), &chainmailv1.RuleDefinition{Description: "Wakes the scheduler again"}, "description")
		want++
		runs = itWaitRuns(t, h2, rule.GetId(), "dove", "schedule", want, itRunWithin)
		catchUp = runs[0]
	}
	if len(runs) != want {
		t.Fatalf("%d finished schedule runs after the restart, want %d: missed runs were repeated: %v", len(runs), want, runs)
	}
	// The catch-up takes the digest that arrived during the downtime and
	// skips the two the first run flagged.
	if catchUp.GetOutcome() != "ok" || catchUp.GetMatched() != 3 || catchUp.GetSkipped() != 2 || catchUp.GetApplied() != 1 || catchUp.GetFailed() != 0 {
		t.Errorf("catch-up run = %v, want outcome ok, 3 matched, 2 skipped by the act-once table, 1 applied", catchUp)
	}
	itQuietRuns(t, h2, rule.GetId(), "dove", "schedule", want, itQuiet)
	if next := itGetRule(t, h2, rule.GetId()).GetNextRunAt().AsTime(); !itNear(next, catchUp.GetFinishedAt().AsTime().Add(time.Hour)) {
		t.Errorf("next run after the catch-up = %v, want an hour after it, %v", next, catchUp.GetFinishedAt().AsTime().Add(time.Hour))
	}
	// Each digest is flagged, once, and nothing else.
	starred = itServerRows(t, h2, "dove", "in:inbox is:starred")
	if len(starred) != 3 || slices.ContainsFunc(starred, func(r *chainmailv1.MessageRow) bool {
		return !strings.HasPrefix(r.GetSubject().GetUntrusted(), "Weekly digest")
	}) {
		t.Errorf("starred in INBOX after the catch-up: %v, want the three digests", starred)
	}
}
