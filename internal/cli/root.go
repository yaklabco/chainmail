// Package cli implements the chainmail command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/config"
)

// Execute runs chainmail with args and returns the process exit code. On a
// terminal, help and errors are styled by fang; otherwise errors are one
// line of JSON on stderr. The exit code is the same either way.
func Execute(ctx context.Context, args []string, env Env) int {
	root := newRoot(ctx, env)
	// cobra reads os.Args when args is nil, which would parse the host's command line.
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)
	if env.StderrTerminal {
		// fang has already shown the error through styledError.
		if err := fang.Execute(ctx, root, fangOptions()...); err != nil {
			return classify(err).code
		}
		return api.ExitOK
	}
	if err := root.ExecuteContext(ctx); err != nil {
		return reportError(env.Stderr, err)
	}
	return api.ExitOK
}

// globals holds the flags every command shares, and what they resolve to.
type globals struct {
	// socketFlag is --socket as given, empty when it was not. The daemon
	// reads only this, so a client's settings never move its socket.
	socketFlag string
	// socket and output are resolved by resolve.
	socket string
	output string
	// outputChosen says output came from --output, CHAINMAIL_OUTPUT or
	// cli.toml rather than the terminal default.
	outputChosen bool
}

// socketPath is the daemon socket clients dial.
func (g *globals) socketPath(Env) string {
	if g.socket == "" {
		return config.DefaultSocket
	}
	return g.socket
}

// addGlobalFlags registers the flags every command shares.
func addGlobalFlags(root *cobra.Command, g *globals) {
	root.PersistentFlags().StringVar(&g.socketFlag, "socket", "", "daemon socket (default $CHAINMAIL_SOCKET, then cli.toml, then "+config.DefaultSocket+")")
	root.PersistentFlags().StringVar(&g.output, "output", "", "output format: json or table (default $CHAINMAIL_OUTPUT, then cli.toml, then table on a terminal, json otherwise)")
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
	// cobra's own completion command prints help and exits 0 for a shell
	// it does not know, outside the exit contract. newCompletionCmd
	// replaces it. Its help command is replaced below for the same reason.
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(newHelpCmd())
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// Completion requests resolve settings inside each completion
		// function, against the command being completed, and must not fail
		// on a bad cli.toml.
		// The daemon uses neither client setting, so a bad cli.toml or
		// CHAINMAIL_OUTPUT in its environment must not stop it.
		if cmd.Name() == cobra.ShellCompRequestCmd || cmd.Name() == cobra.ShellCompNoDescRequestCmd || cmd.Name() == "daemon" {
			return nil
		}
		return g.resolve(env, cmd)
	}
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return usageError{err: err, hint: helpHint(cmd)} })
	addGlobalFlags(root, g)
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
		newCompletionCmd(),
		newManCmd(),
	)
	root.AddCommand(newOrganizeCmds(ctx, env, g)...)
	root.AddGroup(commandGroups...)
	for _, c := range root.Commands() {
		c.GroupID = groupOf[c.Name()]
	}
	root.SetHelpCommandGroupID("admin")
	applyHelp(root)
	return root
}

// commandGroups orders the top-level commands in help.
var commandGroups = []*cobra.Group{
	{ID: "read", Title: "Read"},
	{ID: "compose", Title: "Compose"},
	{ID: "organize", Title: "Organize"},
	{ID: "watch", Title: "Watch"},
	{ID: "accounts", Title: "Accounts and auth"},
	{ID: "admin", Title: "Admin"},
}

// groupOf names each top-level command's group. A test fails for a
// visible top-level command missing from it.
var groupOf = map[string]string{
	"search": "read", "get": "read", "thread": "read", "attachment": "read", "folders": "read",
	"send": "compose", "send-raw": "compose", "reply": "compose", "forward": "compose", "draft": "compose",
	"move": "organize", "copy": "organize", "archive": "organize", "trash": "organize", "expunge": "organize",
	"read": "organize", "unread": "organize", "flag": "organize", "unflag": "organize", "flags": "organize",
	"label": "organize", "folder": "organize",
	"watch":    "watch",
	"accounts": "accounts", "auth": "accounts", "secret": "accounts",
	"admin": "admin", "audit": "admin", "daemon": "admin", "mcp": "admin", "version": "admin", "completion": "admin",
}

// walk calls fn for cmd and every command below it, parents first.
func walk(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, c := range cmd.Commands() {
		walk(c, fn)
	}
}

// groupAnnotation marks a command that only holds subcommands.
const groupAnnotation = "chainmail.group"

// newGroup returns a command that only holds subcommands. It runs only when
// no subcommand matched. Setting Args stops cobra reporting unknown commands
// itself, or printing help and exiting 0 for them, and RunE makes the group
// runnable so cobra does not answer a bare invocation with help. Both become
// usage errors.
func newGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Annotations: map[string]string{groupAnnotation: "true"},
		Args:        cobra.ArbitraryArgs,
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
