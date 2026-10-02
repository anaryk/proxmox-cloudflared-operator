package doctor

import (
	"bytes"
	"context"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

const (
	// hostTimeout bounds every question to the host.
	hostTimeout = 5 * time.Second
	// maxVersionOutput is how much of what cloudflared --version prints is
	// kept; it prints a line.
	maxVersionOutput = 4 << 10

	defaultCloudflared = "/usr/bin/cloudflared"
)

// Proxmox is the part of the Proxmox client the doctor asks.
type Proxmox interface {
	Version(ctx context.Context) (pve.Version, error)
}

// HostEnv is the Env of the daemon on a node.
type HostEnv struct {
	Systemd    connector.Systemd
	Proxmox    Proxmox
	Interval   func() time.Duration // the poll interval of the engine
	Clock      func() time.Time
	StoreCheck func() error // nil when the store is mounted and set up
	LockCheck  func() error // nil when this daemon holds the lock of the node
	// Binary is the cloudflared the connectors run; empty is
	// /usr/bin/cloudflared, as the unit of a connector has it.
	Binary string
	// Dial connects for CanDial; nil is a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

var _ Env = (*HostEnv)(nil)

// CloudflaredVersion runs cloudflared --version, without a shell, and returns
// the first line it prints.
func (h *HostEnv) CloudflaredVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	bin := h.Binary
	if bin == "" {
		bin = defaultCloudflared
	}
	out := &capped{max: maxVersionOutput}
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return strings.TrimSpace(line), nil
}

// UnitActive asks systemd whether a unit runs.
func (h *HostEnv) UnitActive(ctx context.Context, unit string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	return h.Systemd.IsActive(ctx, unit)
}

// CanDial connects to addr and hangs up.
func (h *HostEnv) CanDial(ctx context.Context, network, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	dial := h.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	conn, err := dial(ctx, network, addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// PVEVersion asks the Proxmox API for its release.
func (h *HostEnv) PVEVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	v, err := h.Proxmox.Version(ctx)
	if err != nil {
		return "", err
	}
	return v.Release, nil
}

func (h *HostEnv) Store(context.Context) error    { return h.StoreCheck() }
func (h *HostEnv) NodeLock(context.Context) error { return h.LockCheck() }
func (h *HostEnv) PollInterval() time.Duration    { return h.Interval() }
func (h *HostEnv) Now() time.Time                 { return h.Clock() }

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }
