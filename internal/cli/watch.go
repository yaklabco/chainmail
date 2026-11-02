package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

// newWatchCmd builds chainmail watch. Until the event stream lands it only
// holds the status and groups subcommands.
func newWatchCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("watch", "Watch for mail events")
	cmd.AddCommand(newWatchStatusCmd(ctx, env, g), newWatchGroupsCmd(ctx, env, g))
	return cmd
}

func newWatchStatusCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show each synced folder's mode, state, last sync and last error",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Watch.GetWatchStatus(ctx, connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
			if err != nil {
				return err
			}
			if g.table() {
				return watchStatusTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
}

func newWatchGroupsCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groups",
		Short: "List watch groups with their acknowledged event, lag and last use",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Watch.ListGroups(ctx, connect.NewRequest(&chainmailv1.ListGroupsRequest{}))
			if err != nil {
				return err
			}
			if g.table() {
				return watchGroupsTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a watch group, so its next use starts at the newest event",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Watch.DeleteGroup(ctx, connect.NewRequest(&chainmailv1.DeleteGroupRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			if resp.Msg.GetDeleted() {
				return writeString(env.Stdout, "deleted group "+termText(args[0])+"\n")
			}
			return writeString(env.Stdout, "no group named "+termText(args[0])+"\n")
		},
	})
	return cmd
}

// orDash shows an empty cell as "-", so a table never ends a line with padding.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// watchStatusTable prints one line per synced folder. Folder names come
// from mail servers, so every value is cleaned for the terminal.
func watchStatusTable(w io.Writer, loc *time.Location, r *chainmailv1.GetWatchStatusResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tFOLDER\tMODE\tSTATE\tCONDSTORE\tLAST SYNC\tLAST SWEEP\tERROR")
	for _, f := range r.GetFolders() {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			termText(f.GetAccount()), termText(f.GetFolder()), orDash(termText(f.GetMode())), orDash(termText(f.GetState())),
			f.GetCondstore(), orDash(shortDate(f.GetLastSyncAt(), loc)), orDash(shortDate(f.GetLastSweepAt(), loc)),
			orDash(termText(f.GetLastError())))
	}
	return tw.Flush()
}

// watchGroupsTable prints one line per group.
func watchGroupsTable(w io.Writer, loc *time.Location, r *chainmailv1.ListGroupsResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "NAME\tACKED\tLAG\tLAST SEEN")
	for _, gr := range r.GetGroups() {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n",
			termText(gr.GetName()), gr.GetAckedId(), gr.GetLag(), orDash(shortDate(gr.GetLastSeenAt(), loc)))
	}
	return tw.Flush()
}
