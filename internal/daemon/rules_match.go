package daemon

import (
	"cmp"
	"context"
	"errors"
	"slices"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/query"
	"github.com/yaklabco/chainmail/internal/rules"
)

// ruleEnv builds the environment a rule definition is validated against:
// every configured account, with whether it is a Google account, and the
// max_recipients the send policy applies, defaulted as Parse defaults it.
func (s *Server) ruleEnv() rules.Env {
	cfg := s.opts.Config
	env := rules.Env{
		Accounts:      make(map[string]rules.AccountInfo, len(cfg.Accounts)),
		MaxRecipients: cfg.Policy.WithDefaults().MaxRecipients,
	}
	for _, name := range cfg.AccountNames() {
		acct, err := cfg.Account(name)
		if err != nil {
			continue // AccountNames lists only configured accounts
		}
		env.Accounts[name] = rules.AccountInfo{Gmail: acct.Provider.Gmail}
	}
	return env
}

// ruleMessageKey is row's key in the act-once table on acct: the Gmail
// message ID on a Google account, else the folder, UIDVALIDITY and UID.
func ruleMessageKey(acct config.ResolvedAccount, r cache.Row) string {
	return rules.MessageKey(acct.Provider.Gmail, r.GmailMsgID, r.Folder, r.UIDValidity, r.UID)
}

// ruleActionsProto is the API form of a rule's actions, in order. The
// recipient list of a forward is copied, so the response never shares the
// rule's memory.
func ruleActionsProto(as []rules.Action) []*chainmailv1.RuleAction {
	out := make([]*chainmailv1.RuleAction, len(as))
	for i, a := range as {
		out[i] = &chainmailv1.RuleAction{
			Kind:   string(a.Kind),
			Label:  a.Label,
			Folder: a.Folder,
			ItemId: a.ItemID,
			Anchor: a.Anchor,
			To:     slices.Clone(a.To),
			Note:   a.Note,
		}
	}
	return out
}

// ruleQueryErr is a rule query that does not parse, with fixed text: the
// parser's own error, which can quote the query, stays on the chain for
// the reason and the logs.
func ruleQueryErr(err error) error {
	return &fixedTextError{msg: "the rule's query does not parse", err: err}
}

// ruleAccountErr is a rule account, or account: term, that names no
// configured account, with fixed text.
func ruleAccountErr(err error) error {
	return &fixedTextError{msg: "the rule names an account that is not configured", err: err}
}

// ruleAccounts resolves the accounts a rule covers: its Accounts or every
// configured account, intersected with the query's account: terms. The
// result is sorted without repeats and may be empty, when the query names
// only accounts the rule leaves out.
func (s *Server) ruleAccounts(d rules.Definition) ([]config.ResolvedAccount, error) {
	n, err := query.Parse(d.Query)
	if err != nil {
		return nil, ruleQueryErr(err)
	}
	scope, _ := query.Split(n)
	accts, err := s.resolveAccounts(d.Accounts)
	if err != nil {
		return nil, ruleAccountErr(err)
	}
	if len(scope.Accounts) == 0 {
		return accts, nil
	}
	for _, name := range scope.Accounts {
		if _, err := s.opts.Config.Account(name); err != nil {
			return nil, ruleAccountErr(err)
		}
	}
	return slices.DeleteFunc(accts, func(a config.ResolvedAccount) bool {
		return !slices.Contains(scope.Accounts, a.Name)
	}), nil
}

// ruleDefaultExclude are the Google labels a rule with no in: term never
// matches (spec amendment 6), so a rule never acts on its own forwards'
// Sent copies or on held drafts. Naming in:sent or in:drafts opts in.
var ruleDefaultExclude = []string{`\Sent`, `\Draft`}

// errRuleUnsupported refuses a rule whose query holds a value the cache
// never evaluates on the account, such as a system flag in label: off
// Google. Rules match only the cache, so waiting does not help.
var errRuleUnsupported = &argError{
	msg:  "the rule's query holds a value the local cache cannot evaluate on this account",
	hint: `off Google label: takes a keyword, not a system flag such as \Seen; use is:read, is:unread or is:starred for those`,
}

