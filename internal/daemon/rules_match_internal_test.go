package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/rules"
)

// TestRuleEnv checks that a rule is validated against every configured
// account, with whether it is a Google account, and against the policy's
// max_recipients, defaulted when the config leaves it out.
func TestRuleEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   rules.Env
	}{
		{
			name:   "default max_recipients",
			config: routeConfig,
			want: rules.Env{
				Accounts:      map[string]rules.AccountInfo{"gmail": {Gmail: true}, "host": {Gmail: false}},
				MaxRecipients: 10,
			},
		},
		{
			name:   "configured max_recipients",
			config: passwordConfig + "\n[policy]\nmax_recipients = 3\n",
			want: rules.Env{
				Accounts:      map[string]rules.AccountInfo{"host": {Gmail: false}},
				MaxRecipients: 3,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerConfig(t, tt.config)
			got := s.ruleEnv()
			if !maps.Equal(got.Accounts, tt.want.Accounts) || got.MaxRecipients != tt.want.MaxRecipients {
				t.Errorf("ruleEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestRuleMessageKey checks the act-once key of a cached row: the Gmail
// message ID on a Google account, else UIDVALIDITY, UID and folder, which a
// Google row without a message ID falls back to.
func TestRuleMessageKey(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, routeConfig)
	host, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	gmail, err := s.opts.Config.Account("gmail")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		gmail bool
		row   cache.Row
		want  string
	}{
		{name: "standard", row: cache.Row{Folder: "INBOX", UIDValidity: 7, UID: 42}, want: "i:7:42:INBOX"},
		{name: "standard ignores a message ID", row: cache.Row{Folder: "Archive", UIDValidity: 3, UID: 9, GmailMsgID: 55}, want: "i:3:9:Archive"},
		{name: "google", gmail: true, row: cache.Row{Folder: "[Gmail]/All Mail", UIDValidity: 3, UID: 9, GmailMsgID: 1001}, want: "g:1001"},
		{name: "google without a message ID", gmail: true, row: cache.Row{Folder: "[Gmail]/All Mail", UIDValidity: 3, UID: 9}, want: "i:3:9:[Gmail]/All Mail"},
	}
	for _, tt := range tests {
		acct := host
		if tt.gmail {
			acct = gmail
		}
		if got := ruleMessageKey(acct, tt.row); got != tt.want {
			t.Errorf("%s: ruleMessageKey = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestRuleActionsProto checks that every argument of an action reaches its
// API form, in order, and that the recipient list is a copy.
func TestRuleActionsProto(t *testing.T) {
	t.Parallel()
	to := []string{"a@example.com", "b@example.com"}
	in := []rules.Action{
		{Kind: rules.ActLabel, Label: "Receipts"},
		{Kind: rules.ActCopy, Folder: "Keep"},
		{Kind: rules.ActLinkItem, ItemID: "it_x", Anchor: true},
		{Kind: rules.ActForward, To: to, Note: "fyi"},
		{Kind: rules.ActArchive},
	}
	want := []*chainmailv1.RuleAction{
		{Kind: "label", Label: "Receipts"},
		{Kind: "copy", Folder: "Keep"},
		{Kind: "link_item", ItemId: "it_x", Anchor: true},
		{Kind: "forward", To: []string{"a@example.com", "b@example.com"}, Note: "fyi"},
		{Kind: "archive"},
	}
	got := ruleActionsProto(in)
	if len(got) != len(want) {
		t.Fatalf("got %d actions, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("action %d = %v, want %v", i, got[i], want[i])
		}
	}
	to[0] = "changed@example.com"
	if got[3].GetTo()[0] != "a@example.com" {
		t.Error("the API form shares the rule's recipient list")
	}
	if got := ruleActionsProto(nil); len(got) != 0 {
		t.Errorf("no actions gave %v, want none", got)
	}
}

// ruleMarker is in every query, folder, item ID and account name the rule
// tests put in a failing rule. No error text or hint may repeat it.
const ruleMarker = "zqx"

// ruleWantErr fails unless err maps to reason with exactly msg as its text,
// and neither its text nor its hint holds ruleMarker.
func ruleWantErr(t *testing.T, err error, reason, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s %q", reason, msg)
	}
	d := api.Detail(toConnectError(err))
	if d.GetReason() != reason || err.Error() != msg {
		t.Errorf("err = %s %q, want %s %q", d.GetReason(), err.Error(), reason, msg)
	}
	if strings.Contains(err.Error(), ruleMarker) || strings.Contains(d.GetHint(), ruleMarker) {
		t.Errorf("err %q (hint %q) repeats rule text", err.Error(), d.GetHint())
	}
}

// TestRuleAccounts checks that a rule covers its own accounts, or every
// configured one, narrowed by its query's account: terms, and that a name
// no config holds is refused without repeating it.
func TestRuleAccounts(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, routeConfig)
	tests := []struct {
		name     string
		accounts []string
		query    string
		want     []string
	}{
		{name: "every account", want: []string{"gmail", "host"}},
		{name: "explicit", accounts: []string{"host"}, want: []string{"host"}},
		{name: "explicit, repeated and unsorted", accounts: []string{"host", "gmail", "host"}, want: []string{"gmail", "host"}},
		{name: "query narrows every account", query: "account:gmail from:acme", want: []string{"gmail"}},
		{name: "query narrows the explicit list", accounts: []string{"gmail", "host"}, query: "account:host", want: []string{"host"}},
		{name: "query names two accounts", query: "account:host account:gmail", want: []string{"gmail", "host"}},
		{name: "query outside the explicit list", accounts: []string{"host"}, query: "account:gmail", want: []string{}},
	}
	for _, tt := range tests {
		accts, err := s.ruleAccounts(rules.Definition{Accounts: tt.accounts, Query: tt.query})
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		names := make([]string, len(accts))
		for i, a := range accts {
			names[i] = a.Name
		}
		if !slices.Equal(names, tt.want) {
			t.Errorf("%s: accounts = %q, want %q", tt.name, names, tt.want)
		}
	}
	errs := []struct {
		name   string
		d      rules.Definition
		reason string
		msg    string
	}{
		{"unknown account", rules.Definition{Accounts: []string{"zqxnope"}}, api.ReasonNotFound, "the rule names an account that is not configured"},
		{"invalid account name", rules.Definition{Accounts: []string{"zqx bad"}}, api.ReasonInvalidArgument, "the rule names an account that is not configured"},
		{"unknown account: term", rules.Definition{Query: "account:zqxnope"}, api.ReasonNotFound, "the rule names an account that is not configured"},
		{"query does not parse", rules.Definition{Query: "is:zqxbogus"}, api.ReasonInvalidArgument, "the rule's query does not parse"},
	}
	for _, tt := range errs {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.ruleAccounts(tt.d)
			ruleWantErr(t, err, tt.reason, tt.msg)
		})
	}
}

// ruleFixture is a route fixture for rules: seedRouteMail's mail, a draft
// and a sent message in gmail's All Mail, then each seed, both caches warm
// and installed, the daemon's cache store set as Serve sets it, and
// itemFixture's resolver, which links host's plan thread to it_plan.
func ruleFixture(t *testing.T, seeds ...func(*routeFixture)) *routeFixture {
	t.Helper()
	f := newRouteFixture(t)
	seedRouteMail(f)
	gmail := f.fakes["gmail"]
	gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: routeDaysAgo(7), Subject: "draft note", Body: "a draft", Labels: []string{`\Draft`}})
	gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Date: routeDaysAgo(6), Subject: "sent note", Body: "a sent copy", Labels: []string{`\Sent`}})
	for _, seed := range seeds {
		seed(f)
	}
	f.warm(t, "host")
	f.warm(t, "gmail")
	f.install(t, "host", "gmail")
	f.s.cache = f.store
	f.s.itemThreads = &stubItemResolver{
		keys:   map[string]map[string][]string{"it_plan": {"host": {planKey}}, "it_empty": {}},
		linked: map[string][]string{"host": {planKey}},
	}
	return f
}

