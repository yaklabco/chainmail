package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/rules"
)

// stubApplied answers act-once lookups from a set of "account key"
// strings, as *rules.Store does from rule_applied, and records the rule
// IDs it was asked about. err, when set, fails every call.
type stubApplied struct {
	applied map[string]bool
	asked   []string
	err     error
}

var _ appliedLookup = (*stubApplied)(nil)

func (a *stubApplied) Applied(_ context.Context, ruleID, account string, keys []string) (map[string]rules.AppliedRow, error) {
	a.asked = append(a.asked, ruleID)
	if a.err != nil {
		return nil, a.err
	}
	out := map[string]rules.AppliedRow{}
	for _, k := range keys {
		if a.applied[account+" "+k] {
			out[k] = rules.AppliedRow{Outcome: rules.AppliedDone, RunID: 1, At: routeNow}
		}
	}
	return out, nil
}

// ruleApply records account's cached rows with subjects as acted on in a.
func ruleApply(t *testing.T, f *routeFixture, a *stubApplied, account string, subjects ...string) {
	t.Helper()
	acct := ruleAcct(t, f, account)
	if a.applied == nil {
		a.applied = map[string]bool{}
	}
	for _, s := range subjects {
		a.applied[account+" "+ruleMessageKey(acct, ruleRowNamed(t, f, account, s))] = true
	}
}

// seedRuleBulk adds n messages from bulk@example.com to host's INBOX, a
// minute apart from sixty days ago, older than every seedRouteMail message.
func seedRuleBulk(n int) func(*routeFixture) {
	return func(f *routeFixture) {
		host := f.fakes["host"]
		from := routeAddr("Bulk", "bulk@example.com")
		for i := range n {
			host.Add(fake.Message{
				Folder: "INBOX", Date: routeDaysAgo(60).Add(time.Duration(i) * time.Minute),
				Subject: fmt.Sprintf("bulk %03d", i), Body: "bulk mail", From: from,
			})
		}
	}
}

// ruleDryRunSummary is one account of a dry-run in a comparable form.
type ruleDryRunSummary struct {
	Account          string
	Matched, Skipped int64
	Subjects         []string
	Reason           string
}

// ruleDryRunSummaries summarizes resp, checking that every account carries
// the API form of actions.
func ruleDryRunSummaries(t *testing.T, resp *chainmailv1.DryRunRuleResponse, actions []rules.Action) []ruleDryRunSummary {
	t.Helper()
	want := ruleActionsProto(actions)
	var out []ruleDryRunSummary
	for _, a := range resp.GetAccounts() {
		if !slices.EqualFunc(a.GetActions(), want, func(x, y *chainmailv1.RuleAction) bool { return proto.Equal(x, y) }) {
			t.Errorf("%s: actions = %v, want %v", a.GetAccount(), a.GetActions(), want)
		}
		out = append(out, ruleDryRunSummary{
			Account: a.GetAccount(), Matched: a.GetMatched(), Skipped: a.GetSkipped(),
			Subjects: rowSubjects(a.GetMatches()), Reason: a.GetReason(),
		})
	}
	return out
}

var (
	ruleAcme      = rules.Definition{Query: "from:acme", Actions: []rules.Action{{Kind: rules.ActArchive}}}
	ruleGmailAcme = ruleDryRunSummary{Account: "gmail", Matched: 1, Subjects: []string{"invoice 40"}}
	ruleHostAcme  = ruleDryRunSummary{Account: "host", Matched: 2, Subjects: []string{"invoice 41", "invoice 42"}}
)

