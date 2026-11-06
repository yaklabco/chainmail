package cli

import (
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
)

// itemCompletionLimit is how many items an item ID completion reads: the
// most one ListItems page holds.
const itemCompletionLimit = 500

// listItemRows asks the daemon for the first itemCompletionLimit items in
// tree order, the deleted ones too when deleted is set.
func (cp *completer) listItemRows(cmd *cobra.Command, deleted bool) ([]*chainmailv1.WorkItemRow, bool) {
	var rows []*chainmailv1.WorkItemRow
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		req := &chainmailv1.ListItemsRequest{Limit: itemCompletionLimit, IncludeDeleted: deleted}
		resp, err := c.Items.ListItems(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		rows = resp.Msg.GetItems()
		return nil
	})
	return rows, ok
}

// items completes the IDs of live items, as up to maxArgs positional
// arguments, or a flag value when maxArgs is 0. An ID already among the
// arguments is not offered again. Each is described by its type and title.
func (cp *completer) items(maxArgs int) cobra.CompletionFunc {
	return cp.itemIDs(maxArgs, false)
}

// itemIDs completes live item IDs, or deleted ones when deleted is set.
func (cp *completer) itemIDs(maxArgs int, deleted bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		rows, ok := cp.listItemRows(cmd, deleted)
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []cobra.Completion
		for _, r := range rows {
			id := r.GetId()
			if r.GetDeleted() != deleted || !safeValue(id) || !strings.HasPrefix(id, toComplete) || slices.Contains(args, id) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(id, completionText(r.GetType()+" · "+r.GetTitle().GetUntrusted())))
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// itemSpecs is how the item commands complete, by command path. specs
// merges it in.
func (cp *completer) itemSpecs() map[string]compSpec {
	return map[string]compSpec{
		"chainmail item":      {args: noComplete},
		"chainmail item show": {args: cp.items(1)},
		"chainmail item list": {args: noComplete, flags: map[string]cobra.CompletionFunc{
			"type": fixed(itemTypes...), "state": fixed(itemStates...), "area": fixed(itemAreas...), "under": cp.items(0),
			"changed-since": noComplete, "limit": noComplete, "page-token": noComplete,
		}},
		"chainmail item create": {
			args: byPosition(noComplete, fixed(itemTypes...)),
			flags: mergeFlagCompletions(itemFieldFlagSpecs(), map[string]cobra.CompletionFunc{
				"parent": cp.items(0), "criterion": noComplete, "idempotency-key": noComplete,
			}),
		},
		"chainmail item update": {args: cp.items(1), flags: mergeFlagCompletions(itemFieldFlagSpecs(), map[string]cobra.CompletionFunc{
			"type": fixed(itemTypes...), "title": noComplete, "clear": commaList(itemClearFields...),
		})},
		"chainmail item move":            {args: cp.items(2)},
		"chainmail item delete":          {args: cp.items(1)},
		"chainmail item restore":         {args: cp.deletedItems(1)},
		"chainmail item criteria":        {args: noComplete},
		"chainmail item criteria add":    {args: byPosition(noComplete, cp.items(0))},
		"chainmail item criteria edit":   {args: byPosition(noComplete, cp.items(0), cp.criteria)},
		"chainmail item criteria remove": {args: byPosition(cp.criteria, cp.items(0))},
		"chainmail item criteria order":  {args: byPosition(cp.criteria, cp.items(0))},
		"chainmail item criteria tick": {
			args:  byPosition(noComplete, cp.items(0), cp.criteria),
			flags: map[string]cobra.CompletionFunc{"evidence": noComplete},
		},
		"chainmail item criteria untick": {args: byPosition(noComplete, cp.items(0), cp.criteria)},
		"chainmail item block":           {args: cp.items(2)},
		"chainmail item link": {
			args:  byPosition(noComplete, cp.items(0), cp.refs("in:inbox", 0, sender)),
			flags: map[string]cobra.CompletionFunc{"note": noComplete},
		},
		"chainmail item unlink": {args: byPosition(noComplete, cp.items(0), cp.linkAccounts, cp.linkThreads)},
	}
}

// byPosition completes positional argument i with fns[i], and every
// argument after the last of fns with rest.
func byPosition(rest cobra.CompletionFunc, fns ...cobra.CompletionFunc) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) < len(fns) {
			return fns[len(args)](cmd, args, toComplete)
		}
		return rest(cmd, args, toComplete)
	}
}

// itemFieldFlagSpecs completes the field flags item create and item update share.
func itemFieldFlagSpecs() map[string]cobra.CompletionFunc {
	return map[string]cobra.CompletionFunc{
		"description": noComplete, "description-file": files, "state": fixed(itemStates...), "area": fixed(itemAreas...),
		"priority": fixed(itemPriorities...), "start": noComplete, "due": noComplete, "estimate": noComplete,
	}
}

func (cp *completer) deletedItems(maxArgs int) cobra.CompletionFunc {
	return cp.itemIDs(maxArgs, true)
}

// getItem asks the daemon for one item's view.
func (cp *completer) getItem(cmd *cobra.Command, id string) (*chainmailv1.GetItemResponse, bool) {
	var view *chainmailv1.GetItemResponse
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Items.GetItem(ctx, connect.NewRequest(&chainmailv1.GetItemRequest{Id: id}))
		if err != nil {
			return err
		}
		view = resp.Msg
		return nil
	})
	return view, ok
}

// criteria completes the criterion IDs of the item the first argument
// names, leaving out those already among the arguments. Each is described
// by its tick box and text.
func (cp *completer) criteria(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	view, ok := cp.getItem(cmd, args[0])
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, c := range view.GetCriteria() {
		id := c.GetId()
		if !safeValue(id) || !strings.HasPrefix(id, toComplete) || slices.Contains(args[1:], id) {
			continue
		}
		box := "[ ] "
		if c.GetTicked() {
			box = "[x] "
		}
		out = append(out, cobra.CompletionWithDesc(id, completionText(box+c.GetText().GetUntrusted())))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// linkAccounts completes the accounts of the threads linked to the item
// the first argument names, for item unlink.
func (cp *completer) linkAccounts(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	view, ok := cp.getItem(cmd, args[0])
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, l := range view.GetThreads() {
		if a := l.GetAccount(); safeValue(a) && strings.HasPrefix(a, toComplete) {
			names = append(names, a)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), cobra.ShellCompDirectiveNoFileComp
}

// linkThreads completes the keys of the threads linked to the item the
// first argument names on the account the second names, for item unlink.
// Each is described by its newest message's subject.
func (cp *completer) linkThreads(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	view, ok := cp.getItem(cmd, args[0])
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, l := range view.GetThreads() {
		key := l.GetThreadKey()
		if l.GetAccount() != args[1] || !safeValue(key) || !strings.HasPrefix(key, toComplete) {
			continue
		}
		out = append(out, cobra.CompletionWithDesc(key, completionText(l.GetLatest().GetSubject().GetUntrusted())))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
