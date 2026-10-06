package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
	"github.com/spf13/pflag"
)

// docsCmd writes what is made of the help of the commands: the command
// reference in docs/cli, and the man pages and the completion files of the
// package. It is hidden: it is for the build, not for an admin.
func (a *app) docsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "docs",
		Short:  "Write the command reference, the man pages and the completion files",
		Long:   "Write what is made of the help of the commands, for the repository and the package.",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	write := func(use, short string, run func(root *cobra.Command, dir string) error) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <dir>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := a.noJSON(cmd); err != nil {
					return err
				}
				return run(docsRoot(a.env), args[0])
			},
		}
	}
	cmd.AddCommand(
		write("markdown", "Write the command reference as Markdown, a page for each command", writeMarkdown),
		write("man", "Write a man page for each command, gzipped, dated by SOURCE_DATE_EPOCH",
			func(root *cobra.Command, dir string) error {
				date, err := sourceDate(a.getenv("SOURCE_DATE_EPOCH"), a.now)
				if err != nil {
					return err
				}
				return writeMan(root, dir, date)
			}),
		write("completion", "Write the completion scripts of bash, zsh and fish", writeCompletionFiles),
	)
	return cmd
}

// docsRoot is a tree of the commands to write the documents of, with the
// help and completion commands cobra otherwise adds only when it runs.
func docsRoot(e env) *cobra.Command {
	root := newRootCmdWith(e)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	return root
}

// documented returns the commands below c an admin can run, in the order of
// their names: no help command and nothing hidden.
func documented(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() {
			out = append(out, sub)
		}
	}
	return out
}

// walk calls fn for c and every command below it that is documented.
func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range documented(c) {
		walk(sub, fn)
	}
}

// writeTree writes files into dir, and removes what else dir holds whose
// name stale matches: what the generator wrote before for a command that is
// gone.
func writeTree(dir string, files map[string][]byte, stale func(name string) bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok && e.Type().IsRegular() && stale(e.Name()) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(dir, name)
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, files[name]) {
			continue
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// The command reference: a page for each command, named by its path, and an
// index of them by group.

// markdownPages returns the pages of the reference by their file names.
func markdownPages(root *cobra.Command) map[string][]byte {
	pages := map[string][]byte{"index.md": indexPage(root)}
	for _, c := range documented(root) {
		walk(c, func(c *cobra.Command) { pages[pageName(c)] = commandPage(c) })
	}
	return pages
}

func writeMarkdown(root *cobra.Command, dir string) error {
	return writeTree(dir, markdownPages(root), func(name string) bool { return strings.HasSuffix(name, ".md") })
}

// pageName is the file of the page of a command: pco-route-manual-add.md.
func pageName(c *cobra.Command) string {
	if !c.HasParent() {
		return "index.md"
	}
	return strings.ReplaceAll(c.CommandPath(), " ", "-") + ".md"
}

func indexPage(root *cobra.Command) []byte {
	var b bytes.Buffer
	b.WriteString("# Command reference\n\n")
	b.WriteString("The commands of `pco`, by group, as `pco help` shows them: each page holds the help of the\n" +
		"command, its examples and its flags.\n\n")
	writeLong(&b, root.Long)
	writeUsage(&b, root)
	for _, g := range root.Groups() {
		var cmds []*cobra.Command
		for _, c := range documented(root) {
			if c.GroupID == g.ID {
				cmds = append(cmds, c)
			}
		}
		if len(cmds) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", strings.TrimSuffix(g.Title, ":"))
		for _, c := range cmds {
			writeCommandList(&b, c, 0)
		}
		b.WriteString("\n")
	}
	var others []*cobra.Command
	for _, c := range documented(root) {
		if c.GroupID == "" {
			others = append(others, c)
		}
	}
	if len(others) > 0 {
		b.WriteString("## Other commands\n\n")
		for _, c := range others {
			writeCommandList(&b, c, 0)
		}
		b.WriteString("\n")
	}
	writeFlags(&b, root)
	return append(bytes.TrimRight(b.Bytes(), "\n"), '\n')
}

// writeCommandList writes c as an item of a list, and the commands below it
// as items of a list inside it.
func writeCommandList(b *bytes.Buffer, c *cobra.Command, depth int) {
	fmt.Fprintf(b, "%s- [%s](%s): %s\n", strings.Repeat("  ", depth), c.CommandPath(), pageName(c), inline(c.Short))
	for _, sub := range documented(c) {
		writeCommandList(b, sub, depth+1)
	}
}

func commandPage(c *cobra.Command) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", c.CommandPath())
	writeLong(&b, c.Long)
	writeUsage(&b, c)
	if subs := documented(c); len(subs) > 0 {
		b.WriteString("## Commands\n\n")
		for _, sub := range subs {
			fmt.Fprintf(&b, "- [%s](%s): %s\n", sub.CommandPath(), pageName(sub), inline(sub.Short))
		}
		b.WriteString("\n")
	}
	writeFlags(&b, c)
	b.WriteString("## See also\n\n")
	if p := c.Parent(); p.HasParent() {
		fmt.Fprintf(&b, "- [%s](%s): %s\n", p.CommandPath(), pageName(p), inline(p.Short))
	}
	b.WriteString("- [Command reference](index.md): every command of pco, by group\n")
	return b.Bytes()
}

