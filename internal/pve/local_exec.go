package pve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// pveshPath is where Proxmox VE installs pvesh.
const pveshPath = "/usr/bin/pvesh"

// ExecPvesh is the Runner of NewLocal on a node: it runs pvesh itself, with
// a fixed environment, and keeps at most as much of what it prints as an
// answer of the API may hold.
func ExecPvesh() Runner { return execRunner{path: pveshPath, limit: maxBodyBytes} }

// execRunner runs path, whatever name it is asked for, keeping limit bytes
// of its stdout and of its stderr.
type execRunner struct {
	path  string
	limit int
}

// errTooMuch ends a command that printed more than its runner keeps.
var errTooMuch = errors.New("printed too much")

func (r execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.WaitDelay = 5 * time.Second
	stdout, stderr := &capped{limit: r.limit}, &capped{limit: r.limit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	switch {
	case stdout.over:
		return "", fmt.Errorf("%s printed more than %d bytes", name, r.limit)
	case err != nil:
		if detail := strings.TrimSpace(stderr.buf.String()); detail != "" {
			return "", fmt.Errorf("%s: %w: %s", name, err, detail)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return stdout.buf.String(), nil
}

// capped keeps up to limit bytes, and fails the write that would go past
// them, which ends the copy of the command's output.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		c.over = true
		return 0, errTooMuch
	}
	return c.buf.Write(p)
}
