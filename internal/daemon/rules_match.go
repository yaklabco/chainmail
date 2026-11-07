package daemon

import (
	"slices"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
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