func writeUsage(b *bytes.Buffer, c *cobra.Command) {
	// As pco --help has it: the flag makes the line end in [flags].
	c.InitDefaultHelpFlag()
	var usage []string
	if c.Runnable() {
		usage = append(usage, c.UseLine())
	}
	if c.HasAvailableSubCommands() {
		usage = append(usage, c.CommandPath()+" [command]")
	}
	writeFence(b, "Usage", strings.Join(usage, "\n"))
	if c.Example != "" {
		writeFence(b, "Examples", dedent(c.Example))
	}
}

func writeFlags(b *bytes.Buffer, c *cobra.Command) {
	c.InitDefaultHelpFlag()
	if c.HasAvailableLocalFlags() {
		writeFence(b, "Flags", dedent(c.LocalFlags().FlagUsages()))
	}
	if c.HasAvailableInheritedFlags() {
		writeFence(b, "Global flags", dedent(c.InheritedFlags().FlagUsages()))
	}
}

func writeFence(b *bytes.Buffer, title, text string) {
	fmt.Fprintf(b, "## %s\n\n```text\n%s\n```\n\n", title, strings.TrimRight(text, "\n"))
}

// writeLong writes the help of a command as Markdown: the paragraphs as
// they are, and the lines indented in it, such as a table of values, as a
// block of text.
func writeLong(b *bytes.Buffer, long string) {
	b.WriteString(markdownLong(long))
	b.WriteString("\n\n")
}

func markdownLong(long string) string {
	var out []string
	lines := strings.Split(strings.TrimRight(long, "\n"), "\n")
	for i := 0; i < len(lines); {
		if !indentedLine(lines[i]) {
			if line := strings.TrimRight(lines[i], " "); line == "" {
				out = append(out, "")
			} else {
				out = append(out, blockSafe(inline(line)))
			}
			i++
			continue
		}
		j := i
		for j < len(lines) && indentedLine(lines[j]) {
			j++
		}
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, "```text", dedent(strings.Join(lines[i:j], "\n")), "```")
		if j < len(lines) && lines[j] != "" {
			out = append(out, "")
		}
		i = j
	}
	return strings.Join(out, "\n")
}

