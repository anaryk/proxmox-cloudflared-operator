package doctor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// localEnv is a node whose daemon does not run: the host facts of fakeEnv and
// the egress table as the node has it.
type localEnv struct {
	*fakeEnv
	egress    engine.EgressView
	egressErr error
}

func (l *localEnv) Egress(context.Context) (engine.EgressView, error) { return l.egress, l.egressErr }

// healthyNode is a node on which only the daemon is not running: both units
// start at boot and the egress unit has loaded the table.
func healthyNode() *localEnv {
	env := healthyEnv()
	env.enabled = map[string]bool{daemonUnit: true, egressUnit: true}
	env.inactive = map[string]bool{daemonUnit: true}
	return &localEnv{fakeEnv: env, egress: engine.EgressView{State: engine.EgressOn}}
}

const (
	wantNeedDaemon = "the checks that need the daemon were not made: the mode, the last cycle, the inventory, " +
		"the credentials, the tunnels and their connectors, the way out to Cloudflare, the writer, Proxmox, " +
		"the lock of the node and what waits for a confirmation or an approval"
	wantFixStart   = "start it with systemctl start pco.service, then run pco doctor again"
	wantFixTable   = "pco egress show shows the table, pco egress load loads it again"
	wantFixStarted = "journalctl -u pco.service says what it waits for, then run pco doctor again"
)

func TestTheDoctorWithoutTheDaemonChecksWhatNeedsNone(t *testing.T) {
	findings := RunLocal(t.Context(), healthyNode())

	require.Equal(t, []Finding{
		{Check: "cloudflared", Level: LevelOK, Detail: "cloudflared 2026.9.0"},
		{Check: "daemon", Level: LevelWarn, Detail: "the daemon does not run; " + wantNeedDaemon, Fix: wantFixStart},
		{Check: "egress", Level: LevelOK, Detail: "the egress filter confines the connectors"},
		{Check: "store", Level: LevelOK, Detail: "the store is mounted and set up"},
		{Check: "unit pco-egress.service", Level: LevelOK, Detail: "pco-egress.service is running and starts at boot"},
		{Check: "unit pco.service", Level: LevelFail, Detail: "pco.service is not running: no command is answered and no cycle runs",
			Fix: "systemctl status pco.service; journalctl -u pco.service"},
	}, findings)
	require.True(t, Failed(findings), "a daemon that is not running is a failure")
}

func TestADaemonWhoseStateIsNotKnownIsNotSaidNotToRun(t *testing.T) {
	node := healthyNode()
	node.unitErr = errors.New("Failed to connect to bus")

	var got Finding
	for _, f := range RunLocal(t.Context(), node) {
		if f.Check == "daemon" {
			got = f
		}
	}

	require.Equal(t, Finding{Check: "daemon", Level: LevelWarn, Detail: "the daemon does not answer; " + wantNeedDaemon,
		Fix: "systemctl status pco.service; journalctl -u pco.service"}, got)
}

