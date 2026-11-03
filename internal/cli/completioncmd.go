package cli

import (
	"fmt"

	mcobra "github.com/muesli/mango-cobra"
	"github.com/muesli/roff"
	"github.com/spf13/cobra"
)

const completionHelp = `Print a script that teaches your shell to complete chainmail's commands,
flags and arguments, with descriptions. Completions that name accounts,
folders, labels, messages, attachments, secrets and watch groups ask the
daemon, so they need it running; when it is not, they offer nothing.`

// newCompletionCmd prints a completion script. Unlike cobra's own
// completion command, a missing or unknown shell is a usage error.
func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Print a shell completion script",
		Long:  completionHelp,
		Example: `  # bash: add to ~/.bashrc
  source <(chainmail completion bash)
  # zsh: write it to a directory on $fpath
  chainmail completion zsh > "${fpath[1]}/_chainmail"
  # fish
  chainmail completion fish > ~/.config/fish/completions/chainmail.fish
  # PowerShell
  chainmail completion powershell | Out-String | Invoke-Expression`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, out := cmd.Root(), cmd.OutOrStdout()
			var err error
			switch args[0] {
			case "bash":
				err = root.GenBashCompletionV2(out, true)
			case "zsh":
				err = root.GenZshCompletion(out)
			case "fish":
				err = root.GenFishCompletion(out, true)
			case "powershell":
				err = root.GenPowerShellCompletionWithDesc(out)
			default:
				return usageError{err: fmt.Errorf("unknown shell %q: want bash, zsh, fish or powershell", args[0]), hint: helpHint(cmd)}
			}
			if err != nil {
				return fmt.Errorf("writing the completion script: %w", err)
			}
			return nil
		},
	}
}

// newManCmd prints a man page for the whole command tree. It is hidden:
// packagers run it, people read the page it makes.
func newManCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "man",
		Short:  "Print chainmail's man page in roff",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			page, err := mcobra.NewManPage(1, cmd.Root())
			if err != nil {
				return fmt.Errorf("building the man page: %w", err)
			}
			return writeString(cmd.OutOrStdout(), page.Build(roff.NewDocument()))
		},
	}
}
