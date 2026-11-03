package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// refCompletionLimit is how many messages a ref completion offers.
const refCompletionLimit = 20

// listFolders asks the daemon for the folders of accounts, or of every
// account when accounts is empty.
func (cp *completer) listFolders(cmd *cobra.Command, accounts []string) ([]*chainmailv1.AccountFolders, bool) {
	var out []*chainmailv1.AccountFolders
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Message.ListFolders(ctx, connect.NewRequest(&chainmailv1.ListFoldersRequest{Accounts: accounts}))
		if err != nil {
			return err
		}
		out = resp.Msg.GetAccounts()
		return nil
	})
	return out, ok
}

// refAccounts returns the accounts of the refs among args, in order,
// without repeats. Arguments that are not refs are skipped.
func refAccounts(args []string) []string {
	var out []string
	for _, a := range args {
		r, err := ref.Parse(a)
		if err == nil && !slices.Contains(out, r.Account) {
			out = append(out, r.Account)
		}
	}
	return out
}

// flagAccount returns the --account flag's value as a one-account list,
// or nil when it is not set.
func flagAccount(cmd *cobra.Command, _ []string) []string {
	if a, err := cmd.Flags().GetString("account"); err == nil && a != "" {
		return []string{a}
	}
	return nil
}

// argAccounts is refAccounts as an accountsOf function.
func argAccounts(_ *cobra.Command, args []string) []string { return refAccounts(args) }

// anyAccount names no account, so a folder completion covers them all.
func anyAccount(*cobra.Command, []string) []string { return nil }

// folders completes folder names on the accounts accountsOf names (every
// account when it names none) that keep accepts (nil for all), as up to
// maxArgs positional arguments or a flag value when maxArgs is 0. A name
// on several accounts is offered once, its description listing them.
func (cp *completer) folders(accountsOf func(*cobra.Command, []string) []string, keep func(*chainmailv1.Folder) bool, maxArgs int, noSpace bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		directive := cobra.ShellCompDirectiveNoFileComp
		if noSpace {
			directive |= cobra.ShellCompDirectiveNoSpace
		}
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, directive
		}
		lists, ok := cp.listFolders(cmd, accountsOf(cmd, args))
		if !ok {
			return nil, directive
		}
		where := map[string][]string{}
		var names []string
		for _, af := range lists {
			for _, f := range af.GetFolders() {
				name := f.GetName()
				if !safeValue(name) || !strings.HasPrefix(name, toComplete) || (keep != nil && !keep(f)) {
					continue
				}
				if _, seen := where[name]; !seen {
					names = append(names, name)
				}
				desc := af.GetAccount()
				if role := f.GetRole(); role != "" && role != "none" {
					desc += " " + role
				}
				where[name] = append(where[name], desc)
			}
		}
		slices.Sort(names)
		out := make([]cobra.Completion, 0, len(names))
		for _, n := range names {
			out = append(out, cobra.CompletionWithDesc(n, completionText(strings.Join(where[n], ", "))))
		}
		return out, directive
	}
}

// labelFolder keeps a Gmail folder that is a label of the user's own,
// rather than INBOX or a [Gmail] system folder.
func labelFolder(f *chainmailv1.Folder) bool {
	role := f.GetRole()
	return (role == "" || role == "none") && !strings.HasPrefix(f.GetName(), "[Gmail]/")
}

// noAccount is an account name no daemon has, so a folder lookup for it
// offers nothing.
const noAccount = "\x00"

// googleAccounts returns the Google accounts among the typed refs' accounts,
// or every Google account when no ref is typed.
func (cp *completer) googleAccounts(cmd *cobra.Command, args []string) []string {
	list, ok := cp.listAccounts(cmd)
	if !ok {
		return []string{noAccount}
	}
	typed := refAccounts(args)
	var out []string
	for _, a := range list {
		google := a.GetProvider() == "gmail" || a.GetProvider() == "google-workspace"
		if google && (len(typed) == 0 || slices.Contains(typed, a.GetName())) {
			out = append(out, a.GetName())
		}
	}
	if len(out) == 0 {
		return []string{noAccount}
	}
	return out
}

// sender describes a row by its first sender.
func sender(r *chainmailv1.MessageRow) string { return firstSender(r.GetFrom()) }

// recipient describes a row by its first recipient, for drafts.
func recipient(r *chainmailv1.MessageRow) string { return firstSender(r.GetTo()) }

// refs completes message refs from the newest messages query finds, as
// up to maxArgs positional arguments. A typed cm1:<account>: prefix limits
// the search to that account. who picks the person a row's description
// names.
func (cp *completer) refs(query string, maxArgs int, who func(*chainmailv1.MessageRow) string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		req := &chainmailv1.SearchRequest{QueryString: query, Limit: refCompletionLimit}
		if parts := strings.Split(toComplete, ":"); len(parts) >= 3 && parts[0] == "cm1" {
			req.Accounts = []string{parts[1]}
		}
		var rows []*chainmailv1.MessageRow
		ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
			resp, err := c.Message.Search(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			rows = resp.Msg.GetRows()
			return nil
		})
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []cobra.Completion
		for _, r := range rows {
			v := r.GetRef()
			if !safeValue(v) || !strings.HasPrefix(v, toComplete) || slices.Contains(args, v) {
				continue
			}
			desc := shortDate(r.GetDate(), cp.env.location()) + " · " + who(r) + " · " + r.GetSubject().GetUntrusted()
			out = append(out, cobra.CompletionWithDesc(v, completionText(desc)))
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// parts completes the attachment part numbers of the message args[0] names.
func (cp *completer) parts(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var atts []*chainmailv1.Attachment
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Message.Get(ctx, connect.NewRequest(&chainmailv1.GetRequest{Refs: args[:1]}))
		if err != nil {
			return err
		}
		for _, it := range resp.Msg.GetItems() {
			atts = append(atts, it.GetMessage().GetAttachments()...)
		}
		return nil
	})
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, a := range atts {
		if !safeValue(a.GetPart()) || !strings.HasPrefix(a.GetPart(), toComplete) {
			continue
		}
		desc := fmt.Sprintf("%s (%s, %s)", a.GetFilename().GetUntrusted(), a.GetContentType(), humanize.IBytes(uint64(max(a.GetSize(), 0))))
		out = append(out, cobra.CompletionWithDesc(a.GetPart(), completionText(desc)))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// refThenPart completes attachment <ref> <part>.
func (cp *completer) refThenPart(inbox cobra.CompletionFunc) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return inbox(cmd, args, toComplete)
		case 1:
			return cp.parts(cmd, args, toComplete)
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
}
