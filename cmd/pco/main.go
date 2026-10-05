package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
)

func main() {
	os.Exit(exitCode(newRootCmd().Execute(), os.Stderr))
}

// exitCode is the exit status for what a command returned, as the help says,
// and prints the error to stderr unless the command has reported it itself.
// What is printed is cleaned like everything else: an error may carry text of
// the daemon, and so of others.
func exitCode(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errReported):
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "pco: %s\n", present.Printable(err.Error()))
	var coded interface{ ExitCode() int }
	switch {
	case errors.Is(err, apiclient.ErrNoAnswer) || errors.As(err, new(couldNotAsk)):
		return 2
	case errors.As(err, &coded):
		// The daemon of an appliance without its volume: its unit does not
		// start it again on this status.
		return coded.ExitCode()
	}
	return 1
}
