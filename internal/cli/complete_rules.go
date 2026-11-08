package cli

import (
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
)

// listRules asks the daemon for the live rules, or for the deleted ones
// when deleted is set.
func (cp *completer) listRules(cmd *cobra.Command, deleted bool) ([]*chainmailv1.Rule, bool) {
	var list []*chainmailv1.Rule
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Rules.ListRules(ctx, connect.NewRequest(&chainmailv1.ListRulesRequest{Deleted: deleted}))
		if err != nil {
			return err
		}
		list = resp.Msg.GetRules()
		return nil
	})
	return list, ok
}

// ruleRefs completes rules as up to maxArgs positional arguments, or a
// flag value when maxArgs is 0. Live rules are offered by name, described
// by ID and description. With deleted set, deleted rules are offered by
// ID, described by name, since a live rule may hold the name by now.
func (cp *completer) ruleRefs(maxArgs int, deleted bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		list, ok := cp.listRules(cmd, deleted)
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []cobra.Completion
		for _, r := range list {
			value, desc := r.GetName(), r.GetId()
			if d := r.GetDescription().GetUntrusted(); d != "" {
				desc += " · " + d
			}
			if deleted {
				value, desc = r.GetId(), r.GetName()
			}
			if !safeValue(value) || !strings.HasPrefix(value, toComplete) || slices.Contains(args, value) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(value, completionText(desc)))
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// ruleActionChoices are the --action forms completion offers, in the
// model's order. A kind that takes a value ends in =.
var ruleActionChoices = []cobra.Completion{
	cobra.CompletionWithDesc("label=", "add a Gmail label"),
	cobra.CompletionWithDesc("unlabel=", "remove a Gmail label"),
	cobra.CompletionWithDesc("copy=", "copy to a folder"),
	cobra.CompletionWithDesc("flag", "flag the message"),
	cobra.CompletionWithDesc("unflag", "unflag the message"),
	cobra.CompletionWithDesc("read", "mark it read"),
	cobra.CompletionWithDesc("unread", "mark it unread"),
	cobra.CompletionWithDesc("link_item=", "file the thread against an item; add ,anchor"),
	cobra.CompletionWithDesc("forward=", "forward to addresses, comma separated"),
	cobra.CompletionWithDesc("move=", "move to a folder, last"),
	cobra.CompletionWithDesc("archive", "archive it, last"),
	cobra.CompletionWithDesc("trash", "move it to Trash, last"),
}

// ruleActions completes --action: a kind, then after link_item= the IDs of
// live items, each described by its type and title. Other values are free
// text.
func (cp *completer) ruleActions(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	kind, value, hasValue := strings.Cut(toComplete, "=")
	if !hasValue {
		return withPrefix(ruleActionChoices, toComplete), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	if kind != "link_item" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	rows, ok := cp.listItemRows(cmd, false)
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, r := range rows {
		id := r.GetId()
		if !safeValue(id) || !strings.HasPrefix(id, value) {
			continue
		}
		out = append(out, cobra.CompletionWithDesc("link_item="+id, completionText(r.GetType()+" · "+r.GetTitle().GetUntrusted())))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// ruleFieldSpecs completes the field flags rule create and rule edit share.
func (cp *completer) ruleFieldSpecs(accounts cobra.CompletionFunc) map[string]cobra.CompletionFunc {
	return map[string]cobra.CompletionFunc{
		"name": noComplete, "description": noComplete, "account": accounts, "query": noComplete,
		"every": fixed("15m", "1h", "6h", "1d", "7d"), "at": noComplete, "priority": noComplete,
		"max-per-run": noComplete, "action": cp.ruleActions, "note": noComplete, "file": files,
	}
}

// ruleSpecs is how the rule commands complete, by command path. specs
// merges it in.
func (cp *completer) ruleSpecs() map[string]compSpec {
	allAccounts := cp.accounts(nil, 0)
	live := cp.ruleRefs(1, false)
	return map[string]compSpec{
		"chainmail rule":        {args: noComplete},
		"chainmail rule show":   {args: live},
		"chainmail rule list":   {args: noComplete, flags: map[string]cobra.CompletionFunc{"account": allAccounts}},
		"chainmail rule create": {args: noComplete, flags: cp.ruleFieldSpecs(allAccounts)},
		"chainmail rule edit": {args: live, flags: mergeFlagCompletions(cp.ruleFieldSpecs(allAccounts), map[string]cobra.CompletionFunc{
			"expected-revision": noComplete,
		})},
		"chainmail rule enable":  {args: live},
		"chainmail rule disable": {args: live},
		"chainmail rule delete":  {args: live},
		"chainmail rule restore": {args: cp.ruleRefs(1, true)},
		"chainmail rule dry-run": {args: live, flags: map[string]cobra.CompletionFunc{
			"file": files, "trigger": fixed(ruleTriggers...), "account": allAccounts,
		}},
		"chainmail rule run": {args: live, flags: map[string]cobra.CompletionFunc{"max": noComplete, "account": allAccounts}},
		"chainmail rule runs": {args: live, flags: map[string]cobra.CompletionFunc{
			"account": allAccounts, "trigger": fixed(ruleTriggers...), "outcome": fixed(ruleOutcomes...),
			"limit": noComplete, "before-id": noComplete,
		}},
		"chainmail rule status": {args: noComplete},
	}
}
