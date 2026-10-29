// Package cli implements the chainmail command line.
package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// Execute runs chainmail with args and returns the process exit code.
func Execute(ctx context.Context, args []string, env Env) int {
	root := newRoot(env)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		writeError(env.Stderr, err)
		return exitCode(err)
	}
	return exitOK
}

func newRoot(env Env) *cobra.Command {
	root := &cobra.Command{
		Use:           "chainmail",
		Short:         "Email for AI agents, behind a policy daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(newVersionCmd(env))
	return root
}
