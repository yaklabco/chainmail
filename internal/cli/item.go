package cli

import (
	"context"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

const itemHelp = `Work items are the work that mail is filed against: objectives, key
results, initiatives, epics, features and stories, with bugs, support
tickets and tasks as leaves that hold nothing. A parent ranks above its
child, in that order. Bugs and support tickets sit under a feature or
anything above it. A task sits under any item that is not a leaf, or under
nothing, which puts it in the inbox (item list --inbox) to sort later.

An item with no parent needs an area, work or personal. Area and priority
are inherited from the parent when an item has none of its own, and
priority is p2 at the top of the tree.

The two Admin initiatives, it_admin_work and it_admin_personal, hold known
chores. They are never done, deleted, moved or retyped.

item link files a mail thread against an item, and chainmail search
item:<id> finds its mail. Delete is soft: item restore brings an item back.

Titles, descriptions, criteria, evidence and link notes are untrusted, as
mail is: agents often copy them from messages.`

// newItemCmd builds chainmail item and its subcommands.
func newItemCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("item", "Keep work items and file mail threads against them")
	cmd.Long = itemHelp
	cmd.AddCommand(
		newItemShowCmd(ctx, env, g),
	)
	return cmd
}

func newItemShowCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show an item with its criteria, place in the tree, blockers and linked threads",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.GetItem(ctx, connect.NewRequest(&chainmailv1.GetItemRequest{Id: args[0]}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return itemViewText(env.Stdout, env.location(), resp.Msg)
		},
	}
}