// ruleCompileErr is a rule query the cache refuses, with fixed text: the
// compiler's error quotes the query's values.
func ruleCompileErr(err error) error {
	return &fixedTextError{msg: "the rule's query does not compile for the local cache", err: err}
}

// ruleFolderErr is a failure listing a rule's folders, with fixed text: an
// in: that names no folder on the account is NOT_FOUND without the name,
// anything else keeps its reason with messageErr's text.
func ruleFolderErr(err error) error {
	if errors.Is(err, mailstore.ErrNotFound) {
		return &fixedTextError{msg: "the rule's in: names no folder on this account", err: err}
	}
	return messageErr(err)
}

// ruleCacheErr is a failed read of the cache while matching a rule, with
// fixed text. A full disk turns the cache off, as on the search path.
func (s *Server) ruleCacheErr(err error) error {
	s.noteCacheErr(err)
	return &fixedTextError{msg: "reading the local cache failed", err: err}
}

// ruleScope is one rule on one account, ready to match.
type ruleScope struct {
	// acct is the account the scope covers.
	acct config.ResolvedAccount
	// ruleID is the rule's ID, "" for an unsaved definition.
	ruleID string
	// where is the compiled query, in: and account: split off.
	where cache.Where
	// needsBodies is set when the query has a text term, which a message's
	// body can satisfy.
	needsBodies bool
	// sources are the cached folders the rule covers, each with its
	// progress UIDVALIDITY. Empty matches nothing.
	sources []mailstore.Source
	// excludeLabels are the labels whose messages the rule skips.
	excludeLabels []string
}

// scopeRule parses and splits the query, resolves item: terms through
// s.itemThreads, compiles with cache.Compile, picks folders (Google: the
// synced All Mail; others: SearchFolders(in) minus Sent and Drafts when in
// is empty), and sets excludeLabels (Google, in empty: \Sent and \Draft).
// complete reports whether the cache can answer the rule completely on this
// account (cacheCanAnswer); when it cannot, reason is the verdict's reason.
//
// Scheduled and manual runs, and their dry-runs, pass complete true. A
// new-mail batch passes false, since it waits for the cache to apply the
// batch instead. An account without a cache worker, or a daemon whose cache
// is down, gives reason cache_disabled either way. What waiting does not
// change is an error: a query that does not parse or compile, a value the
// cache never evaluates on the account, an item: term naming no live item,
// an in: naming no folder. No error text holds rule text.
func (s *Server) scopeRule(ctx context.Context, ruleID string, d rules.Definition, acct config.ResolvedAccount, complete bool) (sc ruleScope, reason string, err error) {
	sc = ruleScope{acct: acct, ruleID: ruleID}
	n, err := query.Parse(d.Query)
	if err != nil {
		return sc, "", ruleQueryErr(err)
	}
	scope, rest := query.Split(n)
	threads, err := s.ruleItemThreads(ctx, rest, acct.Name)
	if err != nil {
		return sc, "", err
	}
	plan, err := cache.Compile(rest, scope.In, acct.Provider.Gmail, s.cacheClock.Now(), threads)
	if err != nil {
		return sc, "", ruleCompileErr(err)
	}
	if plan.Unsupported {
		return sc, "", errRuleUnsupported
	}
	sc.where, sc.needsBodies = plan.Where, plan.NeedsBodies
	ca := s.cacheAccount(acct.Name)
	if ca == nil || s.cache == nil {
		return sc, reasonCacheDisabled, nil
	}
	server := s.stores.get(acct)
	scoper, ok := server.(mailstore.Scoper)
	if !ok {
		return sc, cache.ReasonFolderNotSynced, nil
	}
	folders, err := ruleFolders(ctx, scoper, acct, scope.In)
	if err != nil {
		return sc, "", err
	}
	if acct.Provider.Gmail && scope.In == "" {
		sc.excludeLabels = slices.Clone(ruleDefaultExclude)
	}
	if sc.sources, err = s.cache.SourcesOf(ctx, acct.Name, folders); err != nil {
		return sc, "", s.ruleCacheErr(err)
	}
	if !complete {
		return sc, "", nil
	}
	p := &searchPlan{tree: rest, in: scope.In, itemThreads: map[string]map[string][]string{acct.Name: threads}}
	if v := (&messageService{s: s}).cacheCanAnswer(ctx, ca, server, acct, p); !v.ok {
		// Every verdict without ok has a reason once the query compiles
		// and is supported; stale stands in should that ever change.
		return sc, cmp.Or(v.reason, cache.ReasonStale), nil
	}
	return sc, "", nil
}