func indentedLine(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// dedent takes from every line the indentation they all have.
func dedent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	common := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if common < 0 || n < common {
			common = n
		}
	}
	for i, l := range lines {
		if len(l) >= common && common > 0 {
			lines[i] = l[common:]
		}
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// blockSafe keeps a line of a paragraph from starting a block of Markdown of
// its own: a heading, a quote, a list or a rule.
var (
	blockStart = regexp.MustCompile(`^(#|>|[-+*] |[-=_*]+$)`)
	listNumber = regexp.MustCompile(`^(\d+)([.)] )`)
)

func blockSafe(line string) string {
	if blockStart.MatchString(line) {
		return `\` + line
	}
	return listNumber.ReplaceAllString(line, `$1\$2`)
}

// literal is what the help writes as it is typed, without the punctuation
// after it: a word with a part to fill in, such as <hostname>, manual/<id>
// or <bridge>[:<vlan>]; the name of a variable of the environment, such as
// $CREDENTIALS_DIRECTORY; a flag, such as --keep-template=false; and an
// absolute path, such as /etc/pve/pco/claims.
var literal = regexp.MustCompile(`[^\s(]*<[^>\s]+>[^\s,;.)]*` +
	`|\$[A-Z_][A-Z0-9_]*` +
	`|--[a-z][a-z0-9]*(?:-[a-z0-9]+)*(?:=[a-z0-9]+)?` +
	`|/[A-Za-z0-9_@-]+(?:/\.?[A-Za-z0-9_@-]+|\.[A-Za-z0-9_@-]+)+`)

// inline writes text for a line of Markdown: what is typed in a code span,
// and the characters that would mark up the rest escaped.
func inline(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range literal.FindAllStringIndex(text, -1) {
		// Only a whole word: not the -- or the / in the middle of one.
		if m[0] > 0 && !strings.ContainsRune(" (", rune(text[m[0]-1])) {
			continue
		}
		b.WriteString(escapeMarkdown(text[last:m[0]]))
		b.WriteString("`" + text[m[0]:m[1]] + "`")
		last = m[1]
	}
	b.WriteString(escapeMarkdown(text[last:]))
	return b.String()
}

// emphasis is an underscore or a star where it could open or close
// emphasis: at the edge of a word.
var emphasis = regexp.MustCompile(`(^|[^\pL\pN])([_*])|([_*])([^\pL\pN]|$)`)

func escapeMarkdown(text string) string {
	text = strings.NewReplacer(`\`, `\\`, "`", "\\`", "[", `\[`, "]", `\]`, "<", `\<`).Replace(text)
	return emphasis.ReplaceAllStringFunc(text, func(m string) string {
		return strings.NewReplacer("_", `\_`, "*", `\*`).Replace(m)
	})
}

// The man pages: one for each command, through cobra's own writer, gzipped.

// sourceDate is the date of the man pages: SOURCE_DATE_EPOCH, so that two
// builds of one commit write the same bytes, or now without it.
func sourceDate(epoch string, now func() time.Time) (time.Time, error) {
	if epoch == "" {
		return now().UTC(), nil
	}
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q is no number of seconds", epoch)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// manPages returns the gzipped man page of every command by its file name,
// pco-route-manual-add.1.gz. md2man reads the help as Markdown, so it is
// given as the reference has it, with the rest escaped.
func manPages(root *cobra.Command, date time.Time) (map[string][]byte, error) {
	escaped := map[*pflag.Flag]bool{}
	walk(root, func(c *cobra.Command) {
		if name, rest, ok := strings.Cut(c.Use, " "); ok {
			c.Use = name + " " + escapeMarkdown(rest)
		}
		c.Short = escapeMarkdown(c.Short)
		if c.Long != "" {
			c.Long = markdownLong(c.Long)
		}
		for _, set := range []*pflag.FlagSet{c.LocalFlags(), c.InheritedFlags()} {
			set.VisitAll(func(f *pflag.Flag) {
				if !escaped[f] {
					f.Usage = escapeMarkdown(f.Usage)
					escaped[f] = true
				}
			})
		}
	})
	pages := map[string][]byte{}
	var failed error
	walk(root, func(c *cobra.Command) {
		var page bytes.Buffer
		header := &doc.GenManHeader{Section: "1", Date: &date, Source: "pco", Manual: "pco manual"}
		if err := doc.GenMan(c, header, &page); err != nil {
			failed = errors.Join(failed, err)
			return
		}
		var gz bytes.Buffer
		w, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		if err == nil {
			_, err = w.Write(page.Bytes())
		}
		if err == nil {
			err = w.Close()
		}
		if err != nil {
			failed = errors.Join(failed, err)
			return
		}
		pages[strings.ReplaceAll(c.CommandPath(), " ", "-")+".1.gz"] = gz.Bytes()
	})
	return pages, failed
}

func writeMan(root *cobra.Command, dir string, date time.Time) error {
	pages, err := manPages(root, date)
	if err != nil {
		return err
	}
	return writeTree(dir, pages, func(name string) bool { return strings.HasSuffix(name, ".1.gz") })
}

// The completion scripts the package installs, by the names of their files
// in the build.
var completionFiles = map[string]string{"pco.bash": "bash", "pco.zsh": "zsh", "pco.fish": "fish"}

func completionScripts(root *cobra.Command) (map[string][]byte, error) {
	files := map[string][]byte{}
	for name, shell := range completionFiles {
		var b bytes.Buffer
		if err := writeCompletion(root, shell, &b, true); err != nil {
			return nil, err
		}
		files[name] = b.Bytes()
	}
	return files, nil
}

func writeCompletionFiles(root *cobra.Command, dir string) error {
	files, err := completionScripts(root)
	if err != nil {
		return err
	}
	return writeTree(dir, files, func(string) bool { return false })
}
