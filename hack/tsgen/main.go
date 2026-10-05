// Command tsgen writes what the web interface takes from the Go code, as
// TypeScript, to standard output:
//
//	go run ./hack/tsgen -words > web/src/gen/words.gen.ts
//
// -words writes the words of internal/present that are lookups, and the
// classes of characters present.Printable replaces.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "tsgen:", err)
		os.Exit(2)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tsgen", flag.ContinueOnError)
	words := fs.Bool("words", false, "write the word tables of internal/present")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*words {
		return errors.New("nothing to write: pass -words")
	}
	_, err := io.WriteString(out, wordsModule())
	return err
}
