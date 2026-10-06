// Command tsgen writes what the web interface takes from the Go code, as
// TypeScript, to standard output:
//
//	go run ./hack/tsgen -words > web/src/gen/words.gen.ts
//	go run ./hack/tsgen -types > web/src/api/types.gen.ts
//
// -words writes the words of internal/present that are lookups, and the
// classes of characters present.Printable replaces; -types the JSON of the
// daemon's answers and notices, and of the web process's own.
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
	types := fs.Bool("types", false, "write the types of the JSON of the API")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var module string
	switch {
	case *words && *types:
		return errors.New("pass -words or -types, not both")
	case *words:
		module = wordsModule()
	case *types:
		var err error
		if module, err = typesModule(); err != nil {
			return err
		}
	default:
		return errors.New("nothing to write: pass -words or -types")
	}
	_, err := io.WriteString(out, module)
	return err
}
