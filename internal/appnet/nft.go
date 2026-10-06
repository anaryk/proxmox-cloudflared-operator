package appnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

const (
	nftPath = "/usr/sbin/nft"
	// nftTimeout bounds one run of nft, so that one that hangs holds neither
	// the boot nor the daemon's keeper for ever.
	nftTimeout = time.Minute
	// maxOutput is far more than the listing of the table takes, and than
	// nft says about an error.
	maxOutput = 1 << 20
)

// NewNft returns the nft of the table: Apply runs a script as `nft -f -`, and
// List lists inet pco_net, or returns egress.ErrNotLoaded when there is none.
func NewNft() egress.Nft { return nftCmd{bin: nftPath} }

type nftCmd struct{ bin string }

func (n nftCmd) Apply(ctx context.Context, script string) error {
	_, err := n.run(ctx, strings.NewReader(script), "-f", "-")
	return err
}

func (n nftCmd) List(ctx context.Context) ([]byte, error) {
	out, err := n.run(ctx, nil, "-j", "list", "table", "inet", tableName)
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(err.Error(), "No such file or directory") {
		return nil, egress.ErrNotLoaded
	}
	return out, err
}

// run runs nft with its messages in English, which List reads.
func (n nftCmd) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, nftTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, n.bin, args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Stdin = stdin
	stdout, stderr := &capped{max: maxOutput}, &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.Join(strings.Fields(stderr.buf.String()), " ")
		if msg != "" {
			return nil, fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("nft %s: %w", strings.Join(args, " "), err)
	}
	if stdout.over {
		return nil, fmt.Errorf("nft %s: more than %d bytes of output", strings.Join(args, " "), maxOutput)
	}
	return stdout.buf.Bytes(), nil
}

// capped keeps the first max bytes written to it, so that nft can neither
// make this process hold more nor block on a full pipe.
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