// TestRuleDryRun checks spec 5.4 on a fresh cache: every trigger, every
// covered account or one, the rule's own accounts, and the act-once count
// for a saved rule only.
func TestRuleDryRun(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	stub := &stubApplied{}
	ruleApply(t, f, stub, "host", "invoice 42")
	hostOnly := ruleAcme
	hostOnly.Accounts = []string{"host"}
	hostSkipped := ruleHostAcme
	hostSkipped.Skipped = 1
	everything := rules.Definition{Actions: []rules.Action{{Kind: rules.ActFlag}}}
	both := []ruleDryRunSummary{ruleGmailAcme, ruleHostAcme}
	tests := []struct {
		name    string
		ruleID  string
		def     rules.Definition
		trigger rules.Trigger
		account string
		lookup  appliedLookup
		asked   int
		want    []ruleDryRunSummary
	}{
		{name: "manual", def: ruleAcme, trigger: rules.TriggerManual, want: both},
		{name: "empty trigger is manual", def: ruleAcme, want: both},
		{name: "schedule", def: ruleAcme, trigger: rules.TriggerSchedule, want: both},
		{name: "new", def: ruleAcme, trigger: rules.TriggerNew, want: both},
		{name: "one account", def: ruleAcme, account: "host", want: []ruleDryRunSummary{ruleHostAcme}},
		{name: "the rule's own accounts", def: hostOnly, want: []ruleDryRunSummary{ruleHostAcme}},
		{name: "act-once table", ruleID: "ru_test", def: ruleAcme, lookup: stub, asked: 2, want: []ruleDryRunSummary{ruleGmailAcme, hostSkipped}},
		{name: "act-once table, trigger new", ruleID: "ru_test", def: ruleAcme, trigger: rules.TriggerNew, lookup: stub, asked: 2, want: []ruleDryRunSummary{ruleGmailAcme, hostSkipped}},
		{name: "an unsaved rule skips nothing", def: ruleAcme, lookup: stub, want: both},
		{name: "no act-once table", ruleID: "ru_test", def: ruleAcme, want: both},
		{name: "default scope", def: everything, trigger: rules.TriggerNew, want: []ruleDryRunSummary{
			{Account: "gmail", Matched: 3, Subjects: []string{"invoice 40", "trip", "re: trip"}},
			{Account: "host", Matched: 5, Subjects: []string{"old plan", "invoice 41", "re: old plan", "invoice 42", "lunch"}},
		}},
	}
	for _, tt := range tests {
		f.s.ruleApplied = tt.lookup
		before := len(stub.asked)
		resp, err := f.s.dryRun(t.Context(), tt.ruleID, tt.def, tt.trigger, tt.account)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got := ruleDryRunSummaries(t, resp, tt.def.Actions); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: dry run = %+v, want %+v", tt.name, got, tt.want)
		}
		asked := stub.asked[before:]
		if len(asked) != tt.asked {
			t.Errorf("%s: %d act-once lookups, want %d", tt.name, len(asked), tt.asked)
		}
		for _, id := range asked {
			if id != "ru_test" {
				t.Errorf("%s: looked up rule %q, want ru_test", tt.name, id)
			}
		}
	}
}

// TestRuleDryRunRowsAreSearchRows checks that dry-run matches are search
// rows, with the same untrusted wrapping and no snippet, which would need
// the mail server.
func TestRuleDryRunRowsAreSearchRows(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	resp, err := f.s.dryRun(t.Context(), "", ruleAcme, rules.TriggerManual, "host")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetAccounts()) != 1 {
		t.Fatalf("accounts = %v, want host only", resp.GetAccounts())
	}
	matches := resp.GetAccounts()[0].GetMatches()
	rows := ruleMatchAll(t, f, ruleScopeOf(t, f, "host", "from:acme", true), rulePageSize)
	if len(matches) != len(rows) || len(rows) != 2 {
		t.Fatalf("%d matches and %d rows, want 2 of each", len(matches), len(rows))
	}
	for i, m := range matches {
		if want := rowFromSummary(rows[i].Summary(), false, 0); !proto.Equal(m, want) {
			t.Errorf("match %d = %v, want %v", i, m, want)
		}
		if m.GetSnippet() != nil {
			t.Errorf("match %d has a snippet", i)
		}
		if m.GetSubject() == nil || len(m.GetFrom()) != 1 || m.GetFrom()[0].GetEmail().GetUntrusted() != "billing@acme.example" {
			t.Errorf("match %d is not wrapped as untrusted: %v", i, m)
		}
	}
}

