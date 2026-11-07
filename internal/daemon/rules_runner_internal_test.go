package daemon

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

// schedRunnerServer is a server with cache.db open, fake mail stores and
// a cache worker and a rule runner for host. The worker never runs, so the
// cache cannot answer. The account off has the cache turned off and no runner.
func schedRunnerServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newCacheServerOptions(t, cacheConfig, Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store { return fake.New(a.Name, a.Provider.Gmail) },
	})
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	ca := cache.NewAccount(s.cacheAccountOptions(acct))
	s.cacheAccounts = map[string]*cache.Account{"host": ca}
	s.ruleRunners = map[string]*ruleRunner{"host": s.newRuleRunner(acct, ca)}
	return s
}

// A manual run goes through the account's runner where it has one, and is
// recorded as cache_disabled where it has none.
func TestRunRuleUsesTheAccountRunner(t *testing.T) {
	t.Parallel()
	s := schedRunnerServer(t)
	rs := &rulesService{s: s}
	ctx := peerContext(t.Context())
	if _, err := rs.CreateRule(ctx, connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
		Name: "both", Query: "in:inbox", Actions: []*chainmailv1.RuleAction{{Kind: "flag"}},
	}})); err != nil {
		t.Fatal(err)
	}
	resp, err := rs.RunRule(ctx, connect.NewRequest(&chainmailv1.RunRuleRequest{Rule: "both"}))
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[string]*chainmailv1.RuleRun{}
	for _, run := range resp.Msg.GetRuns() {
		byAccount[run.GetAccount()] = run
	}
	host, off := byAccount["host"], byAccount["off"]
	if host == nil || host.GetOutcome() != "cache_not_ready" || host.GetReason() == runnerCacheDisabled || host.GetTrigger() != "manual" {
		t.Errorf("host run = %v, want cache_not_ready from the runner", host)
	}
	if off == nil || off.GetOutcome() != "cache_not_ready" || off.GetReason() != runnerCacheDisabled || off.GetHint() != cacheDisabledHint {
		t.Errorf("off run = %v, want cache_disabled", off)
	}
}

// RulesStatus reports a runner's state, acknowledged position and lag,
// and cache_disabled for an account without one.
func TestRulesStatusReportsRunners(t *testing.T) {
	t.Parallel()
	s := schedRunnerServer(t)
	for range 3 {
		if err := s.watchLog.Publish(t.Context(), watch.Event{At: time.Now(), Kind: watch.KindStatus, Account: "host", Folder: "INBOX", State: watch.StateWatching}); err != nil {
			t.Fatal(err)
		}
	}
	// The runner never runs, so the test records the acknowledged position
	// open would have: two events short of the tail.
	tail, err := s.watchLog.Tail(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.ruleRunners["host"].setAcked(tail - 2)
	resp, err := (&rulesService{s: s}).RulesStatus(peerContext(t.Context()), connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	runners := resp.Msg.GetRunners()
	if len(runners) != 2 || runners[0].GetAccount() != "host" || runners[1].GetAccount() != "off" {
		t.Fatalf("runners = %v, want host then off", runners)
	}
	host := runners[0]
	if host.GetState() != runnerIdle {
		t.Errorf("host state = %q, want idle once it has an acknowledged position", host.GetState())
	}
	if host.GetAckedId() != tail-2 || host.GetLag() != 2 {
		t.Errorf("host acked %d lag %d, want acked %d lag 2", host.GetAckedId(), host.GetLag(), tail-2)
	}
	if off := runners[1]; off.GetState() != runnerCacheDisabled || off.GetLag() != 0 || off.GetAckedId() != 0 {
		t.Errorf("off = %v, want cache_disabled", off)
	}
	for _, tt := range []struct{ tail, acked, want int64 }{{3, 0, 3}, {3, 3, 0}, {3, 5, 0}, {3, -1, 0}} {
		if got := runnerLag(tt.tail, tt.acked); got != tt.want {
			t.Errorf("runnerLag(%d, %d) = %d, want %d", tt.tail, tt.acked, got, tt.want)
		}
	}
}

// A store failure on the runner path reaches the client as fixed text,
// never as the store's or the driver's words, as it does on the no-runner
// path. A refusal of the account still reaches the client unchanged.
func TestRunOnAccountsHidesStoreFailures(t *testing.T) {
	t.Parallel()
	s := schedRunnerServer(t)
	host, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	d := rules.Defaults()
	d.Name, d.Query, d.Actions = "elsewhere", "in:inbox", []rules.Action{{Kind: rules.ActFlag}}
	d.Accounts = []string{"off"}
	elsewhere, err := s.rules.Create(t.Context(), items.Actor{UID: 1000, Client: "test"}, d.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.runOnAccounts(t.Context(), elsewhere, []config.ResolvedAccount{host}, rules.TriggerManual, 0); !errors.Is(err, errRuleAccount) {
		t.Errorf("uncovered account: err = %v, want errRuleAccount", err)
	}
	d.Name, d.Accounts = "everywhere", nil
	everywhere, err := s.rules.Create(t.Context(), items.Actor{UID: 1000, Client: "test"}, d.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.opts.StateDB.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.runOnAccounts(t.Context(), everywhere, []config.ResolvedAccount{host}, rules.TriggerManual, 0)
	if err == nil {
		t.Fatal("runOnAccounts on a closed state.db = nil error")
	}
	ce := toConnectError(err)
	if ce.Code() != connect.CodeInternal || ce.Message() != "the rule store failed" {
		t.Errorf("store failure = %v %q, want internal with fixed text", ce.Code(), ce.Message())
	}
}
