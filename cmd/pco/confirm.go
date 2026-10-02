package main

import (
	"bufio"
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

var (
	// errAborted is the error of a command the admin did not confirm.
	errAborted = errors.New("aborted: nothing was changed")
	// errNoTerminal is the error of a question that has nobody to answer it.
	errNoTerminal = errors.New("stdin is not a terminal: pass --yes to confirm")
)

// confirm asks a question on stderr and reads the answer from stdin, which has
// to be a terminal: a pipe that says yes is a script that has not read what it
// confirms, and one that stays silent would hang. A script passes --yes, and
// the question is not asked. Only "y" and "yes" say yes; an empty answer,
// anything else and the end of the input say no.
func (a *app) confirm(cmd *cobra.Command, yes bool, question string) (bool, error) {
	if yes {
		return true, nil
	}
	if _, terminal := a.stdinTerminal(cmd.InOrStdin()); !terminal {
		return false, errNoTerminal
	}
	s := &screen{w: cmd.ErrOrStderr()}
	s.printf("%s ", question)
	if err := s.done(); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		// The answer did not end with a newline, or never came.
		s.println("")
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// addYesFlag adds --yes, which answers the questions of a command with yes.
func addYesFlag(cmd *cobra.Command, yes *bool) {
	cmd.Flags().BoolVarP(yes, "yes", "y", false, "do not ask for confirmation (needed without a terminal)")
}
