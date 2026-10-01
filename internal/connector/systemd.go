package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	systemctlPath = "/usr/bin/systemctl"

	// exitNotActive is the exit code of systemctl is-active for a unit that
	// is not active.
	exitNotActive = 3
	// stateStarting is what is-active prints for a unit that was started and
	// has not signalled readiness yet, and for one that waits out its restart
	// back-off. Both have a start queued or a process that is up.
	stateStarting = "activating"
)

// Systemd is the slice of systemctl the manager needs.
type Systemd interface {
	// EnableNow is enable --now --no-block: cloudflared signals readiness
	// only after its first edge connection, so the call must not wait for it.
	EnableNow(ctx context.Context, unit string) error
	// DisableNow is disable --now --no-block: it queues the stop and does not
	// wait for the unit to drain.
	DisableNow(ctx context.Context, unit string) error
	// Restart does not wait for readiness either.
	Restart(ctx context.Context, unit string) error
	// IsActive reports whether the unit is started or starting, including the
	// restart back-off. Such a unit holds, or will start with, the files it
	// finds when it starts, so a change to them needs a restart.
	IsActive(ctx context.Context, unit string) (bool, error)
	// ListUnits returns the names of the loaded units that match the glob.
	ListUnits(ctx context.Context, pattern string) ([]string, error)
}

// NewSystemctl returns a Systemd that runs /usr/bin/systemctl.
func NewSystemctl() Systemd { return systemctl{bin: systemctlPath} }

type systemctl struct{ bin string }

func (s systemctl) EnableNow(ctx context.Context, unit string) error {
	_, err := s.run(ctx, "enable", "--now", "--no-block", "--", unit)
	return err
}

func (s systemctl) DisableNow(ctx context.Context, unit string) error {
	_, err := s.run(ctx, "disable", "--now", "--no-block", "--", unit)
	return err
}

func (s systemctl) Restart(ctx context.Context, unit string) error {
	_, err := s.run(ctx, "restart", "--no-block", "--", unit)
	return err
}

func (s systemctl) IsActive(ctx context.Context, unit string) (bool, error) {
	out, err := s.run(ctx, "is-active", "--", unit)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == exitNotActive {
		return strings.TrimSpace(out) == stateStarting, nil
	}
	return false, err
}

func (s systemctl) ListUnits(ctx context.Context, pattern string) ([]string, error) {
	out, err := s.run(ctx, "list-units", "--all", "--plain", "--no-legend", "--", pattern)
	if err != nil {
		return nil, err
	}
	var units []string
	for line := range strings.Lines(out) {
		if fields := strings.Fields(line); len(fields) > 0 {
			units = append(units, fields[0])
		}
	}
	return units, nil
}

// run returns what the command wrote to stdout, also when it failed. The
// error carries what it wrote to stderr.
func (s systemctl) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return stdout.String(), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return stdout.String(), fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
