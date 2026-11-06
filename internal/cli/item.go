package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

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

// itemMaskOrder is the order update and move list UpdateItem mask paths in:
// the order of UpdateItemRequest's fields, so a request never depends on
// the order the flags were given in.
var itemMaskOrder = []string{"type", "title", "description", "state", "area", "priority", "start_date", "due_date", "estimate", "parent_id"}

// itemClearFields are the names --clear takes, in the order completion
// offers them.
var itemClearFields = []string{"area", "priority", "start", "due", "estimate", "description"}

// itemFlagPaths maps each item update flag, and so each --clear name, to
// the UpdateItem mask path it changes.
var itemFlagPaths = map[string]string{
	"type": "type", "title": "title", "description": "description", "description-file": "description",
	"state": "state", "area": "area", "priority": "priority", "start": "start_date", "due": "due_date", "estimate": "estimate",
}

// newItemCmd builds chainmail item and its subcommands.
func newItemCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("item", "Keep work items and file mail threads against them")
	cmd.Long = itemHelp
	cmd.AddCommand(
		newItemCreateCmd(ctx, env, g),
		newItemShowCmd(ctx, env, g),
		newItemListCmd(ctx, env, g),
		newItemUpdateCmd(ctx, env, g),
		newItemMoveCmd(ctx, env, g),
		newItemDeleteCmd(ctx, env, g),
		newItemRestoreCmd(ctx, env, g),
		newItemCriteriaCmd(ctx, env, g),
		newItemBlockCmd(ctx, env, g),
		newItemLinkCmd(ctx, env, g),
		newItemUnlinkCmd(ctx, env, g),
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

// readDescription returns the description --description or
// --description-file gives, and whether either was given. A file that is
// not UTF-8 is a usage error.
func readDescription(cmd *cobra.Command, text, file string) (string, bool, error) {
	fromText, fromFile := cmd.Flags().Changed("description"), cmd.Flags().Changed("description-file")
	switch {
	case fromText && fromFile:
		return "", false, usageError{err: errors.New("--description and --description-file cannot be used together"), hint: helpHint(cmd)}
	case fromText:
		return text, true, nil
	case !fromFile:
		return "", false, nil
	}
	b, err := readFile(file, "--description-file")
	if err != nil {
		return "", false, err
	}
	if !utf8.Valid(b) {
		return "", false, usageError{err: errors.New("--description-file is not UTF-8"), hint: helpHint(cmd)}
	}
	return string(b), true, nil
}

func newItemCreateCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	req := &chainmailv1.CreateItemRequest{}
	var description, descriptionFile string
	var estimate float64
	cmd := &cobra.Command{
		Use:   "create <type> <title>",
		Short: "Create a work item; a task with no --parent goes to the inbox",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			desc, _, err := readDescription(cmd, description, descriptionFile)
			if err != nil {
				return err
			}
			req.Type, req.Title, req.Description = args[0], args[1], desc
			if cmd.Flags().Changed("estimate") {
				req.Estimate = proto.Float64(estimate)
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.CreateItem(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, createdText(resp.Msg))
		},
	}
	f := cmd.Flags()
	f.StringVar(&description, "description", "", "the description, as Markdown")
	f.StringVar(&descriptionFile, "description-file", "", "read the description from this file")
	f.StringVar(&req.ParentId, "parent", "", "the parent item's ID (default: none, and a task then goes to the inbox)")
	f.StringVar(&req.Area, "area", "", "work or personal; required with no --parent, inherited otherwise")
	f.StringVar(&req.Priority, "priority", "", "p0 to p3 (default: the parent's, or p2 with no parent)")
	f.StringVar(&req.State, "state", "", "backlog, in-progress, in-review or done (default backlog)")
	f.StringVar(&req.StartDate, "start", "", "the start date, YYYY-MM-DD")
	f.StringVar(&req.DueDate, "due", "", "the due date, YYYY-MM-DD")
	f.Float64Var(&estimate, "estimate", 0, "the estimate, a number in a unit of your choosing such as days")
	f.StringArrayVar(&req.Criteria, "criterion", nil, "an acceptance criterion (repeatable, kept in order)")
	f.StringVar(&req.IdempotencyKey, "idempotency-key", "", "a key that makes a retry return the first item instead of making a second")
	return cmd
}

func newItemUpdateCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	req := &chainmailv1.UpdateItemRequest{}
	var description, descriptionFile string
	var estimate float64
	var clears []string
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Change an item's fields; --clear unsets those that can be unset",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			desc, hasDesc, err := readDescription(cmd, description, descriptionFile)
			if err != nil {
				return err
			}
			paths, err := updateMask(cmd, clears, hasDesc)
			if err != nil {
				return err
			}
			req.Id, req.Description, req.UpdateMask = args[0], desc, &fieldmaskpb.FieldMask{Paths: paths}
			if cmd.Flags().Changed("estimate") {
				req.Estimate = proto.Float64(estimate)
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.UpdateItem(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, updatedText(resp.Msg.GetItem()))
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Type, "type", "", "the new type, which must fit the parent and every child")
	f.StringVar(&req.Title, "title", "", "the new title")
	f.StringVar(&description, "description", "", "the new description, as Markdown")
	f.StringVar(&descriptionFile, "description-file", "", "read the new description from this file")
	f.StringVar(&req.State, "state", "", "backlog, in-progress, in-review or done")
	f.StringVar(&req.Area, "area", "", "work or personal")
	f.StringVar(&req.Priority, "priority", "", "p0 to p3")
	f.StringVar(&req.StartDate, "start", "", "the start date, YYYY-MM-DD")
	f.StringVar(&req.DueDate, "due", "", "the due date, YYYY-MM-DD")
	f.Float64Var(&estimate, "estimate", 0, "the estimate")
	f.StringSliceVar(&clears, "clear", nil, "unset these, comma separated: area, priority, start, due, estimate, description")
	return cmd
}

// updateMask returns the UpdateItem mask paths for the field flags cmd was
// given and the names in clears, in itemMaskOrder. hasDescription says
// --description or --description-file was given. An unknown --clear name,
// a field both set and cleared, or nothing to change is a usage error.
func updateMask(cmd *cobra.Command, clears []string, hasDescription bool) ([]string, error) {
	set := map[string]bool{"description": hasDescription}
	for _, name := range []string{"type", "title", "state", "area", "priority", "start", "due", "estimate"} {
		if cmd.Flags().Changed(name) {
			set[itemFlagPaths[name]] = true
		}
	}
	cleared := map[string]bool{}
	for _, name := range clears {
		name = strings.TrimSpace(name)
		if !slices.Contains(itemClearFields, name) {
			return nil, usageError{err: fmt.Errorf("unknown --clear %q: want area, priority, start, due, estimate or description", name), hint: helpHint(cmd)}
		}
		path := itemFlagPaths[name]
		if set[path] {
			return nil, usageError{err: fmt.Errorf("--clear %s cannot be used with the flag that sets it", name), hint: helpHint(cmd)}
		}
		cleared[path] = true
	}
	var paths []string
	for _, p := range itemMaskOrder {
		if set[p] || cleared[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, usageError{err: errors.New("nothing to change: give a field flag or --clear"), hint: helpHint(cmd)}
	}
	return paths, nil
}

func newItemMoveCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var root bool
	cmd := &cobra.Command{
		Use:   "move <id> (<parent-id> | --root)",
		Short: "Move an item under another, or to the top of the tree with --root",
		Args:  rangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case root && len(args) == 2:
				return usageError{err: errors.New("--root and a parent ID cannot be used together"), hint: helpHint(cmd)}
			case !root && len(args) == 1:
				return usageError{err: errors.New("give a parent ID or --root"), hint: helpHint(cmd)}
			}
			req := &chainmailv1.UpdateItemRequest{Id: args[0], UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"parent_id"}}}
			if !root {
				req.ParentId = args[1]
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.UpdateItem(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, movedText(resp.Msg.GetItem()))
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "move the item to the top of the tree; a task goes to the inbox")
	return cmd
}

func newItemDeleteCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var cascade bool
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an item, softly; --cascade deletes the items below it too",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.DeleteItem(ctx, connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: args[0], Cascade: cascade}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, idsText("deleted", resp.Msg.GetIds(), resp.Msg.GetRevision()))
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "also delete every item below it")
	return cmd
}

func newItemRestoreCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var cascade bool
	cmd := &cobra.Command{
		Use:   "restore <id>",
		Short: "Bring back a deleted item; --cascade brings back the items deleted with it",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.RestoreItem(ctx, connect.NewRequest(&chainmailv1.RestoreItemRequest{Id: args[0], Cascade: cascade}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, idsText("restored", resp.Msg.GetIds(), resp.Msg.GetRevision()))
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "also restore the items deleted with it")
	return cmd
}