func TestWhatTheDoctorFindsWithoutTheDaemon(t *testing.T) {
	for _, tt := range []struct {
		name   string
		node   func(n *localEnv)
		check  string
		expect Finding
	}{
		{"a daemon that does not start at boot", func(n *localEnv) {
			n.inactive, n.enabled = map[string]bool{daemonUnit: true}, map[string]bool{egressUnit: true}
		}, "unit pco.service", Finding{Check: "unit pco.service", Level: LevelFail,
			Detail: "pco.service is not running and does not start at boot: no command is answered and no cycle runs",
			Fix:    "systemctl enable --now pco.service"}},
		{"a daemon that runs but does not answer", func(n *localEnv) { n.inactive = nil }, "daemon",
			Finding{Check: "daemon", Level: LevelWarn,
				Detail: "pco.service runs, but nothing answers on its socket; it may still be starting; " + wantNeedDaemon, Fix: wantFixStarted}},
		{"a daemon that runs but does not start at boot", func(n *localEnv) {
			n.inactive, n.enabled = nil, map[string]bool{egressUnit: true}
		}, "unit pco.service", Finding{Check: "unit pco.service", Level: LevelWarn,
			Detail: "pco.service is running but does not start at boot", Fix: "systemctl enable pco.service"}},
		{"an egress unit that does not run", func(n *localEnv) {
			n.inactive = map[string]bool{daemonUnit: true, egressUnit: true}
		}, "unit pco-egress.service", Finding{Check: "unit pco-egress.service", Level: LevelFail,
			Detail: "pco-egress.service is not running: the connectors, which require it, do not start",
			Fix:    "systemctl status pco-egress.service; journalctl -u pco-egress.service"}},
		{"an egress unit that does not start at boot", func(n *localEnv) { n.enabled = map[string]bool{daemonUnit: true} },
			"unit pco-egress.service", Finding{Check: "unit pco-egress.service", Level: LevelWarn,
				Detail: "pco-egress.service is running but does not start at boot", Fix: "systemctl enable pco-egress.service"}},
		{"systemd that does not say whether a unit runs", func(n *localEnv) { n.unitErr = errors.New("Failed to connect to bus") },
			"unit pco.service", Finding{Check: "unit pco.service", Level: LevelWarn,
				Detail: "systemd did not say whether it runs: Failed to connect to bus", Fix: "systemctl status pco.service"}},
		{"systemd that does not say whether a unit is enabled", func(n *localEnv) { n.enabledErr = errors.New("Failed to connect to bus") },
			"unit pco.service", Finding{Check: "unit pco.service", Level: LevelWarn,
				Detail: "systemd did not say whether it is enabled: Failed to connect to bus", Fix: "systemctl is-enabled pco.service"}},
		{"no cloudflared", func(n *localEnv) { n.versionErr = errors.New(`exec: "/usr/bin/cloudflared": file does not exist`) },
			"cloudflared", Finding{Check: "cloudflared", Level: LevelFail,
				Detail: `cloudflared does not run: exec: "/usr/bin/cloudflared": file does not exist`,
				Fix:    "install cloudflared from the package repository of Cloudflare"}},
		{"a cloudflared more than ten months old", func(n *localEnv) { n.version = "cloudflared version 2025.11.2 (built 2025-11-30)" },
			"cloudflared", Finding{Check: "cloudflared", Level: LevelWarn,
				Detail: "cloudflared 2025.11.2 is more than ten months old", Fix: "update cloudflared"}},
		{"a store that is not mounted", func(n *localEnv) { n.storeErr = store.ErrNotMounted }, "store",
			Finding{Check: "store", Level: LevelFail, Detail: store.ErrNotMounted.Error(), Fix: "systemctl status pve-cluster"}},
		{"a store that is not set up", func(n *localEnv) { n.storeErr = errors.New("pco is not set up on this node; run pco setup") }, "store",
			Finding{Check: "store", Level: LevelFail, Detail: "pco is not set up on this node; run pco setup", Fix: "pco setup"}},
		{"an egress filter switched off", func(n *localEnv) {
			n.egress = engine.EgressView{State: engine.EgressOff, Since: now.Add(-2 * time.Hour)}
		}, "egress", Finding{Check: "egress", Level: LevelFail,
			Detail: "the egress filter is switched off since 2026-10-01T10:00:00Z: the connectors are not confined", Fix: "pco egress on"}},
		{"an egress table that is not loaded", func(n *localEnv) { n.egress = engine.EgressView{State: engine.EgressNotLoaded} }, "egress",
			Finding{Check: "egress", Level: LevelFail,
				Detail: "the egress table is not loaded: the connectors are not confined", Fix: wantFixTable}},
		{"an egress table that is not the one pco loads", func(n *localEnv) { n.egress = engine.EgressView{State: engine.EgressChanged} }, "egress",
			Finding{Check: "egress", Level: LevelFail,
				Detail: "the egress table is not the one pco loads: the connectors may not be confined", Fix: wantFixTable}},
		{"an egress table that cannot be read", func(n *localEnv) { n.egressErr = errors.New("nft: Operation not permitted") }, "egress",
			Finding{Check: "egress", Level: LevelWarn,
				Detail: "the egress table could not be read: nft: Operation not permitted", Fix: "pco egress show"}},
		{"no connector user", func(n *localEnv) { n.egressErr = egress.ErrNoConnectorUser }, "egress",
			Finding{Check: "egress", Level: LevelFail,
				Detail: "the connector user " + egress.ConnectorUser + " does not exist: no connector starts, and the egress table cannot be compared",
				Fix:    "install the package again or run systemd-sysusers"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := healthyNode()
			tt.node(node)

			findings := RunLocal(t.Context(), node)

			var got []Finding
			for _, f := range findings {
				if f.Check == tt.check {
					got = append(got, f)
				}
			}
			require.Equal(t, []Finding{tt.expect}, got)
		})
	}
}

func TestADaemonThatRunsIsNotAFailureOfTheDoctorWithoutIt(t *testing.T) {
	node := healthyNode()
	node.inactive = nil

	findings := RunLocal(t.Context(), node)

	require.False(t, Failed(findings), "it may be starting: %v", findings)
}

func TestTheDoctorWithoutTheDaemonHasAFindingForEveryCheckItMakes(t *testing.T) {
	node := healthyNode()
	node.storeErr = errors.New("no")
	node.versionErr = errors.New("no")
	node.unitErr = errors.New("no")
	node.egressErr = errors.New("no")

	var checks []string
	for _, f := range RunLocal(t.Context(), node) {
		checks = append(checks, f.Check)
	}

	require.Equal(t, []string{"cloudflared", "daemon", "egress", "store", "unit pco-egress.service", "unit pco.service"}, checks)
}
