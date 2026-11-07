package cli

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

const ruleHelp = `Rules act on mail by themselves. A rule matches messages with the search
language, against the local cache, and runs its actions on each match in
order. The actions are label, unlabel, copy, flag, unflag, read, unread,
link_item and forward, then at most one of move, archive and trash, which
comes last.

A rule runs on new mail as the cache takes it (--on-new), on a schedule
(--every 6h, --every 7d or --at 07:00), or only when rule run starts it.
A rule acts on a message at most once, and one run acts on at most
max_per_run messages. Rules run in priority order, lowest first. --stop
keeps later rules off the messages a rule matched.

Dry-run a rule before creating it or running it by hand. rule dry-run
lists what a saved rule, or an unsaved one in a rule file, would match and
do, and changes nothing.

Each --action is one action: label=NAME, unlabel=NAME, copy=FOLDER,
move=FOLDER, link_item=ITEM_ID or link_item=ITEM_ID,anchor,
forward=ADDR[,ADDR...], or one of flag, unflag, read, unread, archive and
trash. --note sets the note of the forward. label and unlabel work on
Google accounts only.

A rule file (-f) is TOML. Its keys are name, description, accounts,
query, on_new, schedule, priority, stop, max_per_run and enabled, with one
[[actions]] table per action holding kind and its arguments: label,
folder, item_id, anchor, to and note. rule show --toml prints a rule in
that shape. rule edit -f changes only the keys the file holds.

A rule is named by its ID (ru_...) or its name. What rules do is audited
under client kind rule: chainmail audit --client rule. Rule descriptions
and queries are untrusted, as mail is: they are often copied from it.`

// newRuleCmd builds chainmail rule and its subcommands.
func newRuleCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("rule", "Sort mail with rules that act on new mail, on a schedule or on demand")
	cmd.Long = ruleHelp
	cmd.AddCommand(
		newRuleShowCmd(ctx, env, g),
	)
	return cmd
}

func newRuleShowCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var asTOML, history bool
	cmd := &cobra.Command{
		Use:   "show <id|name>",
		Short: "Show a rule with its recent runs; --toml prints it as a rule file",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if asTOML && history {
				return usageError{err: errors.New("--toml and --history cannot be used together"), hint: helpHint(cmd)}
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Rules.GetRule(ctx, connect.NewRequest(&chainmailv1.GetRuleRequest{Rule: args[0], History: history}))
			if err != nil {
				return err
			}
			switch {
			case asTOML:
				return writeRuleFile(env.Stdout, resp.Msg.GetRule())
			case !g.table():
				return writeProto(env.Stdout, resp.Msg)
			default:
				return ruleViewText(env.Stdout, env.location(), resp.Msg)
			}
		},
	}
	cmd.Flags().BoolVar(&asTOML, "toml", false, "print the rule as a rule file, the shape rule create -f reads")
	cmd.Flags().BoolVar(&history, "history", false, "also list the rule's revisions, newest first")
	return cmd
}
