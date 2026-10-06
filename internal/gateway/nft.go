package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	nftPath = "/usr/sbin/nft"
	ipPath  = "/usr/sbin/ip"

	// nftTimeout bounds one run of nft, so that an nft that hangs holds no
	// command or cycle of the daemon for ever.
	nftTimeout = time.Minute
	// maxListing is the most of a listing that is read, far more than a table
	// of thousands of service addresses takes.
	maxListing = 32 << 20
	maxStderr  = 4 << 10
)

// NewHostNft returns the Nft of the host's own network namespace.
func NewHostNft() Nft { return nftCmd{argv: []string{nftPath}} }

// NewNamespaceNft returns the Nft of the network namespace pinned under a
// name, which it runs nft in through ip netns exec.
func NewNamespaceNft(name string) Nft {
	return nftCmd{argv: []string{ipPath, "netns", "exec", name, nftPath}}
}

type nftCmd struct{ argv []string }

func (n nftCmd) Apply(ctx context.Context, script string) error {
	_, err := n.run(ctx, strings.NewReader(script), "-f", "-")
	return err
}

func (n nftCmd) List(ctx context.Context, table string) ([]byte, error) {
	out, err := n.run(ctx, nil, "-j", "list", "table", "inet", table)
	var ne *nftError
	if errors.As(err, &ne) && ne.missing() {
		return nil, fmt.Errorf("table inet %s: %w", table, ErrNotLoaded)
	}
	return out, err
}

// run runs nft with args and returns what it printed, in English, which List
// reads.
func (n nftCmd) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, nftTimeout)
	defer cancel()
	argv := append(append([]string{}, n.argv...), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdin = stdin
	stdout, stderr := &capped{max: maxListing}, &capped{max: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, &nftError{argv: argv, err: err, stderr: oneLine(stderr.buf.String())}
	}
	if stdout.over {
		return nil, fmt.Errorf("%s: more than %d bytes of output", strings.Join(argv, " "), maxListing)
	}
	return stdout.buf.Bytes(), nil
}

// nftError is a run of nft that failed, with what it said.
type nftError struct {
	argv   []string
	err    error
	stderr string
}

func (e *nftError) Error() string {
	msg := fmt.Sprintf("%s: %v", strings.Join(e.argv, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *nftError) Unwrap() error { return e.err }

// missing reports whether nft ran and said that the table does not exist;
// ip netns exec says the same words of a namespace it cannot open.
func (e *nftError) missing() bool {
	var exit *exec.ExitError
	return errors.As(e.err, &exit) && strings.Contains(e.stderr, "No such file or directory") &&
		!strings.Contains(e.stderr, "Cannot open network namespace")
}

// oneLine joins the lines nft writes about an error, so that it reads on one
// line.
func oneLine(s string) string {
	var parts []string
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:room])
		c.over = true
		return len(p), nil
	}
	return c.buf.Write(p)
}