// ruleItemThreads maps each item: value of n to account's thread keys, as
// resolveItems does for a search, for one account. It is nil when n has no
// item: term. Links change, so a rule resolves them on every match.
func (s *Server) ruleItemThreads(ctx context.Context, n *query.Node, account string) (map[string][]string, error) {
	values := itemValues(n)
	if len(values) == 0 {
		return nil, nil
	}
	r := s.itemThreads
	if r == nil {
		return nil, errItemsOff
	}
	out := make(map[string][]string, len(values))
	for _, v := range values {
		if v == query.ItemNone {
			keys, err := r.LinkedThreadKeys(ctx, account)
			if err != nil {
				return nil, itemLookupErr(err)
			}
			out[v] = keys
			continue
		}
		byAccount, err := r.ThreadKeys(ctx, v)
		if err != nil {
			return nil, itemLookupErr(err)
		}
		out[v] = byAccount[account]
	}
	return out, nil
}

// ruleFolders is the folders a rule with in: value in covers on acct: the
// folders a search would pick, less the Sent and Drafts role folders off
// Google when in is empty (spec amendment 6). On Google the default scope
// is All Mail, and excludeLabels does the same job.
func ruleFolders(ctx context.Context, scoper mailstore.Scoper, acct config.ResolvedAccount, in string) ([]string, error) {
	folders, err := scoper.SearchFolders(ctx, in)
	if err != nil {
		return nil, ruleFolderErr(err)
	}
	if acct.Provider.Gmail || in != "" {
		return folders, nil
	}
	for _, role := range []string{"sent", "drafts"} {
		drop, err := scoper.SearchFolders(ctx, role)
		switch {
		case errors.Is(err, mailstore.ErrNotFound):
			continue // the account has no folder with the role
		case err != nil:
			return nil, ruleFolderErr(err)
		}
		folders = slices.DeleteFunc(folders, func(f string) bool { return slices.Contains(drop, f) })
	}
	return folders, nil
}

// rulePageSize is how many rows one cache read of a rule's matches
// returns: a page of a scheduled, manual or dry run, or of a batch.
const rulePageSize = 500

// errRuleCacheOff is a match on a daemon whose cache.db is not open.
var errRuleCacheOff = &fixedTextError{msg: "the local cache is not running", err: errNotReady}

// matchRows reads one page of sc's matches oldest first with o's batch,
// cursor and limit, in sc's sources and without its excluded labels. No
// sources match nothing, without a query.
func (s *Server) matchRows(ctx context.Context, sc ruleScope, o cache.MatchOptions) ([]cache.Row, error) {
	if s.cache == nil {
		return nil, errRuleCacheOff
	}
	if len(sc.sources) == 0 {
		return nil, nil
	}
	o.Sources, o.ExcludeLabels, o.Order = sc.sources, sc.excludeLabels, cache.OldestFirst
	rows, err := s.cache.MatchRows(ctx, sc.acct.Name, sc.where, o)
	if err != nil {
		return nil, s.ruleCacheErr(err)
	}
	return rows, nil
}

// matchBatch matches only the given UIDs per folder: a new-mail batch, or
// the stand-in batch of a trigger new dry-run. A folder missing from uids
// matches nothing, and so does an empty or nil uids, never the whole scope.
// Rows come oldest first, every page of them.
func (s *Server) matchBatch(ctx context.Context, sc ruleScope, uids map[string][]uint32) ([]cache.Row, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if s.cache == nil {
		return nil, errRuleCacheOff
	}
	var (
		out   []cache.Row
		after cache.MatchCursor
	)
	for {
		rows, err := s.matchRows(ctx, sc, cache.MatchOptions{UIDs: uids, After: after, Limit: rulePageSize})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < rulePageSize {
			return out, nil
		}
		after = cache.CursorOf(rows[len(rows)-1])
	}
}

