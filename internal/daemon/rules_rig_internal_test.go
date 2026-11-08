package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/sender"
)

// ruleConfig is the rule tests' daemon: the routing tests' two accounts,
// and a send policy that lets mail to example.net out.
const ruleConfig = routeConfig + `
[policy]
send_allow = ["*@example.net"]
`

// ruleActor is who the rule tests create rules and items as.
var ruleActor = items.Actor{UID: 1, Client: "test"}

// actRig is a never-served daemon whose two accounts' mail is cached,
// fresh and installed, for running rule actions and runners against.
type actRig struct {
	f *routeFixture
	// refs are the seeded messages: "host invoice", "host lunch",
	// "gmail invoice" and "gmail trip".
	refs map[string]ref.Ref
}

// newActRig seeds the rule tests' mail, then what seed adds, caches both
// accounts with bodies and hands their caches to the daemon.
func newActRig(t *testing.T, seed func(*routeFixture)) *actRig {
	t.Helper()
	f := newRouteFixtureConfig(t, ruleConfig)
	acme := routeAddr("Acme Billing", "billing@acme.example")
	host, gmail := f.fakes["host"], f.fakes["gmail"]
	refs := map[string]ref.Ref{
		"host invoice": host.Add(fake.Message{
			Folder: "INBOX", Date: routeDaysAgo(3), Subject: "invoice 42", MessageID: "inv42@x",
			Body: "please pay the invoice", From: acme,
		}),
		"host lunch": host.Add(fake.Message{Folder: "INBOX", Date: routeDaysAgo(1), Subject: "lunch", MessageID: "lunch@x", Body: "noon?"}),
		"gmail invoice": gmail.Add(fake.Message{
			Folder: "[Gmail]/All Mail", Date: routeDaysAgo(5), Subject: "invoice 40", MessageID: "inv40@x",
			Body: "an old invoice", From: acme, Labels: []string{`\Inbox`},
		}),
		"gmail trip": gmail.Add(fake.Message{
			Folder: "[Gmail]/All Mail", Date: routeDaysAgo(2), Subject: "trip", MessageID: "trip@x",
			Body: "the trip plan", Labels: []string{`\Inbox`},
		}),
	}
	if seed != nil {
		seed(f)
	}
	f.warm(t, "host")
	f.warm(t, "gmail")
	f.install(t, "host", "gmail")
	// The daemon reads cached rows and body states through s.cache, as
	// startWatch leaves it beside cacheAccounts.
	f.s.cache = f.store
	return &actRig{f: f, refs: refs}
}

func (g *actRig) acct(t *testing.T, name string) config.ResolvedAccount {
	t.Helper()
	acct, err := g.f.s.opts.Config.Account(name)
	if err != nil {
		t.Fatal(err)
	}
	return acct
}

// row is the cached row of the seeded message name.
func (g *actRig) row(t *testing.T, name string) cache.Row {
	t.Helper()
	row, ok, err := g.f.store.Row(t.Context(), g.refs[name])
	if err != nil || !ok {
		t.Fatalf("cached row of %s: %v, %t", name, err, ok)
	}
	return row
}

// newRule stores an enabled on_new rule on account: newer_than:30d, flag,
// as edit leaves it.
func (g *actRig) newRule(t *testing.T, account, name string, edit func(*rules.Definition)) rules.Rule {
	t.Helper()
	d := rules.Defaults()
	d.Name, d.Accounts, d.OnNew = name, []string{account}, true
	d.Query = "newer_than:30d"
	d.Actions = []rules.Action{{Kind: rules.ActFlag}}
	if edit != nil {
		edit(&d)
	}
	rule, err := g.f.s.rules.Create(t.Context(), ruleActor, d)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

// password stores host's password, for SMTP logins.
func (g *actRig) password(t *testing.T) {
	t.Helper()
	if err := setSecretDirect(peerContext(t.Context()), g.f.s, auth.PasswordSecret("host"), []byte("pw")); err != nil {
		t.Fatal(err)
	}
}

// failDial makes every SMTP dial fail, so a forward can only be held or
// replayed. With strict, a dial also fails the test.
func (g *actRig) failDial(t *testing.T, strict bool) {
	g.f.s.compose.dial = func(context.Context, provider.Endpoint, auth.Credentials, sender.DialOptions) (*sender.Conn, error) {
		if strict {
			t.Error("a rule forward dialed SMTP")
		}
		return nil, errors.New("no SMTP server in this test")
	}
}

// crashedForward leaves rule's forward key of the message key on account
// as an earlier daemon that stopped mid-send leaves it: unknown, with no
// stored result.
func (g *actRig) crashedForward(t *testing.T, ruleID, account, key string) {
	t.Helper()
	ctx := t.Context()
	at := "2026-11-03T11:00:00.000000000Z"
	if _, err := g.f.stateDB.ExecContext(ctx,
		`INSERT INTO idempotency (account, key, request_hash, state, result, created_at, updated_at)
		 VALUES (?, ?, ?, 'in_progress', x'', ?, ?)`,
		account, rules.ForwardKey(ruleID, account, key), make([]byte, 32), at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.s.idem.Recover(ctx); err != nil {
		t.Fatal(err)
	}
}

// appliedRow is rule's act-once row of key on account.
func (g *actRig) appliedRow(t *testing.T, ruleID, account, key string) (rules.AppliedRow, bool) {
	t.Helper()
	rows, err := g.f.s.rules.Applied(t.Context(), ruleID, account, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows[key]
	return row, ok
}

// foundIn is the ref of the one message with messageID in folder, where a
// move or copy put it.
func (g *actRig) foundIn(t *testing.T, account, folder, messageID string) ref.Ref {
	t.Helper()
	refs, err := g.f.fakes[account].FindByMessageID(t.Context(), folder, messageID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("%s in %s/%s: %v, %v, want one", messageID, account, folder, refs, err)
	}
	return refs[0]
}

// keyOf is the act-once key of m.
func keyOf(gmail bool, m ref.Ref) string {
	return rules.MessageKey(gmail, m.GmailMsgID, m.Folder, m.UIDValidity, m.UID)
}

// labelsOf reads m's Gmail labels from the fake.
func labelsOf(t *testing.T, fk *fake.Store, m ref.Ref) []string {
	t.Helper()
	sums, err := fk.FetchSummaries(t.Context(), mailstore.Source{Folder: m.Folder, UIDValidity: m.UIDValidity}, []uint32{m.UID}, mailstore.SummaryOptions{})
	if err != nil || len(sums) != 1 {
		t.Fatalf("summary of %s: %v, %v", m, sums, err)
	}
	return sums[0].Labels
}

// ruleAuditRow is one audit row written under client kind rule.
type ruleAuditRow struct {
	uid                                       uint32
	account, action, decision, result, reason string
	targets                                   []string
}

// audit returns the rows written under client kind rule, oldest first.
func (g *actRig) audit(t *testing.T) []ruleAuditRow {
	t.Helper()
	rows, err := g.f.stateDB.QueryContext(t.Context(),
		`SELECT caller_uid, account, action, targets, decision, result, reason FROM audit WHERE client = 'rule' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []ruleAuditRow
	for rows.Next() {
		var r ruleAuditRow
		var targets string
		if err := rows.Scan(&r.uid, &r.account, &r.action, &targets, &r.decision, &r.result, &r.reason); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(targets), &r.targets); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// auditActions lists the rows' actions in order.
func auditActions(rows []ruleAuditRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.action
	}
	return out
}
