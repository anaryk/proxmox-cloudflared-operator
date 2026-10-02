package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// errAborted is the error of a command the admin did not confirm.
var errAborted = errors.New("aborted: nothing was changed")

// confirm asks a question on stderr and reads the answer from stdin. Only "y"
// and "yes" say yes; an empty answer, anything else and the end of the input
// say no. With yes set the question is not asked.
func confirm(cmd *cobra.Command, yes bool, question string) (bool, error) {
	if yes {
		return true, nil
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), question+" "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		// The answer did not end with a newline, or never came.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// addYesFlag adds --yes, which answers the questions of a command with yes.
func addYesFlag(cmd *cobra.Command, yes *bool) {
	cmd.Flags().BoolVarP(yes, "yes", "y", false, "do not ask for confirmation")
}