// ruleAcct returns the fixture's configured account name.
func ruleAcct(t *testing.T, f *routeFixture, name string) config.ResolvedAccount {
	t.Helper()
	acct, err := f.s.opts.Config.Account(name)
	if err != nil {
		t.Fatal(err)
	}
	return acct
}

// ruleScopeOf scopes a rule with query q, as rule ru_test, on account,
// failing the test on an error or a reason.
func ruleScopeOf(t *testing.T, f *routeFixture, account, q string, complete bool) ruleScope {
	t.Helper()
	sc, reason, err := f.s.scopeRule(t.Context(), "ru_test", rules.Definition{Query: q}, ruleAcct(t, f, account), complete)
	if err != nil || reason != "" {
		t.Fatalf("scopeRule(%s, %q) = reason %q, err %v; want neither", account, q, reason, err)
	}
	return sc
}

// ruleSourceFolders returns the folders of sc's sources, sorted.
func ruleSourceFolders(sc ruleScope) []string {
	out := make([]string, len(sc.sources))
	for i, src := range sc.sources {
		out[i] = src.Folder
	}
	slices.Sort(out)
	return out
}

// ruleStaleArchive makes host's polled Archive folder stale.
func ruleStaleArchive(t *testing.T, f *routeFixture) { f.ageSync(t, "host", "Archive", time.Hour) }