// newItemCriteriaCmd builds chainmail item criteria and its subcommands.
func newItemCriteriaCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("criteria", "Add, edit, remove, order and tick an item's acceptance criteria")
	cmd.AddCommand(
		newCriteriaEditCmd(ctx, env, g, "add <id> <text>...", "Add criteria to the end of an item's list, in order", minArgs(2),
			func(a []string) *chainmailv1.EditCriteriaRequest {
				return &chainmailv1.EditCriteriaRequest{ItemId: a[0], Add: a[1:]}
			}),
		newCriteriaEditCmd(ctx, env, g, "edit <id> <criterion-id> <text>", "Change the text of one criterion", exactArgs(3),
			func(a []string) *chainmailv1.EditCriteriaRequest {
				return &chainmailv1.EditCriteriaRequest{ItemId: a[0], Edit: []*chainmailv1.CriterionText{{Id: a[1], Text: a[2]}}}
			}),
		newCriteriaEditCmd(ctx, env, g, "remove <id> <criterion-id>...", "Remove criteria from an item", minArgs(2),
			func(a []string) *chainmailv1.EditCriteriaRequest {
				return &chainmailv1.EditCriteriaRequest{ItemId: a[0], Remove: a[1:]}
			}),
		newCriteriaEditCmd(ctx, env, g, "order <id> <criterion-id>...", "Put an item's criteria in a new order, naming every one once", minArgs(2),
			func(a []string) *chainmailv1.EditCriteriaRequest {
				return &chainmailv1.EditCriteriaRequest{ItemId: a[0], Order: a[1:]}
			}),
		newCriteriaTickCmd(ctx, env, g, true),
		newCriteriaTickCmd(ctx, env, g, false),
	)
	return cmd
}

// newCriteriaEditCmd builds one item criteria command that sends the
// EditCriteria request build makes from its arguments, and prints the
// item's criteria as they are after it.
func newCriteriaEditCmd(ctx context.Context, env Env, g *globals, use, short string, args cobra.PositionalArgs, build func([]string) *chainmailv1.EditCriteriaRequest) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.EditCriteria(ctx, connect.NewRequest(build(args)))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			var b strings.Builder
			criteriaText(&b, resp.Msg.GetCriteria())
			fmt.Fprintf(&b, "revision %d\n", resp.Msg.GetRevision())
			return writeString(env.Stdout, b.String())
		},
	}
}

// newCriteriaTickCmd builds item criteria tick, which needs --evidence, or
// with tick false item criteria untick.
func newCriteriaTickCmd(ctx context.Context, env Env, g *globals, tick bool) *cobra.Command {
	var evidence string
	cmd := &cobra.Command{
		Use:   "tick <id> <criterion-id> --evidence <text>",
		Short: "Tick a criterion, with evidence that it is met",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tick && strings.TrimSpace(evidence) == "" {
				return usageError{err: errors.New("--evidence is required"), hint: helpHint(cmd)}
			}
			req := &chainmailv1.TickCriterionRequest{ItemId: args[0], CriterionId: args[1], Ticked: tick, Evidence: evidence}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.TickCriterion(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, tickedText(args[0], tick, resp.Msg))
		},
	}
	if tick {
		cmd.Flags().StringVar(&evidence, "evidence", "", "what shows the criterion is met, such as a ref or a link (required)")
	} else {
		cmd.Use, cmd.Short = "untick <id> <criterion-id>", "Untick a criterion and drop its evidence"
	}
	return cmd
}

func newItemBlockCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "block <id> <blocked-by-id>",
		Short: "Record that an item is blocked by another; --remove clears it",
		Args:  exactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			req := &chainmailv1.SetBlockerRequest{ItemId: args[0], BlockedById: args[1], Linked: !remove}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.SetBlocker(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, blockText(args[0], args[1], remove, resp.Msg))
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the blocker instead")
	return cmd
}

func newItemLinkCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var anchor bool
	var note string
	cmd := &cobra.Command{
		Use:   "link <id> <ref>",
		Short: "File the thread of a message against an item; moving the message keeps the link",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := parseRef(cmd, args[1]); err != nil {
				return err
			}
			req := &chainmailv1.LinkThreadRequest{ItemId: args[0], Ref: args[1], Anchor: anchor, Note: note}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.LinkThread(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, linkedText(args[0], resp.Msg))
		},
	}
	cmd.Flags().BoolVar(&anchor, "anchor", false, "make this message the link's anchor, the one the item is about")
	cmd.Flags().StringVar(&note, "note", "", "why the thread belongs to the item (default: keep the note it has)")
	return cmd
}

func newItemUnlinkCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <id> <account> <thread-key>",
		Short: "Remove a thread's link from an item; item show lists the account and thread key",
		Args:  exactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			req := &chainmailv1.UnlinkThreadRequest{ItemId: args[0], Account: args[1], ThreadKey: args[2]}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Items.UnlinkThread(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, fmt.Sprintf("unlinked %s thread %s from %s, revision %d\n",
				termText(args[1]), termText(args[2]), termText(args[0]), resp.Msg.GetRevision()))
		},
	}
}
