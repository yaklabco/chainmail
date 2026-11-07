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

// ruleSpecs is how the rule commands complete, by command path. specs
// merges it in.
func (cp *completer) ruleSpecs() map[string]compSpec {
	live := cp.ruleRefs(1, false)
	return map[string]compSpec{
		"chainmail rule":      {args: noComplete},
		"chainmail rule show": {args: live},
	}
}