// ruleBackfilling reopens host's Archive backfill.
func ruleBackfilling(t *testing.T, f *routeFixture) {
	f.editProgress(t, "host", "Archive", func(p *cache.Progress) { p.BackfillDone = false })
}

// ruleDropGmail leaves gmail without a cache worker.
func ruleDropGmail(_ *testing.T, f *routeFixture) { delete(f.s.cacheAccounts, "gmail") }

// ruleCacheDown turns the daemon's cache off, as a full disk does.
func ruleCacheDown(_ *testing.T, f *routeFixture) { f.s.cacheDown.Store(true) }

// ruleStaleSentDrafts makes host's Sent and Drafts folders stale, folders
// a rule with no in: term never matches (spec amendment 6).
func ruleStaleSentDrafts(t *testing.T, f *routeFixture) {
	f.ageSync(t, "host", "Sent", time.Hour)
	f.ageSync(t, "host", "Drafts", time.Hour)
}

// ruleUnsyncedSentDrafts drops host's Sent and Drafts folders from the
// cache, so neither has been synced into it.
func ruleUnsyncedSentDrafts(t *testing.T, f *routeFixture) {
	for _, folder := range []string{"Sent", "Drafts"} {
		if err := f.store.DropFolder(t.Context(), "host", folder, false); err != nil {
			t.Fatal(err)
		}
	}
}

// ruleUnreadableArchive records a message of host's Archive as unreadable.
func ruleUnreadableArchive(t *testing.T, f *routeFixture) { f.markUnreadable(t, "host", "Archive") }

// ruleUnreadableSent records a message of host's Sent as unreadable.
func ruleUnreadableSent(t *testing.T, f *routeFixture) { f.markUnreadable(t, "host", "Sent") }

