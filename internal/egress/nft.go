package egress

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

	// nftTimeout bounds one run of nft, so that an nft that hangs cannot hold
	// a cycle of the daemon or a command for ever.
	nftTimeout = time.Minute
	// maxListing is the most of a listing that is read: far more than a table
	// of many thousand targets takes.
	maxListing = 32 << 20
	// maxStderr is how much of what nft says about an error is kept.
	maxStderr = 4 << 10
)

// ErrNotLoaded says that the table is not there.
var ErrNotLoaded = errors.New("the egress table is not loaded")

// Nft runs nft for the filter.
type Nft interface {
	// Apply runs an nftables script as `nft -f -`: one transaction, which
	// changes all it says or nothing.
	Apply(ctx context.Context, script string) error
	// List returns the table as `nft -j list table inet pco_egress` prints
	// it, or ErrNotLoaded when there is none.
	List(ctx context.Context) ([]byte, error)
}

// NewNft returns an Nft that runs /usr/sbin/nft.
func NewNft() Nft { return nftCmd{bin: nftPath} }

type nftCmd struct{ bin string }

func (n nftCmd) Apply(ctx context.Context, script string) error {
	_, err := n.run(ctx, strings.NewReader(script), "-f", "-")
	return err
}

func (n nftCmd) List(ctx context.Context) ([]byte, error) {
	out, err := n.run(ctx, nil, "-j", "list", "table", "inet", tableName)
	var ne *nftError
	if errors.As(err, &ne) && ne.missing() {
		return nil, ErrNotLoaded
	}
	return out, err
}

// run runs nft with args and returns what it printed. Its messages are kept
// in English, since List reads them.
func (n nftCmd) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, nftTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, n.bin, args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Stdin = stdin
	stdout, stderr := &capped{max: maxListing}, &capped{max: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, &nftError{args: args, err: err, stderr: oneLine(stderr.buf.String())}
	}
	if stdout.over {
		return nil, fmt.Errorf("nft %s: more than %d bytes of output", strings.Join(args, " "), maxListing)
	}
	return stdout.buf.Bytes(), nil
}

// nftError is a run of nft that failed, with what it said.
type nftError struct {
	args   []string
	err    error
	stderr string
}

func (e *nftError) Error() string {
	msg := fmt.Sprintf("nft %s: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *nftError) Unwrap() error { return e.err }

// missing reports whether nft ran and said that the table does not exist.
func (e *nftError) missing() bool {
	var exit *exec.ExitError
	return errors.As(e.err, &exit) && strings.Contains(e.stderr, "No such file or directory")
}

// oneLine joins the lines nft writes about an error, the statement and the
// marks under it among them, so that the error reads on one line.
func oneLine(s string) string {
	var parts []string
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}

// capped keeps the first max bytes written to it and drops the rest, so that
// nft can neither make this process hold more nor block on a full pipe.
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
