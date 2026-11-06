package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/query"
)

// auditActionRows is how many of the newest audit rows audit --action
// reads for action names: the daemon's per-page maximum.
const auditActionRows = 1000

// auditBeginning is a since that reaches the first audit row. Without a
// since the daemon reads only the last 24 hours.
var auditBeginning = time.Unix(0, 0)

// groups completes watch group names, as one positional argument or a flag value.
func (cp *completer) groups(maxArgs int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var groups []*chainmailv1.GroupStatus
		ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
			resp, err := c.Watch.ListGroups(ctx, connect.NewRequest(&chainmailv1.ListGroupsRequest{}))
			if err != nil {
				return err
			}
			groups = resp.Msg.GetGroups()
			return nil
		})
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []cobra.Completion
		for _, g := range groups {
			if safeValue(g.GetName()) && strings.HasPrefix(g.GetName(), toComplete) {
				desc := fmt.Sprintf("acked %d, lag %d", g.GetAckedId(), g.GetLag())
				out = append(out, cobra.CompletionWithDesc(g.GetName(), desc))
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// secretNames completes secret set's name: accounts/<account> for each
// password account, and the OAuth client secrets already stored, each
// described as stored or not set.
func (cp *completer) secretNames(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var accounts []*chainmailv1.Account
	var stored []string
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		ar, err := c.Account.ListAccounts(ctx, connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
		if err != nil {
			return err
		}
		sr, err := c.Admin.GetStatus(ctx, connect.NewRequest(&chainmailv1.GetStatusRequest{}))
		if err != nil {
			return err
		}
		accounts, stored = ar.Msg.GetAccounts(), sr.Msg.GetSecretNames()
		return nil
	})
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	state := func(name string) string {
		if slices.Contains(stored, name) {
			return "stored"
		}
		return "not set"
	}
	var names []string
	for _, a := range accounts {
		if a.GetAuthMethod() == string(provider.AuthPassword) {
			names = append(names, "accounts/"+a.GetName())
		}
	}
	for _, s := range stored {
		if strings.HasPrefix(s, "oauth_clients/") {
			names = append(names, s)
		}
	}
	slices.Sort(names)
	var out []cobra.Completion
	for _, n := range slices.Compact(names) {
		if safeValue(n) && strings.HasPrefix(n, toComplete) {
			out = append(out, cobra.CompletionWithDesc(n, state(n)))
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// auditActions completes audit --action with the actions in the newest
// auditActionRows rows of the audit log.
func (cp *completer) auditActions(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var actions []string
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		req := &chainmailv1.ReadAuditRequest{Since: timestamppb.New(auditBeginning), Limit: auditActionRows}
		resp, err := c.Admin.ReadAudit(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		for _, e := range resp.Msg.GetEntries() {
			actions = append(actions, e.GetAction())
		}
		return nil
	})
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	slices.Sort(actions)
	var out []cobra.Completion
	for _, a := range slices.Compact(actions) {
		if safeValue(a) && strings.HasPrefix(a, toComplete) {
			out = append(out, a)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// searchOperators are the search operators, from searchHelp.
var searchOperators = []cobra.Completion{
	cobra.CompletionWithDesc("from:", "sender address or name contains"),
	cobra.CompletionWithDesc("to:", "recipient address or name contains"),
	cobra.CompletionWithDesc("cc:", "copied address or name contains"),
	cobra.CompletionWithDesc("bcc:", "blind-copied address or name contains"),
	cobra.CompletionWithDesc("subject:", "subject contains"),
	cobra.CompletionWithDesc("is:", "unread, read or starred"),
	cobra.CompletionWithDesc("newer_than:", "within 7d, 2w, 3m or 1y"),
	cobra.CompletionWithDesc("older_than:", "before 7d, 2w, 3m or 1y ago"),
	cobra.CompletionWithDesc("after:", "on or after YYYY-MM-DD"),
	cobra.CompletionWithDesc("before:", "before YYYY-MM-DD"),
	cobra.CompletionWithDesc("larger:", "bigger than 10K or 5M"),
	cobra.CompletionWithDesc("smaller:", "smaller than 10K or 5M"),
	cobra.CompletionWithDesc("has:", "has:attachment, Google accounts"),
	cobra.CompletionWithDesc("filename:", "attachment name, Google accounts"),
	cobra.CompletionWithDesc("label:", "Gmail label, Google accounts"),
	cobra.CompletionWithDesc("item:", "linked to this item or one below it; item:none"),
	cobra.CompletionWithDesc("in:", "a role or folder name"),
	cobra.CompletionWithDesc("account:", "only this account"),
}

// inRoles are the in: values that name a role on every account.
var inRoles = []string{"inbox", "sent", "drafts", "archive", "trash", "spam", "junk", "anywhere"}

// scopeOperators choose where a search runs, so the query validator
// refuses them negated.
var scopeOperators = []string{string(query.FieldIn), string(query.FieldAccount)}

// searchTerms completes one search term: an operator, or after one the
// values it takes. A leading - (negation) is kept, and offers no operator
// that cannot be negated.
func (cp *completer) searchTerms(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	neg := ""
	if strings.HasPrefix(toComplete, "-") {
		neg, toComplete = "-", toComplete[1:]
	}
	op, value, hasOp := strings.Cut(toComplete, ":")
	if !hasOp {
		var ops []cobra.Completion
		for _, o := range withPrefix(searchOperators, toComplete) {
			name, _, _ := strings.Cut(o, ":")
			if neg == "" || !slices.Contains(scopeOperators, name) {
				ops = append(ops, neg+o)
			}
		}
		return ops, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	if neg != "" && slices.Contains(scopeOperators, op) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var vals []string
	switch op {
	case "is":
		vals = []string{"unread", "read", "starred"}
	case "has":
		vals = []string{"attachment"}
	case "account":
		list, _ := cp.listAccounts(cmd)
		for _, a := range list {
			if querySafe(a.GetName()) {
				vals = append(vals, a.GetName())
			}
		}
	case "in":
		vals = append(slices.Clone(inRoles), cp.searchFolders(cmd, anyAccount, nil)...)
	case "label":
		vals = cp.searchFolders(cmd, cp.googleAccounts, labelFolder)
	case "item":
		vals = []string{"none"}
		// A failed lookup leaves rows empty, which offers only none.
		rows, _ := cp.listItemRows(cmd, false)
		for _, r := range rows {
			if querySafe(r.GetId()) {
				vals = append(vals, r.GetId())
			}
		}
	}
	slices.Sort(vals)
	var out []cobra.Completion
	for _, v := range slices.Compact(vals) {
		if strings.HasPrefix(v, value) {
			out = append(out, neg+op+":"+v)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// querySafe says the query language reads op:s as op with exactly the
// value s, with no quoting: s holds no whitespace, parenthesis or quote,
// which its lexer treats as syntax, and is a safe candidate.
func querySafe(s string) bool {
	return safeValue(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ')' || r == '"'
	})
}

// searchFolders returns the names of the folders on the accounts selects
// that keep accepts (nil for all) and a query can name without quoting.
func (cp *completer) searchFolders(cmd *cobra.Command, selects accountSelector, keep func(*chainmailv1.Folder) bool) []string {
	accounts, ok := selects(cmd, nil)
	if !ok {
		return nil
	}
	// A failed lookup leaves lists empty, which offers no folder.
	lists, _ := cp.listFolders(cmd, accounts)
	var names []string
	for _, af := range lists {
		for _, f := range af.GetFolders() {
			if name := f.GetName(); querySafe(name) && (keep == nil || keep(f)) {
				names = append(names, name)
			}
		}
	}
	return names
}
