package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
	"github.com/yaklabco/chainmail/internal/ref"
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

// accountSelector picks the accounts a folder completion covers from the
// command and its arguments: none means every account. It returns false
// when there is nothing to look up, such as no Google account for labels.
type accountSelector func(*cobra.Command, []string) ([]string, bool)

// flagAccount selects the --account flag's value, or every account when
// it is not set.
func flagAccount(cmd *cobra.Command, _ []string) ([]string, bool) {
	if a, err := cmd.Flags().GetString("account"); err == nil && a != "" {
		return []string{a}, true
	}
	return nil, true
}

// argAccounts selects the accounts of the refs among the arguments, or
// every account when there are none.
func argAccounts(_ *cobra.Command, args []string) ([]string, bool) { return refAccounts(args), true }

// anyAccount selects every account.
func anyAccount(*cobra.Command, []string) ([]string, bool) { return nil, true }

// folderSpec is how one argument or flag completes folder names.
type folderSpec struct {
	// accounts picks the accounts whose folders are offered.
	accounts accountSelector
	// keep accepts the folders to offer; nil offers all.
	keep func(*chainmailv1.Folder) bool
	// maxArgs is how many positional arguments take a folder, or 0 for a
	// flag value.
	maxArgs int
	// noSpace leaves the cursor after the name, for a new folder's name
	// that the user goes on typing.
	noSpace bool
}

// folders completes folder names as spec says. A name on several accounts
// is offered once, its description listing them.
func (cp *completer) folders(spec folderSpec) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		directive := cobra.ShellCompDirectiveNoFileComp
		if spec.noSpace {
			directive |= cobra.ShellCompDirectiveNoSpace
		}
		if spec.maxArgs > 0 && len(args) >= spec.maxArgs {
			return nil, directive
		}
		accounts, ok := spec.accounts(cmd, args)
		if !ok {
			return nil, directive
		}
		lists, ok := cp.listFolders(cmd, accounts)
		if !ok {
			return nil, directive
		}
		where := map[string][]string{}
		var names []string
		for _, af := range lists {
			for _, f := range af.GetFolders() {
				name := f.GetName()
				if !safeValue(name) || !strings.HasPrefix(name, toComplete) || (spec.keep != nil && !spec.keep(f)) {
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

// googleAccounts selects the Google accounts among the typed refs'
// accounts, or every Google account when no ref is typed. It selects
// nothing when the daemon cannot list accounts or none of them is Google.
func (cp *completer) googleAccounts(cmd *cobra.Command, args []string) ([]string, bool) {
	list, ok := cp.listAccounts(cmd)
	if !ok {
		return nil, false
	}
	typed := refAccounts(args)
	var out []string
	for _, a := range list {
		google := a.GetProvider() == "gmail" || a.GetProvider() == "google-workspace"
		if google && (len(typed) == 0 || slices.Contains(typed, a.GetName())) {
			out = append(out, a.GetName())
		}
	}
	return out, len(out) > 0
}

// sender describes a row by its first sender.
func sender(r *chainmailv1.MessageRow) string { return firstSender(r.GetFrom()) }

// recipient describes a row by its first recipient, for drafts.
func recipient(r *chainmailv1.MessageRow) string { return firstSender(r.GetTo()) }

// refs completes message refs from the newest messages each query finds,
// as up to maxArgs positional arguments. It runs queries in order until it
// has refCompletionLimit candidates, offering each message once, so an
// earlier query's messages come first. A query that fails after an
// earlier one found candidates ends the lookup but keeps them. A typed
// cm1:<account>: prefix limits the search to that account. who picks the
// person a row's description names.
func (cp *completer) refs(maxArgs int, who func(*chainmailv1.MessageRow) string, queries ...string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var accounts []string
		if parts := strings.Split(toComplete, ":"); len(parts) >= 3 && parts[0] == "cm1" {
			accounts = []string{parts[1]}
		}
		var out []cobra.Completion
		seen := map[string]bool{}
		keep := func(rows []*chainmailv1.MessageRow) {
			for _, r := range rows {
				if len(out) == refCompletionLimit {
					return
				}
				v := r.GetRef()
				if seen[v] || !safeValue(v) || !strings.HasPrefix(v, toComplete) || slices.Contains(args, v) {
					continue
				}
				seen[v] = true
				desc := shortDate(r.GetDate(), cp.env.location()) + " · " + who(r) + " · " + r.GetSubject().GetUntrusted()
				out = append(out, cobra.CompletionWithDesc(v, completionText(desc)))
			}
		}
		ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
			for _, q := range queries {
				if len(out) == refCompletionLimit {
					return nil
				}
				req := &chainmailv1.SearchRequest{QueryString: q, Accounts: accounts, Limit: refCompletionLimit}
				resp, err := c.Message.Search(ctx, connect.NewRequest(req))
				if err != nil && len(out) > 0 {
					cobra.CompDebugln("chainmail: "+err.Error(), false)
					return nil
				}
				if err != nil {
					return err
				}
				keep(resp.Msg.GetRows())
			}
			return nil
		})
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// parts completes the attachment part numbers of the message ref names.
func (cp *completer) parts(cmd *cobra.Command, ref, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var atts []*chainmailv1.Attachment
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Message.Get(ctx, connect.NewRequest(&chainmailv1.GetRequest{Refs: []string{ref}}))
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
		desc := fmt.Sprintf("%s (%s, %d bytes)", a.GetFilename().GetUntrusted(), a.GetContentType(), a.GetSize())
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
			return cp.parts(cmd, args[0], toComplete)
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
}
