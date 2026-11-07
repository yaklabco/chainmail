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
