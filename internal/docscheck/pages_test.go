package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// repo returns the path of a file of the repository: the tests run in this
// package's directory, two levels below the root.
func repo(rel string) string { return filepath.Join("..", "..", filepath.FromSlash(rel)) }

// page reads a page of docs/.
func page(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(repo("docs/" + name))
	require.NoError(t, err)
	return string(b)
}

// section returns the part of a page under the heading "## <heading>", up to
// the next heading of that level; its subsections are in it.
func section(t *testing.T, text, heading string) string {
	t.Helper()
	var out []string
	in := false
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "## "+heading:
			in = true
		case in && strings.HasPrefix(line, "## "):
			return strings.Join(out, "\n")
		case in:
			out = append(out, line)
		}
	}
	require.True(t, in, "there is no section %q in the page", heading)
	return strings.Join(out, "\n")
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

// firstCells returns the code spans of the first cell of every row of the
// tables in text, each row's spans together.
func firstCells(text string) [][]string {
	var rows [][]string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cell, _, _ := strings.Cut(line[1:], "|")
		var spans []string
		for _, m := range codeSpan.FindAllStringSubmatch(cell, -1) {
			spans = append(spans, m[1])
		}
		if len(spans) > 0 {
			rows = append(rows, spans)
		}
	}
	return rows
}

// spans returns every code span in the first cells of text's tables.
func spans(text string) []string {
	var out []string
	for _, row := range firstCells(text) {
		out = append(out, row...)
	}
	return out
}

// normalized is text with every run of white space made one space, so that a
// line that is wrapped in a page matches the string the code has.
func normalized(text string) string { return strings.Join(strings.Fields(text), " ") }
