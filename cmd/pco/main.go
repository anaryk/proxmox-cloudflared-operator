package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(exitCode(newRootCmd().Execute(), os.Stderr))
}

// exitCode is the exit status for what a command returned, and prints the
// error to stderr unless the command has reported it itself. What is printed
// is cleaned like everything else: an error may carry text of the daemon, and
// so of others.
func exitCode(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errReported):
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "pco: %s\n", printable(err.Error()))
	return 1
}
