package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// cliDocs is the command reference in the repository.
var cliDocs = filepath.Join("..", "..", "docs", "cli")

// The reference in docs/cli is what the commands say of themselves.
func TestTheCommandReferenceIsCurrent(t *testing.T) {
	want := markdownPages(docsRoot(defaultEnv()))
	entries, err := os.ReadDir(cliDocs)
	require.NoError(t, err, "run make docs-cli")
	have := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		have[name] = true
		page, ok := want[name]
		if !ok {
			t.Errorf("docs/cli/%s is the page of no command: run make docs-cli", name)
			continue
		}
		got, err := os.ReadFile(filepath.Join(cliDocs, name))
		require.NoError(t, err)
		if !bytes.Equal(got, page) {
			t.Errorf("docs/cli/%s is not what the command says: run make docs-cli", name)
		}
	}
	for name := range want {
		if !have[name] {
			t.Errorf("docs/cli/%s is missing: run make docs-cli", name)
		}
	}
}

// Every command an admin can run says what it does and shows how it is
// used: two to five invocations, each under a comment of one line.
func TestEveryCommandHasItsHelpAndExamples(t *testing.T) {
	walk(docsRoot(defaultEnv()), func(c *cobra.Command) {
		name := c.CommandPath()
		if c.Long == "" {
			t.Errorf("%s has no Long", name)
		}
		if c.Example == "" {
			t.Errorf("%s has no Example", name)
			return
		}
		lines := strings.Split(dedent(c.Example), "\n")
		runs := 0
		for i, line := range lines {
			switch {
			case line == "":
			case strings.HasPrefix(line, "#"):
				if i+1 == len(lines) || lines[i+1] == "" || strings.HasPrefix(lines[i+1], "#") {
					t.Errorf("%s: the comment %q is above no invocation", name, line)
				}
			default:
				runs++
				if i == 0 || !strings.HasPrefix(lines[i-1], "#") {
					t.Errorf("%s: %q has no comment above it", name, line)
				}
			}
		}
		if runs < 2 || runs > 5 {
			t.Errorf("%s shows %d invocations, want two to five", name, runs)
		}
	})
}

// pco --help and the reference group the commands the same way, and every
// command is in a group.
func TestEveryCommandIsInAGroup(t *testing.T) {
	root := docsRoot(defaultEnv())
	ids := map[string]bool{}
	for _, g := range root.Groups() {
		ids[g.ID] = true
	}
	for _, c := range documented(root) {
		require.True(t, ids[c.GroupID], "%s is in no group of commandGroups", c.Name())
	}
	var listed []string
	for _, g := range commandGroups {
		listed = append(listed, g.commands...)
	}
	for _, name := range listed {
		c, _, err := root.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, c.Name(), "commandGroups names %s, which is no command", name)
	}
	require.Len(t, listed, len(slices.Compact(slices.Sorted(slices.Values(listed)))), "a command is in two groups")
}

func TestTheHelpIsWrittenAsMarkdown(t *testing.T) {
	for _, tt := range []struct{ name, long, want string }{
		{"paragraphs stay", "One line\nand the next.\n\nAnother.", "One line\nand the next.\n\nAnother."},
		{"placeholders in code", "as manual/<id>, or pco@pve!vm<vmid>; and <bridge>[:<vlan>].",
			"as `manual/<id>`, or `pco@pve!vm<vmid>`; and `<bridge>[:<vlan>]`."},
		{"paths with placeholders", "(/sdn/zones/<zone>/<vnet> or", "(`/sdn/zones/<zone>/<vnet>` or"},
		{"variables in code", "in $CREDENTIALS_DIRECTORY, where", "in `$CREDENTIALS_DIRECTORY`, where"},
		{"an indented table is a block", "By --web-cert:\n  ca   the default\n  own  yours\nThen more.",
			"By `--web-cert`:\n\n```text\nca   the default\nown  yours\n```\n\nThen more."},
		{"marks are escaped", "a *b* [c] _d_ e_f `g` \\h", "a \\*b\\* \\[c\\] \\_d\\_ e_f \\`g\\` \\\\h"},
		{"a line is no list or heading", "first\n- second\n# third\n1. fourth", "first\n\\- second\n\\# third\n1\\. fourth"},
		{"flags in code", "--repair keeps it; with --keep-template=false, or --yes.",
			"`--repair` keeps it; with `--keep-template=false`, or `--yes`."},
		{"paths in code", "in /etc/pve/pco/claims, and /var/lib/pco/egress-off.json. Or (/etc/pco/profile) /root/.pco-x",
			"in `/etc/pve/pco/claims`, and `/var/lib/pco/egress-off.json`. Or (`/etc/pco/profile`) `/root/.pco-x`"},
		{"not inside a word", "qemu/101, pco-web, a--b, and/or", "qemu/101, pco-web, a--b, and/or"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, markdownLong(tt.long))
		})
	}
}

