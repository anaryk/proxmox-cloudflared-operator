package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const (
	systemctlPath  = "/usr/bin/systemctl"
	journalctlPath = "/usr/bin/journalctl"

	// exitNotActive is the exit code of systemctl is-active for a unit that
	// is not active.
	exitNotActive = 3
	// exitNotFailed is the exit code of systemctl is-failed for a unit that
	// has not failed, or is not loaded.
	exitNotFailed = 1
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

// failedResetter is a Systemd that can also clear the failed state of a unit.
// A stop leaves a unit that failed as it is, loaded and listed, so the manager
// asks for the reset where it is there; a Systemd without it is left out.
type failedResetter interface {
	ResetFailed(ctx context.Context, unit string) error
}

// NewSystemctl returns a Systemd that runs /usr/bin/systemctl, and reads what
// a unit logged with /usr/bin/journalctl.
func NewSystemctl() Systemd { return systemctl{bin: systemctlPath, journal: journalctlPath} }

type systemctl struct{ bin, journal string }

// Journal returns the last lines a unit logged, oldest first, without their
// metadata.
func (s systemctl) Journal(ctx context.Context, unit string, lines int) ([]string, error) {
	args := []string{"--quiet", "--no-pager", "--output=cat", "--lines=" + strconv.Itoa(lines), "--unit=" + unit}
	cmd := exec.CommandContext(ctx, s.journal, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("journalctl %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("journalctl %s: %w", strings.Join(args, " "), err)
	}
	var out []string
	for line := range strings.Lines(stdout.String()) {
		out = append(out, strings.TrimSuffix(line, "\n"))
	}
	return out, nil
}

func (s systemctl) EnableNow(ctx context.Context, unit string) error {
	_, err := s.run(ctx, "enable", "--now", "--no-block", "--", unit)
	return err
}

func (s systemctl) DisableNow(ctx context.Context, unit string) error {
	_, err := s.run(ctx, "disable", "--now", "--no-block", "--", unit)
	return err
}

// ResetFailed clears the failed state of a unit that is in it and does nothing
// to any other: reset-failed on a unit that is not loaded is an error.
func (s systemctl) ResetFailed(ctx context.Context, unit string) error {
	_, err := s.run(ctx, "is-failed", "--", unit)
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == exitNotFailed:
		return nil
	default:
		return err
	}
	_, err = s.run(ctx, "reset-failed", "--", unit)
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
	return ListUnits(ctx, s.run, pattern)
}

// ListUnits lists the loaded units that match the glob, also those that
// stopped or failed, with the systemctl that run stands for: run gets the
// arguments and returns what the command wrote to stdout. It is the one
// listing the daemon and setup share.
func ListUnits(ctx context.Context, run func(ctx context.Context, args ...string) (string, error), pattern string) ([]string, error) {
	out, err := run(ctx, "list-units", "--all", "--plain", "--no-legend", "--", pattern)
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
