package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	err := newRootCmd().Execute()
	switch {
	case err == nil:
	case errors.Is(err, errReported):
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "pco: %v\n", err)
		os.Exit(1)
	}
}