// TestScopeRuleFolders checks spec amendments 6 and 13: off Google a rule
// with no in: covers every searched folder but Sent and Drafts, and in:
// names the folders; on Google it covers All Mail and, with no in:, skips
// the \Sent and \Draft labels. A text term needs bodies.
func TestScopeRuleFolders(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	gmailDefault := []string{`\Sent`, `\Draft`}
	allMail := []string{"[Gmail]/All Mail"}
	tests := []struct {
		account, query string
		complete       bool
		folders        []string
		exclude        []string
		bodies         bool
	}{
		{account: "host", query: "", complete: true, folders: []string{"Archive", "INBOX"}},
		{account: "host", query: "from:acme", complete: true, folders: []string{"Archive", "INBOX"}},
		{account: "host", query: "in:inbox", complete: true, folders: []string{"INBOX"}},
		{account: "host", query: "in:sent", complete: true, folders: []string{"Sent"}},
		{account: "host", query: "in:drafts from:acme", complete: true, folders: []string{"Drafts"}},
		{account: "host", query: "invoice", folders: []string{"Archive", "INBOX"}, bodies: true},
		{account: "host", query: "invoice newer_than:30d", complete: true, folders: []string{"Archive", "INBOX"}, bodies: true},
		{account: "gmail", query: "", complete: true, folders: allMail, exclude: gmailDefault},
		{account: "gmail", query: "from:acme", complete: true, folders: allMail, exclude: gmailDefault},
		{account: "gmail", query: "in:inbox", complete: true, folders: allMail},
		{account: "gmail", query: "in:sent", complete: true, folders: allMail},
		// Gmail's Trash is not synced: no source, so nothing matches.
		{account: "gmail", query: "in:trash"},
	}
	for _, tt := range tests {
		sc := ruleScopeOf(t, f, tt.account, tt.query, tt.complete)
		if got := ruleSourceFolders(sc); !slices.Equal(got, tt.folders) {
			t.Errorf("%s %q: folders = %q, want %q", tt.account, tt.query, got, tt.folders)
		}
		if !slices.Equal(sc.excludeLabels, tt.exclude) {
			t.Errorf("%s %q: excluded labels = %q, want %q", tt.account, tt.query, sc.excludeLabels, tt.exclude)
		}
		if sc.needsBodies != tt.bodies {
			t.Errorf("%s %q: needsBodies = %t, want %t", tt.account, tt.query, sc.needsBodies, tt.bodies)
		}
		if sc.acct.Name != tt.account || sc.ruleID != "ru_test" {
			t.Errorf("%s %q: scope is for %s, rule %q", tt.account, tt.query, sc.acct.Name, sc.ruleID)
		}
		for _, src := range sc.sources {
			if src.UIDValidity == 0 {
				t.Errorf("%s %q: source %s has no UIDVALIDITY", tt.account, tt.query, src.Folder)
			}
		}
	}
}

// TestScopeRuleReasons checks that a scheduled or manual run gets the
// cache's reason when it cannot answer completely, that a new-mail batch,
// which waits for the cache instead, does not, and that an account with no
// cache is cache_disabled either way.
func TestScopeRuleReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		account  string
		query    string
		complete bool
		spoil    func(*testing.T, *routeFixture)
		reason   string
	}{
		{name: "fresh", account: "host", query: "from:acme", complete: true},
		{name: "fresh google", account: "gmail", query: "from:acme", complete: true},
		{name: "stale folder", account: "host", query: "from:acme", complete: true, spoil: ruleStaleArchive, reason: cache.ReasonStale},
		{name: "stale folder, new mail", account: "host", query: "from:acme", spoil: ruleStaleArchive},
		{name: "backfilling", account: "host", complete: true, spoil: ruleBackfilling, reason: cache.ReasonBackfilling},
		{name: "text without a bound", account: "host", query: "invoice", complete: true, reason: cache.ReasonTextOutsideWindow},
		{name: "text without a bound, new mail", account: "host", query: "invoice"},
		{name: "gmail trash", account: "gmail", query: "in:trash", complete: true, reason: cache.ReasonFolderNotSynced},
		{name: "no cache worker", account: "gmail", spoil: ruleDropGmail, reason: reasonCacheDisabled},
		{name: "no cache worker, complete", account: "gmail", complete: true, spoil: ruleDropGmail, reason: reasonCacheDisabled},
		{name: "cache down", account: "host", complete: true, spoil: ruleCacheDown, reason: reasonCacheDisabled},
		// The cache answers for the folders the rule matches, so Sent and
		// Drafts, which a rule with no in: leaves out, do not hold it up.
		{name: "stale sent and drafts", account: "host", query: "from:acme", complete: true, spoil: ruleStaleSentDrafts},
		{name: "unsynced sent and drafts", account: "host", query: "from:acme", complete: true, spoil: ruleUnsyncedSentDrafts},
		{name: "stale sent, in:sent", account: "host", query: "in:sent", complete: true, spoil: ruleStaleSentDrafts, reason: cache.ReasonStale},
		// A whole-cache run would miss the unreadable messages, so it is not
		// ready; a new-mail batch matches the rows it has (amendment 11).
		{name: "unreadable", account: "host", query: "from:acme", complete: true, spoil: ruleUnreadableArchive, reason: cache.ReasonUnreadableMessages},
		{name: "unreadable, new mail", account: "host", query: "from:acme", spoil: ruleUnreadableArchive},
		{name: "unreadable in sent", account: "host", query: "from:acme", complete: true, spoil: ruleUnreadableSent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := ruleFixture(t)
			if tt.spoil != nil {
				tt.spoil(t, f)
			}
			_, reason, err := f.s.scopeRule(t.Context(), "ru_test", rules.Definition{Query: tt.query}, ruleAcct(t, f, tt.account), tt.complete)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.reason {
				t.Errorf("reason = %q, want %q", reason, tt.reason)
			}
		})
	}
}

