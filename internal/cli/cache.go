package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

const cacheHelp = `The daemon keeps a local cache of every synced folder: headers for all
of it, and bodies for a recent window plus mail read recently. Searches use
it when it can answer completely and is fresh. These commands show how far
it has got and maintain it.`

// newCacheCmd builds chainmail cache and its status, rebuild and evict
// subcommands.
func newCacheCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("cache", "Show and maintain the local mail cache")
	cmd.Long = cacheHelp
	cmd.AddCommand(newCacheStatusCmd(ctx, env, g), newCacheRebuildCmd(ctx, env, g), newCacheEvictCmd(ctx, env, g))
	return cmd
}

func newCacheStatusCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var accounts []string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show each account's cache state, backfill progress, bodies and budget",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: accounts}))
			if err != nil {
				return err
			}
			if g.table() {
				return cacheStatusTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
	cmd.Flags().StringArrayVar(&accounts, "account", nil, "only this account (repeatable)")
	return cmd
}

// rebuiltAccount is one account's part of cache rebuild's JSON output.
type rebuiltAccount struct {
	Account string   `json:"account"`
	Folders []string `json:"folders"`
}

// rebuildOutput is cache rebuild's JSON output. Skipped names the accounts
// a rebuild of every account passed over because their cache is disabled.
type rebuildOutput struct {
	Accounts []rebuiltAccount `json:"accounts"`
	Skipped  []string         `json:"skipped,omitempty"`
}

func newCacheRebuildCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var account, folder string
	cmd := &cobra.Command{
		Use:   "rebuild [--account NAME [--folder FOLDER]]",
		Short: "Drop cached mail and backfill it again: every account, one, or one folder",
		Long: `Drop the cached headers and bodies of every account, of the account
--account names, or of one of its folders, and backfill them again from the
mail server. Searches use the server for what is being rebuilt until the
backfill is done. Without --account, accounts whose cache is disabled are
skipped. Running it again is safe.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if folder != "" && account == "" {
				return usageError{err: errors.New("--folder needs --account"), hint: helpHint(cmd)}
			}
			c := g.client(env)
			defer c.Close()
			accounts := []string{account}
			var skipped []string
			if account == "" {
				resp, err := c.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{}))
				if err != nil {
					return err
				}
				accounts = accounts[:0]
				for _, a := range resp.Msg.GetAccounts() {
					// The daemon refuses to rebuild an account with no
					// cache, so trying one would stop the loop.
					if !a.GetEnabled() {
						skipped = append(skipped, a.GetAccount())
						continue
					}
					accounts = append(accounts, a.GetAccount())
				}
			}
			out := rebuildOutput{Accounts: make([]rebuiltAccount, 0, len(accounts)), Skipped: skipped}
			for _, a := range accounts {
				resp, err := c.Cache.RebuildCache(ctx, connect.NewRequest(&chainmailv1.RebuildCacheRequest{Account: a, Folder: folder}))
				if err != nil {
					return err
				}
				folders := resp.Msg.GetFolders()
				if folders == nil {
					folders = []string{}
				}
				out.Accounts = append(out.Accounts, rebuiltAccount{Account: a, Folders: folders})
			}
			if g.table() {
				return rebuildText(env.Stdout, out)
			}
			return writeJSON(env.Stdout, out)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "only this account (default: every account)")
	cmd.Flags().StringVar(&folder, "folder", "", "only this folder of --account")
	return cmd
}

func newCacheEvictCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "evict",
		Short: "Run a body eviction sweep now and report what it freed",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Cache.EvictCache(ctx, connect.NewRequest(&chainmailv1.EvictCacheRequest{}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, fmt.Sprintf("evicted %d bodies, freed %s\n", resp.Msg.GetBodiesEvicted(), byteSize(resp.Msg.GetFreedBytes())))
		},
	}
}

// rebuildText prints one line per rebuilt account with the folders it
// dropped, then one line per account skipped because its cache is disabled.
func rebuildText(w io.Writer, out rebuildOutput) error {
	var b strings.Builder
	for _, a := range out.Accounts {
		if len(a.Folders) == 0 {
			fmt.Fprintf(&b, "rebuilt %s: no cached folders\n", termText(a.Account))
			continue
		}
		names := make([]string, len(a.Folders))
		for i, f := range a.Folders {
			names[i] = termText(f)
		}
		fmt.Fprintf(&b, "rebuilt %s: %s\n", termText(a.Account), strings.Join(names, ", "))
	}
	for _, a := range out.Skipped {
		fmt.Fprintf(&b, "skipped %s: cache disabled\n", termText(a))
	}
	return writeString(w, b.String())
}

// cacheStatusTable prints the cache file's size, one line per account, and
// one line per cached folder when any account has one.
func cacheStatusTable(w io.Writer, loc *time.Location, r *chainmailv1.GetCacheStatusResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintf(tw, "size\t%s of %s\n", byteSize(r.GetSizeBytes()), byteSize(r.GetMaxSizeBytes()))
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := writeString(w, "\n"); err != nil {
		return err
	}
	if err := cacheAccountsTable(w, r.GetAccounts()); err != nil {
		return err
	}
	var folders bool
	for _, a := range r.GetAccounts() {
		folders = folders || len(a.GetFolders()) > 0
	}
	if !folders {
		return nil
	}
	if err := writeString(w, "\n"); err != nil {
		return err
	}
	return cacheFoldersTable(w, loc, r.GetAccounts())
}

// cacheAccountsTable prints one line per account: whether its cache is
// enabled, its state, today's download against the budget, and its last
// error, for cache status and admin status alike.
func cacheAccountsTable(w io.Writer, accounts []*chainmailv1.AccountCacheStatus) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tCACHE\tSTATE\tBUDGET TODAY\tERROR")
	for _, a := range accounts {
		enabled := "on"
		if !a.GetEnabled() {
			enabled = "off"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s of %s\t%s\n",
			termText(a.GetAccount()), enabled, orDash(termText(a.GetState())),
			byteSize(a.GetBudgetUsedBytes()), byteSize(a.GetBudgetBytes()), orDash(termText(a.GetLastError())))
	}
	return tw.Flush()
}

// cacheFoldersTable prints one line per cached folder. Folder names come
// from mail servers, so every value is cleaned for the terminal.
func cacheFoldersTable(w io.Writer, loc *time.Location, accounts []*chainmailv1.AccountCacheStatus) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tFOLDER\tSTATE\tHEADERS\tBACKFILL\tBODIES\tBODY SIZE\tWINDOW\tFAILED\tUNREADABLE\tATTACH UNKNOWN\tLAST APPLIED")
	for _, a := range accounts {
		for _, f := range a.GetFolders() {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\t%d\t%d\t%d\t%s\n",
				termText(a.GetAccount()), termText(f.GetFolder()), orDash(termText(f.GetState())), f.GetHeaders(),
				backfillText(f), f.GetBodies(), byteSize(f.GetBodyBytes()), windowText(f), f.GetBodyFailures(),
				f.GetUnreadable(), f.GetAttachmentsUnknown(), orDash(shortDate(f.GetLastAppliedAt(), loc)))
		}
	}
	return tw.Flush()
}

// backfillText shows how far backfill has walked down a folder.
func backfillText(f *chainmailv1.FolderCacheStatus) string {
	switch {
	case f.GetBackfillDone():
		return "done"
	case f.GetBackfillLowUid() == 0:
		return "not started"
	default:
		return fmt.Sprintf("at %d of %d", f.GetBackfillLowUid(), f.GetBaselineUid())
	}
}

// windowText shows how many messages in the body window have a cached body.
func windowText(f *chainmailv1.FolderCacheStatus) string {
	s := fmt.Sprintf("%d/%d", f.GetWindowBodies(), f.GetWindowMessages())
	if f.GetWindowDone() {
		s += " done"
	}
	return s
}

// byteSize shows n bytes in the largest binary unit that leaves at least 1,
// with one decimal past bytes: 512 B, 1.5 KiB, 2.0 GiB.
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
