// Package cli implements the chainmail command line.
package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

const helpHint = "run: chainmail --help"

// Execute runs chainmail with args and returns the process exit code.
func Execute(ctx context.Context, args []string, env Env) int {
	root := newRoot(env)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		return reportError(env.Stderr, err)
	}
	return exitOK
}

func newRoot(env Env) *cobra.Command {
	root := &cobra.Command{
		Use:           "chainmail",
		Short:         "Email for AI agents, behind a policy daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The root runs only when no subcommand matched. Setting Args stops cobra
		// reporting unknown commands itself, and RunE makes the root runnable so
		// cobra does not answer a bare invocation with help.
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError{err: errors.New("no command given"), hint: helpHint}
			}
			return usageError{err: fmt.Errorf("unknown command %q", args[0]), hint: helpHint}
		},
	}
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err: err} })
	root.AddCommand(newVersionCmd(env))
	return root
}
