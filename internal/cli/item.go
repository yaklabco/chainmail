package cli

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

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

// The values item commands take for an item's type, state, area and
// priority, in the order help and completion offer them.
var (
	itemTypes      = []string{"objective", "key-result", "initiative", "epic", "feature", "story", "bug", "support-ticket", "task"}
	itemStates     = []string{"backlog", "in-progress", "in-review", "done"}
	itemAreas      = []string{"work", "personal"}
	itemPriorities = []string{"p0", "p1", "p2", "p3"}
)

// newItemCmd builds chainmail item and its subcommands.
func newItemCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("item", "Keep work items and file mail threads against them")
	cmd.Long = itemHelp
	cmd.AddCommand(
		newItemShowCmd(ctx, env, g),
		newItemListCmd(ctx, env, g),
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

func newItemListCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	req := &chainmailv1.ListItemsRequest{}
	var ongoing, notOngoing bool
	var changedSince int64
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List items in tree order; --inbox lists the tasks still to sort",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case ongoing && notOngoing:
				return usageError{err: errors.New("--ongoing and --no-ongoing cannot be used together"), hint: helpHint(cmd)}
			case ongoing:
				req.Ongoing = proto.Bool(true)
			case notOngoing:
				req.Ongoing = proto.Bool(false)
			}
			if cmd.Flags().Changed("changed-since") {
				if changedSince < 0 {
					return usageError{err: errors.New("--changed-since must not be negative"), hint: helpHint(cmd)}
				}
				req.ChangedSince = proto.Int64(changedSince)
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.ListItems(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return itemListTable(env.Stdout, resp.Msg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Type, "type", "", "only items of this type")
	f.StringVar(&req.State, "state", "", "only items in this state")
	f.StringVar(&req.Area, "area", "", "only items whose area, their own or inherited, is this")
	f.StringVar(&req.Under, "under", "", "only the items below this one, not including it")
	f.BoolVar(&req.Inbox, "inbox", false, "only the inbox: tasks with no parent")
	f.BoolVar(&ongoing, "ongoing", false, "only ongoing items, the Admin initiatives")
	f.BoolVar(&notOngoing, "no-ongoing", false, "leave out ongoing items")
	f.BoolVar(&req.IncludeDeleted, "deleted", false, "include deleted items")
	f.Int64Var(&changedSince, "changed-since", 0, "only items changed after this revision, deleted ones included, in revision order")
	f.Int32Var(&req.Limit, "limit", 0, "rows per page, 1 to 500 (default 100)")
	f.StringVar(&req.PageToken, "page-token", "", "continue from the previous page's token")
	return cmd
}