// matchPage pages the whole scope oldest first: at most limit rows after
// the cursor, 1 to 1000. A zero cursor starts at the oldest match. A page
// shorter than limit is the last.
func (s *Server) matchPage(ctx context.Context, sc ruleScope, after cache.MatchCursor, limit int) ([]cache.Row, error) {
	return s.matchRows(ctx, sc, cache.MatchOptions{After: after, Limit: limit})
}

const (
	// dryRunNewest is how many of an account's newest cached messages a
	// dry-run of trigger new matches, standing in for a new-mail batch.
	dryRunNewest = 200
	// dryRunMatches is how many matches a dry-run returns per account.
	dryRunMatches = 50
)

var (
	// errRuleTrigger refuses a dry-run trigger other than new, schedule or
	// manual.
	errRuleTrigger = &argError{
		msg:  "trigger must be new, schedule or manual",
		hint: "leave trigger empty for a manual dry-run",
	}
	// errRuleAccountNotCovered refuses a dry-run of one account the rule
	// does not cover.
	errRuleAccountNotCovered = &argError{
		msg:  "the rule does not cover that account",
		hint: "leave account empty for every account the rule covers; its accounts and its query's account: terms decide which",
	}
)

// appliedLookup reads the act-once table: which of an account's message
// keys a rule has already handled. *rules.Store satisfies it.
type appliedLookup interface {
	Applied(ctx context.Context, ruleID, account string, keys []string) (map[string]rules.AppliedRow, error)
}

// ruleSkipped counts the rows of sc's rule the act-once table holds, in
// any outcome. An unsaved rule, or a daemon without the table, skips none.
func (s *Server) ruleSkipped(ctx context.Context, sc ruleScope, rows []cache.Row) (int, error) {
	if sc.ruleID == "" || s.ruleApplied == nil || len(rows) == 0 {
		return 0, nil
	}
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = ruleMessageKey(sc.acct, r)
	}
	done, err := s.ruleApplied.Applied(ctx, sc.ruleID, sc.acct.Name, keys)
	if err != nil {
		return 0, &fixedTextError{msg: "reading the rule's applied messages failed", err: err}
	}
	n := 0
	for _, k := range keys {
		if _, ok := done[k]; ok {
			n++
		}
	}
	return n, nil
}

// dryRunTally counts one account's dry-run matches and keeps the first
// dryRunMatches of them.
type dryRunTally struct {
	matched, skipped int
	rows             []cache.Row
}

// add counts a page of matches, skipped of which the act-once table holds.
func (d *dryRunTally) add(rows []cache.Row, skipped int) {
	d.matched += len(rows)
	d.skipped += skipped
	if room := dryRunMatches - len(d.rows); room > 0 {
		d.rows = append(d.rows, rows[:min(room, len(rows))]...)
	}
}

// dryRun runs a definition without acting, per covered account (or only
// account when set). Trigger new matches each account's newest
// dryRunNewest cached messages as a new-mail batch; schedule, manual and
// "" (manual) match the whole cache and need it complete. Each account
// reports its match count, how many the act-once table would skip (none
// for an unsaved rule, ruleID ""), its first dryRunMatches matches as
// search rows, oldest first, and the actions each would get, or a reason
// and no matches when its cache cannot answer. Nothing is written.
func (s *Server) dryRun(ctx context.Context, ruleID string, d rules.Definition, t rules.Trigger, account string) (*chainmailv1.DryRunRuleResponse, error) {
	switch t {
	case rules.TriggerNew, rules.TriggerSchedule, rules.TriggerManual, "":
	default:
		return nil, errRuleTrigger
	}
	accts, err := s.ruleAccounts(d)
	if err != nil {
		return nil, err
	}
	if account != "" {
		if !config.ValidName(account) {
			return nil, errInvalidAccountName
		}
		if _, err := s.opts.Config.Account(account); err != nil {
			return nil, err
		}
		accts = slices.DeleteFunc(accts, func(a config.ResolvedAccount) bool { return a.Name != account })
		if len(accts) == 0 {
			return nil, errRuleAccountNotCovered
		}
	}
	resp := &chainmailv1.DryRunRuleResponse{Accounts: make([]*chainmailv1.DryRunAccount, 0, len(accts))}
	for _, acct := range accts {
		out, err := s.dryRunAccount(ctx, ruleID, d, t, acct)
		if err != nil {
			return nil, err
		}
		resp.Accounts = append(resp.Accounts, out)
	}
	return resp, nil
}