// TestRuleDryRunNotReady checks that an account whose cache cannot answer
// completely gets a reason and no matches while the others still answer,
// and that trigger new, which reads the newest cached mail as it is, does
// not wait for freshness.
func TestRuleDryRunNotReady(t *testing.T) {
	t.Parallel()
	gmailOff := ruleDryRunSummary{Account: "gmail", Reason: reasonCacheDisabled}
	tests := []struct {
		name    string
		spoil   func(*testing.T, *routeFixture)
		trigger rules.Trigger
		want    []ruleDryRunSummary
	}{
		{name: "stale folder", spoil: ruleStaleArchive, trigger: rules.TriggerManual, want: []ruleDryRunSummary{ruleGmailAcme, {Account: "host", Reason: "stale"}}},
		{name: "stale folder, scheduled", spoil: ruleStaleArchive, trigger: rules.TriggerSchedule, want: []ruleDryRunSummary{ruleGmailAcme, {Account: "host", Reason: "stale"}}},
		{name: "stale folder, trigger new", spoil: ruleStaleArchive, trigger: rules.TriggerNew, want: []ruleDryRunSummary{ruleGmailAcme, ruleHostAcme}},
		{name: "no cache worker", spoil: ruleDropGmail, trigger: rules.TriggerManual, want: []ruleDryRunSummary{gmailOff, ruleHostAcme}},
		{name: "no cache worker, trigger new", spoil: ruleDropGmail, trigger: rules.TriggerNew, want: []ruleDryRunSummary{gmailOff, ruleHostAcme}},
		{name: "cache down", spoil: ruleCacheDown, trigger: rules.TriggerManual, want: []ruleDryRunSummary{gmailOff, {Account: "host", Reason: reasonCacheDisabled}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := ruleFixture(t)
			tt.spoil(t, f)
			resp, err := f.s.dryRun(t.Context(), "", ruleAcme, tt.trigger, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := ruleDryRunSummaries(t, resp, ruleAcme.Actions); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dry run = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// downScoper is a fake store whose SearchFolders turns the daemon's cache
// off first, as a full disk noticed mid-scope does: scopeRule has already
// found the account's cache worker, and every later look finds none.
type downScoper struct {
	*fake.Store
	s *Server
}

func (d downScoper) SearchFolders(ctx context.Context, in string) ([]string, error) {
	d.s.cacheDown.Store(true)
	return d.Store.SearchFolders(ctx, in)
}

// TestRuleDryRunCacheDownMidRun checks amendment 11 when the cache goes
// down after an account's scope succeeded: that account reports
// cache_disabled with no matches, and the accounts already answered keep
// their results rather than the whole dry run failing.
func TestRuleDryRunCacheDownMidRun(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	// The fixture's workers have stopped, so nothing else reads the store
	// set while the test swaps it.
	f.s.stores = &storeSet{build: func(a config.ResolvedAccount) mailstore.Store {
		if a.Name == "host" {
			return downScoper{Store: f.fakes["host"], s: f.s}
		}
		return f.fakes[a.Name]
	}}
	resp, err := f.s.dryRun(t.Context(), "", ruleAcme, rules.TriggerNew, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []ruleDryRunSummary{ruleGmailAcme, {Account: "host", Reason: reasonCacheDisabled}}
	if got := ruleDryRunSummaries(t, resp, ruleAcme.Actions); !reflect.DeepEqual(got, want) {
		t.Errorf("dry run = %+v, want %+v", got, want)
	}
	if !f.s.cacheDown.Load() {
		t.Error("the cache is still up, so the test did not turn it off mid-run")
	}
}

// TestRuleDryRunErrors checks the refusals: a bad trigger, an account the
// rule does not cover or no config names, a rule that cannot run, and a
// failed act-once lookup, each with fixed text.
func TestRuleDryRunErrors(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	hostOnly := ruleAcme
	hostOnly.Accounts = []string{"host"}
	tests := []struct {
		name    string
		def     rules.Definition
		trigger rules.Trigger
		account string
		lookup  appliedLookup
		reason  string
		msg     string
	}{
		{name: "unknown trigger", def: ruleAcme, trigger: "zqx", reason: api.ReasonInvalidArgument, msg: "trigger must be new, schedule or manual"},
		{name: "an account the rule does not cover", def: hostOnly, account: "gmail", reason: api.ReasonInvalidArgument, msg: "the rule does not cover that account"},
		{name: "an account the query leaves out", def: rules.Definition{Query: "account:host"}, account: "gmail", reason: api.ReasonInvalidArgument, msg: "the rule does not cover that account"},
		{name: "invalid account name", def: ruleAcme, account: "zqx bad", reason: api.ReasonInvalidArgument, msg: "invalid account name"},
		{name: "query does not parse", def: rules.Definition{Query: "is:zqxbogus"}, reason: api.ReasonInvalidArgument, msg: "the rule's query does not parse"},
		{name: "unknown item", def: rules.Definition{Query: "item:it_zqxgone"}, reason: api.ReasonNotFound, msg: "an item: term names no live item"},
		{name: "act-once lookup fails", def: ruleAcme, lookup: &stubApplied{err: errors.New("zqx disk")}, reason: api.ReasonInternal, msg: "reading the rule's applied messages failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.s.ruleApplied = tt.lookup
			_, err := f.s.dryRun(t.Context(), "ru_test", tt.def, cmp.Or(tt.trigger, rules.TriggerManual), tt.account)
			ruleWantErr(t, err, tt.reason, tt.msg)
		})
	}
	f.s.ruleApplied = nil
	if _, err := f.s.dryRun(t.Context(), "", ruleAcme, rules.TriggerManual, "nowhere"); apiReason(err) != api.ReasonNotFound {
		t.Errorf("account nowhere: %v, want NOT_FOUND", err)
	}
}

// TestRuleDryRunCaps checks the spec's limits: at most 50 matches with the
// full count, oldest first, and trigger new over the newest 200 cached
// messages of the account, Sent included before the rule's scope drops it.
func TestRuleDryRunCaps(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t, seedRuleBulk(250))
	def := rules.Definition{Query: "from:bulk", Actions: []rules.Action{{Kind: rules.ActArchive}}}
	bulk := func(from, to int) []string {
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("bulk %03d", i))
		}
		return out
	}
	tests := []struct {
		trigger rules.Trigger
		want    ruleDryRunSummary
	}{
		{rules.TriggerManual, ruleDryRunSummary{Account: "host", Matched: 250, Subjects: bulk(0, 49)}},
		{rules.TriggerSchedule, ruleDryRunSummary{Account: "host", Matched: 250, Subjects: bulk(0, 49)}},
		// host holds 256 messages: the newest 200 are seedRouteMail's six,
		// one of them in Sent, and bulk 056 to bulk 249.
		{rules.TriggerNew, ruleDryRunSummary{Account: "host", Matched: 194, Subjects: bulk(56, 105)}},
	}
	for _, tt := range tests {
		resp, err := f.s.dryRun(t.Context(), "", def, tt.trigger, "host")
		if err != nil {
			t.Fatalf("%s: %v", tt.trigger, err)
		}
		got := ruleDryRunSummaries(t, resp, def.Actions)
		if !reflect.DeepEqual(got, []ruleDryRunSummary{tt.want}) {
			t.Errorf("%s: dry run = %+v, want %+v", tt.trigger, got, tt.want)
		}
	}
	// The whole-scope count pages: a small page gives the same tally.
	sc := ruleScopeOf(t, f, "host", "from:bulk", true)
	small, err := f.s.dryRunScope(t.Context(), sc, 7)
	if err != nil {
		t.Fatal(err)
	}
	large, err := f.s.dryRunScope(t.Context(), sc, rulePageSize)
	if err != nil {
		t.Fatal(err)
	}
	if small.matched != 250 || large.matched != 250 || !slices.Equal(ruleRowSubjects(small.rows), ruleRowSubjects(large.rows)) {
		t.Errorf("page 7 tallied %d %q, page %d tallied %d %q; want 250 and the same rows",
			small.matched, ruleRowSubjects(small.rows), rulePageSize, large.matched, ruleRowSubjects(large.rows))
	}
}

// TestDryRunScopeCountsEveryPage checks that the act-once count covers
// every page of a whole-scope dry-run, whatever the page size.
func TestDryRunScopeCountsEveryPage(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	stub := &stubApplied{}
	ruleApply(t, f, stub, "host", "invoice 41", "lunch")
	f.s.ruleApplied = stub
	sc := ruleScopeOf(t, f, "host", "", true)
	want := []string{"old plan", "invoice 41", "re: old plan", "invoice 42", "lunch"}
	for _, page := range []int{1, 2, rulePageSize} {
		got, err := f.s.dryRunScope(t.Context(), sc, page)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if got.matched != 5 || got.skipped != 2 || !slices.Equal(ruleRowSubjects(got.rows), want) {
			t.Errorf("page %d: matched %d, skipped %d, rows %q; want 5, 2, %q", page, got.matched, got.skipped, ruleRowSubjects(got.rows), want)
		}
	}
}
