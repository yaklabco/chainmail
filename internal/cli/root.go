// Package cli implements the chainmail command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/config"
)

// Execute runs chainmail with args and returns the process exit code.
func Execute(ctx context.Context, args []string, env Env) int {
	root := newRoot(ctx, env)
	// cobra reads os.Args when args is nil, which would parse the host's command line.
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		return reportError(env.Stderr, err)
	}
	return api.ExitOK
}

// globals holds the flags every command shares.
type globals struct {
	socket string
	output string
}

func (g *globals) socketPath(env Env) string {
	if g.socket != "" {
		return g.socket
	}
	if s := env.Getenv("CHAINMAIL_SOCKET"); s != "" {
		return s
	}
	return config.DefaultSocket
}

// client connects to the daemon. Callers Close it, so idle connections do
// not hold up a daemon shutting down.
func (g *globals) client(env Env) *client.Client {
	return client.New(g.socketPath(env), "cli")
}

func (g *globals) table() bool { return g.output == "table" }

// newRoot builds the command tree. Commands use ctx, the context given to
// Execute, rather than cmd.Context(). The two are the same context, but only
// a ctx passed down by parameter lets contextcheck follow cancellation from
// Execute into each command.
func newRoot(ctx context.Context, env Env) *cobra.Command {
	g := &globals{}
	root := newGroup("chainmail", "Email for AI agents, behind a policy daemon")
	root.SilenceUsage = true
	root.SilenceErrors = true
	// cobra's completion command prints help and exits 0 for anything it does
	// not know, outside the exit contract. Its help command is replaced below.
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(newHelpCmd())
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// Changed tells an unset flag from an explicit --output "", which is a mistake.
		switch {
		case !cmd.Flags().Changed("output"):
			g.output = "json"
			if env.StdoutTerminal {
				g.output = "table"
			}
		case g.output == "json", g.output == "table":
		default:
			return usageError{err: fmt.Errorf("--output must be json or table, got %q", g.output), hint: "run: chainmail --help"}
		}
		return nil
	}
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return usageError{err: err, hint: helpHint(cmd)} })
	root.PersistentFlags().StringVar(&g.socket, "socket", "", "daemon socket (default $CHAINMAIL_SOCKET, then "+config.DefaultSocket+")")
	root.PersistentFlags().StringVar(&g.output, "output", "", "output format: json or table (default: table on a terminal, json otherwise)")
	root.AddCommand(
		newVersionCmd(env, g),
		newDaemonCmd(ctx, env, g),
		newAccountsCmd(ctx, env, g),
		newSecretCmd(ctx, env, g),
		newAuthCmd(ctx, env, g),
		newAdminCmd(ctx, env, g),
		newAuditCmd(ctx, env, g),
		newFoldersCmd(ctx, env, g),
		newSearchCmd(ctx, env, g),
		newGetCmd(ctx, env, g),
		newThreadCmd(ctx, env, g),
		newAttachmentCmd(ctx, env, g),
		newSendCmd(ctx, env, g),
		newSendRawCmd(ctx, env, g),
		newReplyCmd(ctx, env, g),
		newForwardCmd(ctx, env, g),
		newDraftCmd(ctx, env, g),
		newMCPCmd(ctx, env, g),
		newWatchCmd(ctx, env, g),
	)
	root.AddCommand(newOrganizeCmds(ctx, env, g)...)
	return root
}

// newGroup returns a command that only holds subcommands. It runs only when
// no subcommand matched. Setting Args stops cobra reporting unknown commands
// itself, or printing help and exiting 0 for them, and RunE makes the group
// runnable so cobra does not answer a bare invocation with help. Both become
// usage errors.
func newGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError{err: errors.New("no command given"), hint: helpHint(cmd)}
			}
			return usageError{err: fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()), hint: helpHint(cmd)}
		},
	}
}

// newHelpCmd replaces cobra's help command, which prints the root help and
// exits 0 for an unknown topic. Here an unknown topic is a usage error.
func newHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, rest, err := cmd.Root().Find(args)
			if err != nil || len(rest) > 0 {
				return usageError{err: fmt.Errorf("unknown help topic %q", strings.Join(args, " ")), hint: helpHint(cmd.Root())}
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	}
}
