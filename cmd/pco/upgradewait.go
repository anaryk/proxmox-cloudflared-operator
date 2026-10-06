package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
)

const (
	// installedPco is the pco the package installs.
	installedPco = "/usr/bin/pco"
	// daemonWait is how long the daemon has to come back after the package
	// of pco was installed, looked at once a second.
	daemonWait = 60 * time.Second
)

// daemonAnswers says whether the daemon answers before an upgrade: one that
// does not run then is not started by the package, and is not waited for.
func (a *app) daemonAnswers(ctx context.Context) bool {
	_, err := a.client().Version(ctx)
	return err == nil
}

// waitForDaemon waits until the daemon answers as the pco now installed. The
// package restarts pco.service and is content when systemd took the restart;
// a pco that cannot start is found only here.
func (a *app) waitForDaemon(ctx context.Context, res upgrade.Result, rollback bool, sleep func(context.Context, time.Duration) error, out *screen) error {
	want, err := a.installedPcoVersion(ctx)
	if err != nil {
		return fmt.Errorf("pco %s is installed, but %s does not say its version: %w", res.Target, installedPco, err)
	}
	var last string
	var lastErr error
	for range int(daemonWait / time.Second) {
		v, err := a.client().Version(ctx)
		if err == nil && v == want {
			out.printf("the daemon runs pco %s\n", want)
			return nil
		}
		last, lastErr = v, err
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	seen := fmt.Sprintf("still answers as %s %s later, not as %s", last, daemonWait, want)
	if lastErr != nil {
		seen = fmt.Sprintf("does not answer %s later (%s)", daemonWait, lastErr.Error())
	}
	if rollback {
		return fmt.Errorf("pco %s is installed again, but the daemon %s: systemctl status pco.service says why, "+
			"and the snapshot taken before the upgrade is the way back", res.Target, seen)
	}
	return fmt.Errorf("pco %s is installed, but the daemon %s: systemctl status pco.service says why, "+
		"and pco upgrade pco --rollback goes back to %s", res.Target, seen, res.Installed)
}

// installedPcoVersion is the version the pco just installed says it is,
// which its daemon answers with. A pco from before version --json says it
// in its first line, after its name.
func (a *app) installedPcoVersion(ctx context.Context) (string, error) {
	run := a.upgrade.run
	if out, err := run.Run(ctx, installedPco, "version", "--json"); err == nil {
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal([]byte(out), &v) == nil && v.Version != "" {
			return v.Version, nil
		}
	}
	out, err := run.Run(ctx, installedPco, "version")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "pco" {
		return "", fmt.Errorf("it says %q", strings.TrimSpace(out))
	}
	return fields[1], nil
}
