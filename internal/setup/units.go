package setup

import (
	"context"
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
	out, err := c.run.Run(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--", pattern)
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
