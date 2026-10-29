package cli

import (
	"context"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

func newAdminCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("admin", "Daemon administration")
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the daemon's version, uptime and stored secret names",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Admin.GetStatus(ctx, connect.NewRequest(&chainmailv1.GetStatusRequest{}))
			if err != nil {
				return err
			}
			if g.table() {
				return statusTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	})
	return cmd
}