// docsInto runs pco docs <kind> <dir> as the build does, with
// SOURCE_DATE_EPOCH set.
func docsInto(t *testing.T, kind, dir string) {
	t.Helper()
	e := defaultEnv()
	e.getenv = func(name string) string {
		if name == "SOURCE_DATE_EPOCH" {
			return "1790000000" // 2026-09-21
		}
		return ""
	}
	cmd := newRootCmdWith(e)
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"docs", kind, dir})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
}

// files reads every file of dir, the man pages unpacked.
func files(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string][]byte{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out[e.Name()] = raw
	}
	return out
}

func gunzip(t *testing.T, raw []byte) string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	page, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(page)
}

// Two builds of one commit write the same man pages and completion files,
// byte for byte, dated by the commit.
func TestTheManPagesAndCompletionFilesAreTheSameTwice(t *testing.T) {
	var builds [2]map[string][]byte
	for i := range builds {
		dir := t.TempDir()
		docsInto(t, "man", filepath.Join(dir, "man"))
		docsInto(t, "completion", filepath.Join(dir, "completion"))
		builds[i] = files(t, filepath.Join(dir, "man"))
		for name, raw := range files(t, filepath.Join(dir, "completion")) {
			builds[i]["completion/"+name] = raw
		}
	}
	require.Equal(t, builds[0], builds[1])

	man := builds[0]
	require.Contains(t, man, "pco.1.gz")
	require.Contains(t, man, "pco-route-manual-add.1.gz")
	require.NotContains(t, man, "pco-help.1.gz")
	require.NotContains(t, man, "pco-docs.1.gz", "a hidden command has no page")
	require.NotContains(t, man, "pco-egress-load.1.gz", "a hidden command has no page")
	page := gunzip(t, man["pco-route-manual-add.1.gz"])
	require.Contains(t, page, `.TH "PCO-ROUTE-MANUAL-ADD" "1" "Sep 2026" "pco" "pco manual"`)
	require.Contains(t, page, `\fBpco route manual add <hostname> (--guest <owner> | --address <ipv4>) --port <port> [flags]\fP`,
		"the placeholders of the synopsis are kept")
	require.NotContains(t, page, "Auto generated")
	require.Contains(t, gunzip(t, man["pco-setup.1.gz"]), ".EX\nca        a key of its own", "an indented table stays one")

	for _, name := range []string{"completion/pco.bash", "completion/pco.zsh", "completion/pco.fish"} {
		require.Contains(t, string(man[name]), "__complete", name)
	}
}

// The test data of the commands never reaches what is published.
func TestTheReferenceHoldsNoTestData(t *testing.T) {
	dir := t.TempDir()
	docsInto(t, "markdown", filepath.Join(dir, "markdown"))
	docsInto(t, "man", filepath.Join(dir, "man"))
	docsInto(t, "completion", filepath.Join(dir, "completion"))
	for _, kind := range []string{"markdown", "man", "completion"} {
		for name, raw := range files(t, filepath.Join(dir, kind)) {
			text := string(raw)
			if strings.HasSuffix(name, ".gz") {
				text = gunzip(t, raw)
			}
			for _, word := range []string{"shop.cz", "example.dev", "o1i.cz"} {
				require.NotContains(t, text, word, "%s/%s", kind, name)
			}
		}
	}
}

// The site reads the pages as Vue templates: outside a block of code, {{
// would be taken for an expression.
func TestTheReferenceHasNoTemplateMarks(t *testing.T) {
	for name, page := range markdownPages(docsRoot(defaultEnv())) {
		fenced := false
		for line := range strings.SplitSeq(string(page), "\n") {
			if strings.HasPrefix(line, "```") {
				fenced = !fenced
				continue
			}
			require.False(t, !fenced && strings.Contains(line, "{{"), "%s: %q", name, line)
		}
	}
}