// TestScopeRuleErrors checks that what waiting cannot change is an error
// with fixed text that never repeats the rule's query, folder or item.
func TestScopeRuleErrors(t *testing.T) {
	t.Parallel()
	itemsOff := func(_ *testing.T, f *routeFixture) { f.s.itemThreads = nil }
	serverDown := func(_ *testing.T, f *routeFixture) {
		f.fakes["host"].Fail("SearchFolders", fmt.Errorf("%w: zqx went away", mailstore.ErrUnavailable))
	}
	tests := []struct {
		name    string
		account string
		query   string
		prep    func(*testing.T, *routeFixture)
		reason  string
		msg     string
	}{
		{name: "query does not parse", account: "host", query: "is:zqxbogus", reason: api.ReasonInvalidArgument, msg: "the rule's query does not parse"},
		{name: "unknown item", account: "host", query: "item:it_zqxgone", reason: api.ReasonNotFound, msg: "an item: term names no live item"},
		{name: "no item store", account: "host", query: "item:it_plan", prep: itemsOff, reason: api.ReasonInvalidArgument, msg: "item: needs the daemon's item store, which is not running"},
		{name: "does not compile", account: "host", query: "before:1969-12-31", reason: api.ReasonInvalidArgument, msg: "the rule's query does not compile for the local cache"},
		{name: "unsupported off Google", account: "host", query: `label:\Seen`, reason: api.ReasonInvalidArgument, msg: "the rule's query holds a value the local cache cannot evaluate on this account"},
		{name: "in: names no folder", account: "host", query: "in:zqx-nowhere", reason: api.ReasonNotFound, msg: "the rule's in: names no folder on this account"},
		{name: "mail server down", account: "host", query: "", prep: serverDown, reason: api.ReasonServerUnavailable, msg: "the mail server failed while reading"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := ruleFixture(t)
			if tt.prep != nil {
				tt.prep(t, f)
			}
			_, reason, err := f.s.scopeRule(t.Context(), "ru_test", rules.Definition{Query: tt.query}, ruleAcct(t, f, tt.account), true)
			if reason != "" {
				t.Errorf("reason = %q, want none with an error", reason)
			}
			ruleWantErr(t, err, tt.reason, tt.msg)
		})
	}
}

// ruleRowNamed returns account's cached row with subject, from any folder
// the cache tracks, Sent and Drafts included.
func ruleRowNamed(t *testing.T, f *routeFixture, account, subject string) cache.Row {
	t.Helper()
	folders, err := f.accounts[account].Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := f.store.SourcesOf(t.Context(), account, folders)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.MatchRows(t.Context(), account, cache.Where{}, cache.MatchOptions{Sources: sources, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Subject == subject {
			return r
		}
	}
	t.Fatalf("%s has no cached row %q", account, subject)
	return cache.Row{}
}

// ruleRowSubjects returns the subjects of rows, in order.
func ruleRowSubjects(rows []cache.Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Subject)
	}
	return out
}

