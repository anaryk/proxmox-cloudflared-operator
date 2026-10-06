package doctor

import (
	"context"
	"errors"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	daemonUnit = "pco.service"
	egressUnit = "pco-egress.service"

	fixLocalTable = "pco egress show shows the table, pco egress load loads it again"

	// needsDaemon is what a doctor without the daemon could not look at.
	needsDaemon = "the checks that need the daemon were not made: the mode, the last cycle, the inventory, " +
		"the credentials, the tunnels and their connectors, the way out to Cloudflare, the writer, Proxmox, " +
		"the lock of the node and what waits for a confirmation or an approval"
)

// LocalEnv is what the doctor reads from the node when the daemon does not
// answer.
type LocalEnv interface {
	facts
	// Egress is the egress filter as the node has it: switched off, its table
	// not loaded, not the one pco loads, or on. A table that cannot be read
	// is an error.
	Egress(ctx context.Context) (engine.EgressView, error)
}

// unitState is what is known of whether a unit runs.
type unitState int

const (
	unitUnknown unitState = iota
	unitStopped
	unitRunning
)

// RunLocal makes the checks that need no daemon, for a node whose daemon does
// not answer: the units of pco, the store, cloudflared and the egress table.
// It says that the rest was not made. Every check has a finding, sorted by
// check.
func RunLocal(ctx context.Context, env LocalEnv) []Finding {
	daemon, daemonState := checkUnit(ctx, env, daemonUnit, "no command is answered and no cycle runs")
	table, _ := checkUnit(ctx, env, egressUnit, "the connectors, which require it, do not start")
	out := []Finding{
		daemon, table, checkSilentDaemon(daemonState),
		checkCloudflared(ctx, env), checkStore(ctx, env, ""), checkLocalEgress(ctx, env),
	}
	slices.SortStableFunc(out, compareChecks)
	return out
}

// checkUnit says whether a unit runs and starts at boot. A unit that does not
// run is a failure, and what that means is consequence; one that runs but
// does not start at boot is a warning.
func checkUnit(ctx context.Context, env LocalEnv, unit, consequence string) (Finding, unitState) {
	check := "unit " + unit
	active, err := env.UnitActive(ctx, unit)
	if err != nil {
		return warn(check, "systemd did not say whether it runs: "+err.Error(), "systemctl status "+unit), unitUnknown
	}
	state := unitStopped
	if active {
		state = unitRunning
	}
	enabled, err := env.UnitEnabled(ctx, unit)
	switch {
	case err != nil:
		return warn(check, "systemd did not say whether it is enabled: "+err.Error(), "systemctl is-enabled "+unit), state
	case !active && !enabled:
		return fail(check, unit+" is not running and does not start at boot: "+consequence, "systemctl enable --now "+unit), state
	case !active:
		return fail(check, unit+" is not running: "+consequence, "systemctl status "+unit+"; journalctl -u "+unit), state
	case !enabled:
		return warn(check, unit+" is running but does not start at boot", "systemctl enable "+unit), state
	}
	return ok(check, unit+" is running and starts at boot"), state
}

// checkSilentDaemon says that the daemon does not answer, and so what was not
// checked. It is a warning: a daemon that is not running is a failure of its
// unit, and one that runs may be starting.
func checkSilentDaemon(state unitState) Finding {
	switch state {
	case unitRunning:
		return warn("daemon", daemonUnit+" runs, but nothing answers on its socket; it may still be starting; "+needsDaemon,
			"journalctl -u "+daemonUnit+" says what it waits for, then run pco doctor again")
	case unitStopped:
		return warn("daemon", "the daemon does not run; "+needsDaemon, "start it with systemctl start "+daemonUnit+", then run pco doctor again")
	}
	return warn("daemon", "the daemon does not answer; "+needsDaemon, "systemctl status "+daemonUnit+"; journalctl -u "+daemonUnit)
}

// checkLocalEgress says how the node has its egress table, read without the
// daemon.
func checkLocalEgress(ctx context.Context, env LocalEnv) Finding {
	v, err := env.Egress(ctx)
	switch {
	case errors.Is(err, egress.ErrNoConnectorUser):
		return fail("egress", "the connector user "+egress.ConnectorUser+" does not exist: no connector starts, and the egress table cannot be compared",
			"install the package again or run systemd-sysusers")
	case err != nil:
		return warn("egress", "the egress table could not be read: "+err.Error(), "pco egress show")
	}
	return checkEgress(v, fixLocalTable)
}
