package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// shells are the shells pco completion writes a script for.
var shells = []string{"bash", "zsh", "fish", "powershell"}

// completionCmd takes the place of the one cobra adds, so that its help can
// say how the scripts are used with pco.
func (a *app) completionCmd() *cobra.Command {
	var noDesc bool
	cmd := &cobra.Command{
		Use:   "completion <shell>",
		Short: "Print the script that completes pco in a shell",
		Long: "Print the script that completes the commands, flags and arguments of pco in a shell: bash,\n" +
			"zsh, fish or powershell. The script asks pco itself for the completions, so it stays right\n" +
			"when pco is upgraded; bash needs the package bash-completion. It changes nothing, and\n" +
			"anyone may run it.",
		Example: "  # Complete pco in the bash you are in\n" +
			"  source <(pco completion bash)\n\n" +
			"  # In the zsh you are in, once compinit has run\n" +
			"  source <(pco completion zsh)\n\n" +
			"  # In the fish you are in\n" +
			"  pco completion fish | source",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: shells,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			return writeCompletion(cmd.Root(), args[0], cmd.OutOrStdout(), !noDesc)
		},
	}
	cmd.Flags().BoolVar(&noDesc, "no-descriptions", false, "leave the descriptions out of the completions")
	return cmd
}

// writeCompletion writes the completion script of a shell for the commands
// of root, with or without the descriptions of the completions.
func writeCompletion(root *cobra.Command, shell string, w io.Writer, descriptions bool) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(w, descriptions)
	case "zsh":
		if descriptions {
			return root.GenZshCompletion(w)
		}
		return root.GenZshCompletionNoDesc(w)
	case "fish":
		return root.GenFishCompletion(w, descriptions)
	case "powershell":
		if descriptions {
			return root.GenPowerShellCompletionWithDesc(w)
		}
		return root.GenPowerShellCompletion(w)
	}
	return fmt.Errorf("no completion for the shell %q: want bash, zsh, fish or powershell", shell)
}