// ruleMatchAll pages sc with matchPage, limit rows at a time, checking
// that no page is longer than limit, and returns every row in order.
func ruleMatchAll(t *testing.T, f *routeFixture, sc ruleScope, limit int) []cache.Row {
	t.Helper()
	var (
		out   []cache.Row
		after cache.MatchCursor
	)
	for range 100 {
		rows, err := f.s.matchPage(t.Context(), sc, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > limit {
			t.Fatalf("page of %d rows, limit %d", len(rows), limit)
		}
		out = append(out, rows...)
		if len(rows) < limit {
			return out
		}
		after = cache.CursorOf(rows[len(rows)-1])
	}
	t.Fatal("matchPage never ran out of rows")
	return nil
}

// TestMatchPage checks that a rule's scope pages oldest first, two rows at
// a time, through every match and only those: default scopes, in:, text,
// item: and Google labels.
func TestMatchPage(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	tests := []struct {
		account, query string
		want           []string
	}{
		{"host", "", []string{"old plan", "invoice 41", "re: old plan", "invoice 42", "lunch"}},
		{"host", "from:acme", []string{"invoice 41", "invoice 42"}},
		{"host", "in:sent", []string{"re: re: old plan"}},
		{"host", "in:inbox newer_than:7d", []string{"invoice 42", "lunch"}},
		{"host", "invoice newer_than:30d", []string{"invoice 41", "invoice 42"}},
		{"host", "item:it_plan", []string{"old plan", "re: old plan"}},
		{"host", "item:it_plan in:sent", []string{"re: re: old plan"}},
		{"host", "item:none", []string{"invoice 41", "invoice 42", "lunch"}},
		{"host", "item:it_empty", nil},
		{"gmail", "", []string{"invoice 40", "trip", "re: trip"}},
		{"gmail", "in:sent", []string{"sent note"}},
		{"gmail", "in:drafts", []string{"draft note"}},
		{"gmail", "item:it_plan", nil},
		{"gmail", "in:trash", nil},
	}
	for _, tt := range tests {
		sc := ruleScopeOf(t, f, tt.account, tt.query, false)
		if got := ruleRowSubjects(ruleMatchAll(t, f, sc, 2)); !slices.Equal(got, tt.want) {
			t.Errorf("%s %q = %q, want %q", tt.account, tt.query, got, tt.want)
		}
	}
}

// TestMatchBatch checks that a batch matches only its own UIDs, only in the
// folders the rule covers, and that an empty or nil batch matches nothing
// rather than the whole scope.
func TestMatchBatch(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	uid := func(account, subject string) uint32 { return ruleRowNamed(t, f, account, subject).UID }
	inv41, inv42, lunch, sent := uid("host", "invoice 41"), uid("host", "invoice 42"), uid("host", "lunch"), uid("host", "re: re: old plan")
	allMail := "[Gmail]/All Mail"
	trip, sentNote := uid("gmail", "trip"), uid("gmail", "sent note")
	tests := []struct {
		name, account, query string
		uids                 map[string][]uint32
		want                 []string
	}{
		{"across folders, Sent left out", "host", "", map[string][]uint32{"INBOX": {inv42}, "Archive": {inv41}, "Sent": {sent}}, []string{"invoice 41", "invoice 42"}},
		{"the query narrows the batch", "host", "from:acme", map[string][]uint32{"INBOX": {inv42, lunch}}, []string{"invoice 42"}},
		{"a folder absent from the batch", "host", "", map[string][]uint32{"INBOX": {lunch}}, []string{"lunch"}},
		{"a UID in the wrong folder", "host", "", map[string][]uint32{"Archive": {lunch}}, nil},
		{"in:sent opts in", "host", "in:sent", map[string][]uint32{"Sent": {sent}, "INBOX": {lunch}}, []string{"re: re: old plan"}},
		{"google skips sent mail", "gmail", "", map[string][]uint32{allMail: {trip, sentNote}}, []string{"trip"}},
		{"empty batch", "host", "", map[string][]uint32{}, nil},
		{"nil batch", "host", "", nil, nil},
	}
	for _, tt := range tests {
		sc := ruleScopeOf(t, f, tt.account, tt.query, false)
		rows, err := f.s.matchBatch(t.Context(), sc, tt.uids)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got := ruleRowSubjects(rows); !slices.Equal(got, tt.want) {
			t.Errorf("%s: matched %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestMatchWithoutACache checks that matching on a daemon whose cache.db
// is not open is CACHE_NOT_READY, not a crash.
func TestMatchWithoutACache(t *testing.T) {
	t.Parallel()
	f := ruleFixture(t)
	sc := ruleScopeOf(t, f, "host", "", false)
	f.s.cache = nil
	_, err := f.s.matchPage(t.Context(), sc, cache.MatchCursor{}, 10)
	ruleWantErr(t, err, api.ReasonCacheNotReady, "the local cache is not running")
	_, err = f.s.matchBatch(t.Context(), sc, map[string][]uint32{"INBOX": {1}})
	ruleWantErr(t, err, api.ReasonCacheNotReady, "the local cache is not running")
}
