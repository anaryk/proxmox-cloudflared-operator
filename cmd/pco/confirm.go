package main

import (
	"errors"
	"io"
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
	answer, err := a.ask(cmd, question)
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", nil
}

// ask asks a question on stderr and returns the answer read from stdin,
// which has to be a terminal, with the white space around it trimmed; the
// end of the input is an empty answer.
func (a *app) ask(cmd *cobra.Command, question string) (string, error) {
	if _, terminal := a.stdinTerminal(cmd.InOrStdin()); !terminal {
		return "", errNoTerminal
	}
	s := &screen{w: cmd.ErrOrStderr()}
	s.printf("%s ", question)
	if err := s.done(); err != nil {
		return "", err
	}
	line, err := readLine(cmd.InOrStdin())
	if err != nil {
		// The answer did not end with a newline, or never came.
		s.println("")
	}
	return strings.TrimSpace(line), nil
}

// readLine reads up to a newline a byte at a time, so that what follows is
// left for the next question.
func readLine(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return string(line), nil
			}
			line = append(line, b[0])
		}
		if err != nil {
			return string(line), err
		}
	}
}

// addYesFlag adds --yes, which answers the questions of a command with yes.
func addYesFlag(cmd *cobra.Command, yes *bool) {
	cmd.Flags().BoolVarP(yes, "yes", "y", false, "do not ask for confirmation (needed without a terminal)")
}
