package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

// unitControl is the systemctl of the connector manager, run through the
// Runner. Unlike the daemon's, a stop waits until the unit has stopped: the
// tunnel of a connector is deleted next, which Cloudflare refuses while the
// connector is still connected.
type unitControl struct{ run Runner }

var _ connector.Systemd = unitControl{}

func (c unitControl) EnableNow(ctx context.Context, unit string) error {
	_, err := c.run.Run(ctx, "systemctl", "enable", "--now", "--", unit)
	return err
}

func (c unitControl) DisableNow(ctx context.Context, unit string) error {
	_, err := c.run.Run(ctx, "systemctl", "disable", "--now", "--", unit)
	return err
}

func (c unitControl) Restart(ctx context.Context, unit string) error {
	_, err := c.run.Run(ctx, "systemctl", "restart", "--", unit)
	return err
}

func (c unitControl) IsActive(ctx context.Context, unit string) (bool, error) {
	out, err := c.run.Run(ctx, "systemctl", "is-active", "--", unit)
	switch strings.TrimSpace(out) {
	case "active", "activating", "reloading":
		return true, nil
	case "inactive", "failed", "deactivating":
		return false, nil
	}
	return false, err
}

func (c unitControl) ListUnits(ctx context.Context, pattern string) ([]string, error) {
	return connector.ListUnits(ctx, func(ctx context.Context, args ...string) (string, error) {
		return c.run.Run(ctx, "systemctl", args...)
	}, pattern)
}

// NamespaceUnit runs pco-netns.service for the daemon, which starts it for
// the probe before the first network, enables it with the first network and
// disables it when the last is detached. Start and Enable wait for the unit:
// the namespace is up when they return. The unit has no stop action, so the
// stop of Disable takes nothing down.
type NamespaceUnit struct{ run Runner }

// NewNamespaceUnit returns the unit of the gateway namespace, run through r.
func NewNamespaceUnit(r Runner) NamespaceUnit { return NamespaceUnit{run: r} }

func (u NamespaceUnit) Start(ctx context.Context) error { return u.systemctl(ctx, "start") }

func (u NamespaceUnit) Enable(ctx context.Context) error { return u.systemctl(ctx, "enable", "--now") }

func (u NamespaceUnit) Disable(ctx context.Context) error {
	return u.systemctl(ctx, "disable", "--now")
}

func (u NamespaceUnit) systemctl(ctx context.Context, args ...string) error {
	if _, err := u.run.Run(ctx, "systemctl", append(args, "--", netnsUnit)...); err != nil {
		return fmt.Errorf("systemctl %s %s: %w", strings.Join(args, " "), netnsUnit, err)
	}
	return nil
}
