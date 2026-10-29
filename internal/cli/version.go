package cli

import (
	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/version"
)

func newVersionCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the chainmail version",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			return writeJSON(env.Stdout, map[string]string{"version": version.Version})
		},
	}
}
