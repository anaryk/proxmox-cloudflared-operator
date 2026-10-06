package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	systemctlPath      = "/usr/bin/systemctl"
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
	// Timeout bounds every question to the host; zero is hostTimeout.
	Timeout time.Duration
	// Enabled asks systemd whether a unit starts at boot; nil runs
	// systemctl is-enabled.
	Enabled func(ctx context.Context, unit string) (bool, error)
	// FileState asks systemd for the unit file state of a unit, the word
	// systemctl is-enabled prints; nil runs systemctl is-enabled.
	FileState func(ctx context.Context, unit string) (string, error)
	// Web reads the certificate of the web interface, and says whether setup
	// set the web interface up; nil is a node without it.
	Web func(ctx context.Context) (WebCert, bool)
	// App is the part of an appliance; nil on a host.
	App ApplianceEnv
}

func (h *HostEnv) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return hostTimeout
}

var _ Env = (*HostEnv)(nil)

// Appliance is the part of the appliance, nil on a host.
func (h *HostEnv) Appliance() ApplianceEnv { return h.App }

// CloudflaredVersion runs cloudflared --version, without a shell, and returns
// the first line it prints. A cloudflared that does not answer within the
// timeout is an error that wraps context.DeadlineExceeded; a child of it that
// keeps the output open is not waited for longer than that.
func (h *HostEnv) CloudflaredVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	bin := h.Binary
	if bin == "" {
		bin = defaultCloudflared
	}
	out := &capped{max: maxVersionOutput}
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stdout, cmd.Stderr = out, out
	// Once it has ended, what it left behind is not waited for long.
	cmd.WaitDelay = min(h.timeout()/2, time.Second)
	err := cmd.Run()
	switch {
	case errors.Is(err, exec.ErrWaitDelay):
		// It answered and ended; what it left behind held the output open.
	case ctx.Err() != nil:
		return "", fmt.Errorf("cloudflared --version did not answer within %s: %w", h.timeout(), context.DeadlineExceeded)
	case err != nil:
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return strings.TrimSpace(line), nil
}

// UnitActive asks systemd whether a unit runs.
func (h *HostEnv) UnitActive(ctx context.Context, unit string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	return h.Systemd.IsActive(ctx, unit)
}

// UnitEnabled asks systemd whether a unit starts at boot.
func (h *HostEnv) UnitEnabled(ctx context.Context, unit string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	if h.Enabled != nil {
		return h.Enabled(ctx, unit)
	}
	return systemctlEnabled(ctx, unit)
}

// UnitFileState asks systemd for the unit file state of a unit: enabled,
// disabled, masked, static, and so on.
func (h *HostEnv) UnitFileState(ctx context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	if h.FileState != nil {
		return h.FileState(ctx, unit)
	}
	return systemctlState(ctx, unit)
}

// notFound is the unit file state of a unit there is none of.
const notFound = "not-found"

// systemctlEnabled runs systemctl is-enabled. A unit that does not exist is
// not enabled; what systemctl prints on another failure is the error.
func systemctlEnabled(ctx context.Context, unit string) (bool, error) {
	state, err := systemctlState(ctx, unit)
	return state == "enabled" || state == "enabled-runtime", err
}

// systemctlState runs systemctl is-enabled and returns the word it prints, or
// notFound for a unit that does not exist; what it prints on another failure
// is the error.
func systemctlState(ctx context.Context, unit string) (string, error) {
	var stdout, stderr capped
	stdout.max, stderr.max = maxVersionOutput, maxVersionOutput
	cmd := exec.CommandContext(ctx, systemctlPath, "is-enabled", "--", unit)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return unitFileState(stdout.String(), stderr.String(), err)
}

// unitFileState reads what systemctl is-enabled answered.
func unitFileState(stdout, stderr string, err error) (string, error) {
	state, _, _ := strings.Cut(strings.TrimSpace(stdout), "\n")
	switch {
	case state != "":
		return state, nil
	case strings.Contains(stderr, "No such file or directory"):
		return notFound, nil
	case err != nil:
		if detail := strings.TrimSpace(stderr); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return "", errors.New("systemctl is-enabled printed nothing")
}

// CanDial connects to addr and hangs up.
func (h *HostEnv) CanDial(ctx context.Context, network, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
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
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	v, err := h.Proxmox.Version(ctx)
	if err != nil {
		return "", err
	}
	return v.Release, nil
}

// Store checks the store, within the timeout: a cluster filesystem that
// stalls does not hold the doctor.
func (h *HostEnv) Store(ctx context.Context) error { return h.within(ctx, h.StoreCheck) }

// NodeLock checks the lock of the node, within the timeout.
func (h *HostEnv) NodeLock(ctx context.Context) error { return h.within(ctx, h.LockCheck) }

func (h *HostEnv) PollInterval() time.Duration { return h.Interval() }
func (h *HostEnv) Now() time.Time              { return h.Clock() }

// within runs check and gives up on it after the timeout, or when ctx ends.
// A check given up on is left to finish on its own.
func (h *HostEnv) within(ctx context.Context, check func() error) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- check() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("no answer within %s: %w", h.timeout(), ctx.Err())
	}
}

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
