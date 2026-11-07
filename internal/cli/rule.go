package cli

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
	"github.com/yaklabco/chainmail/internal/rules"
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

A rule is named by its ID (ru_...) or its name. rule restore takes the ID
alone, which rule list --deleted shows. What rules do is audited under
client kind rule: chainmail audit --client rule. Rule descriptions and
queries are untrusted, as mail is: they are often copied from it.`

// The triggers and run outcomes rule commands take, in the order help and
// completion offer them.
var (
	ruleTriggers = []string{"new", "schedule", "manual"}
	ruleOutcomes = []string{"ok", "capped", "failed", "cache_not_ready", "interrupted"}
)

// newRuleCmd builds chainmail rule and its subcommands.
func newRuleCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("rule", "Sort mail with rules that act on new mail, on a schedule or on demand")
	cmd.Long = ruleHelp
	cmd.AddCommand(
		newRuleListCmd(ctx, env, g),
		newRuleShowCmd(ctx, env, g),
		newRuleCreateCmd(ctx, env, g),
		newRuleEditCmd(ctx, env, g),
		newRuleSwitchCmd(ctx, env, g, true),
		newRuleSwitchCmd(ctx, env, g, false),
		newRuleDeleteCmd(ctx, env, g),
		newRuleRestoreCmd(ctx, env, g),
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

func newRuleListCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	req := &chainmailv1.ListRulesRequest{}
	var enabled, disabled bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List rules in priority order, lowest first",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case enabled && disabled:
				return usageError{err: errors.New("--enabled and --disabled cannot be used together"), hint: helpHint(cmd)}
			case enabled:
				req.Enabled = proto.Bool(true)
			case disabled:
				req.Enabled = proto.Bool(false)
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Rules.ListRules(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return ruleListTable(env.Stdout, env.location(), resp.Msg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Account, "account", "", "only rules that cover this account, by name or by covering every account")
	f.BoolVar(&enabled, "enabled", false, "only enabled rules")
	f.BoolVar(&disabled, "disabled", false, "only disabled rules")
	f.BoolVar(&req.Deleted, "deleted", false, "only deleted rules")
	return cmd
}

// ruleCall sends one request naming rule, by ID or name, and returns the
// response and the rule it holds.
type ruleCall func(ctx context.Context, c *client.Client, rule string) (proto.Message, *chainmailv1.Rule, error)

// newRuleRefCmd builds a command that takes one rule, sends call, and
// prints the response, or on a table verb and the rule.
func newRuleRefCmd(ctx context.Context, env Env, g *globals, use, short, verb string, call ruleCall) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			msg, rule, err := call(ctx, c, args[0])
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, msg)
			}
			return writeString(env.Stdout, ruleChangedText(verb, rule))
		},
	}
}

// newRuleSwitchCmd builds rule enable, or with enable false rule disable:
// an UpdateRule of the enabled field alone.
func newRuleSwitchCmd(ctx context.Context, env Env, g *globals, enable bool) *cobra.Command {
	use, short, verb := "enable <id|name>", "Turn a rule on; this clears an automatic disable and its failure count", "enabled"
	if !enable {
		use, short, verb = "disable <id|name>", "Turn a rule off until rule enable turns it on", "disabled"
	}
	return newRuleRefCmd(ctx, env, g, use, short, verb, func(ctx context.Context, c *client.Client, rule string) (proto.Message, *chainmailv1.Rule, error) {
		req := &chainmailv1.UpdateRuleRequest{
			Rule:       rule,
			Patch:      &chainmailv1.RuleDefinition{Enabled: proto.Bool(enable)},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enabled"}},
		}
		resp, err := c.Rules.UpdateRule(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, nil, err
		}
		return resp.Msg, resp.Msg.GetRule(), nil
	})
}

func newRuleDeleteCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return newRuleRefCmd(ctx, env, g, "delete <id|name>", "Delete a rule, softly; rule restore brings it back", "deleted",
		func(ctx context.Context, c *client.Client, rule string) (proto.Message, *chainmailv1.Rule, error) {
			resp, err := c.Rules.DeleteRule(ctx, connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: rule}))
			if err != nil {
				return nil, nil, err
			}
			return resp.Msg, resp.Msg.GetRule(), nil
		})
}

// newRuleRestoreCmd builds rule restore. It takes a rule ID alone, never a
// name: a live rule may hold a deleted rule's name by now.
func newRuleRestoreCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newRuleRefCmd(ctx, env, g, "restore <id>", "Bring back a deleted rule by its ID; rule list --deleted shows it", "restored",
		func(ctx context.Context, c *client.Client, rule string) (proto.Message, *chainmailv1.Rule, error) {
			resp, err := c.Rules.RestoreRule(ctx, connect.NewRequest(&chainmailv1.RestoreRuleRequest{Rule: rule}))
			if err != nil {
				return nil, nil, err
			}
			return resp.Msg, resp.Msg.GetRule(), nil
		})
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := exactArgs(1)(cmd, args); err != nil {
			return err
		}
		if !rules.ValidID(args[0]) {
			return usageError{err: errors.New("restore takes a rule ID (ru_...), not a name"), hint: "find it with chainmail rule list --deleted"}
		}
		return nil
	}
	return cmd
}