// dryRunAccount is dryRun on one account.
func (s *Server) dryRunAccount(ctx context.Context, ruleID string, d rules.Definition, t rules.Trigger, acct config.ResolvedAccount) (*chainmailv1.DryRunAccount, error) {
	out := &chainmailv1.DryRunAccount{Account: acct.Name, Actions: ruleActionsProto(d.Actions)}
	newMail := t == rules.TriggerNew
	sc, reason, err := s.scopeRule(ctx, ruleID, d, acct, !newMail)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		out.Reason = reason
		return out, nil
	}
	var tally dryRunTally
	if newMail {
		tally, err = s.dryRunNewest(ctx, sc)
	} else {
		tally, err = s.dryRunScope(ctx, sc, rulePageSize)
	}
	switch {
	case errors.Is(err, errRuleCacheOff):
		// The cache went down after scopeRule found it: the account cannot
		// answer, as if scopeRule had seen it down, and the others still do.
		out.Reason = reasonCacheDisabled
		return out, nil
	case err != nil:
		return nil, err
	}
	out.Matched, out.Skipped = int64(tally.matched), int64(tally.skipped)
	out.Matches = make([]*chainmailv1.MessageRow, len(tally.rows))
	for i, r := range tally.rows {
		out.Matches[i] = rowFromSummary(r.Summary(), false, 0)
	}
	return out, nil
}

// dryRunNewest matches sc against the account's newest dryRunNewest
// cached messages, in every folder the cache tracks, as a new-mail batch
// of them would be matched: the rule's scope then drops what it does not
// cover.
func (s *Server) dryRunNewest(ctx context.Context, sc ruleScope) (dryRunTally, error) {
	ca := s.cacheAccount(sc.acct.Name)
	if ca == nil || s.cache == nil {
		return dryRunTally{}, errRuleCacheOff
	}
	folders, err := ca.Folders(ctx)
	if err != nil {
		return dryRunTally{}, s.ruleCacheErr(err)
	}
	all, err := s.cache.SourcesOf(ctx, sc.acct.Name, folders)
	if err != nil {
		return dryRunTally{}, s.ruleCacheErr(err)
	}
	batch := map[string][]uint32{}
	if len(all) > 0 {
		newest, err := s.cache.MatchRows(ctx, sc.acct.Name, cache.Where{}, cache.MatchOptions{Sources: all, Order: cache.NewestFirst, Limit: dryRunNewest})
		if err != nil {
			return dryRunTally{}, s.ruleCacheErr(err)
		}
		for _, r := range newest {
			batch[r.Folder] = append(batch[r.Folder], r.UID)
		}
	}
	rows, err := s.matchBatch(ctx, sc, batch)
	if err != nil {
		return dryRunTally{}, err
	}
	skipped, err := s.ruleSkipped(ctx, sc, rows)
	if err != nil {
		return dryRunTally{}, err
	}
	var tally dryRunTally
	tally.add(rows, skipped)
	return tally, nil
}

// dryRunScope matches sc's whole scope, oldest first, page rows at a time,
// counting every match and keeping the first dryRunMatches.
func (s *Server) dryRunScope(ctx context.Context, sc ruleScope, page int) (dryRunTally, error) {
	var (
		tally dryRunTally
		after cache.MatchCursor
	)
	for {
		rows, err := s.matchPage(ctx, sc, after, page)
		if err != nil {
			return dryRunTally{}, err
		}
		skipped, err := s.ruleSkipped(ctx, sc, rows)
		if err != nil {
			return dryRunTally{}, err
		}
		tally.add(rows, skipped)
		if len(rows) < page {
			return tally, nil
		}
		after = cache.CursorOf(rows[len(rows)-1])
	}
}
