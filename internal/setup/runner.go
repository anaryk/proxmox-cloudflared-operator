package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Runner executes host commands.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// ErrCommandNotFound is the error of a command that is not installed.
var ErrCommandNotFound = errors.New("command not found")

const (
	// commandPath is where commands are looked up, whatever the PATH of the
	// caller says.
	commandPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// maxStderr is how much of what a command wrote to stderr goes into its
	// error.
	maxStderr = 1024

	// waitDelay is how long a command that ctx ended has to close its output
	// before it is left behind.
	waitDelay = 5 * time.Second
)

// NewHostRunner returns the runner that runs commands on this host. A command
// is looked up in a fixed PATH, unless it is given by its absolute path, and
// runs with a minimal environment and stdin from /dev/null.
//
// Its stdout is returned and never logged, as it may hold a secret, such as
// the one of a Proxmox token. What the command wrote to stderr is put into the
// error, trimmed and with printable characters only.
func NewHostRunner() Runner { return hostRunner{} }

type hostRunner struct{}

func (hostRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	path, err := lookPath(name, filepath.SplitList(commandPath))
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = commandEnv()
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%w)", ctx.Err(), err)
		}
		return stdout.String(), commandError(name, args, err, stderr.String())
	}
	return stdout.String(), nil
}

// commandEnv is the environment of every command: the fixed PATH, the C
// locale, so that what is parsed is not translated, and apt without
// questions.
func commandEnv() []string {
	return []string{"PATH=" + commandPath, "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}
}

// lookPath finds a command in dirs. A name with a slash must be absolute and
// is taken as it is.
func lookPath(name string, dirs []string) (string, error) {
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) {
			return "", fmt.Errorf("%s: a command is given by name or by absolute path", name)
		}
		if _, err := os.Stat(name); errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s: %w", name, ErrCommandNotFound)
		}
		return name, nil
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, ErrCommandNotFound)
}

// commandError is the error of a command that failed, with what it wrote to
// stderr.
func commandError(name string, args []string, err error, stderr string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	if detail := printableText(stderr, maxStderr); detail != "" {
		return fmt.Errorf("%s: %w: %s", line, err, detail)
	}
	return fmt.Errorf("%s: %w", line, err)
}

// printableText returns s with its lines joined by "; ", without the
// characters that are not printable and cut to at most limit bytes.
func printableText(s string, limit int) string {
	var lines []string
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			switch {
			case r == '\t':
				return ' '
			case unicode.IsPrint(r):
				return r
			}
			return -1
		}, line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	text := strings.Join(lines, "; ")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}
