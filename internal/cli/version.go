package cli

import (
	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/version"
)

func newVersionCmd(env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the chainmail version",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			if g.table() {
				return writeString(env.Stdout, "chainmail "+version.Version+"\n")
			}
			return writeJSON(env.Stdout, map[string]string{"version": version.Version})
		},
	}
}
