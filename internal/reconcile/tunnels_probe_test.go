package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// seedAt seeds a tunnel created at the given time.
func seedAt(f *cffake.Fake, account, name string, at time.Time) cfapi.Tunnel {
	f.SetNow(func() time.Time { return at })
	defer f.SetNow(nil)
	return f.SeedTunnel(account, name, nil)
}

func deleteTunnel(name, held string) Action {
	return Action{Kind: DeleteTunnel, Credential: "cred1", Target: name, Destructive: true, Applied: held == "", Held: held}
}

func TestTunnelProbeSweep(t *testing.T) {
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	old := seedAt(f, "acct1", "pco-abc_probe_old", t0.Add(-11*time.Minute))
	busy := seedAt(f, "acct1", "pco-abc_probe_busy", t0.Add(-time.Hour))
	f.SetConnectors("acct1", busy.ID, []cfapi.Connector{{ID: "c1", Connections: 1}})
	seedAt(f, "acct1", "pco-abc_probe_young", t0.Add(-9*time.Minute))
	seedAt(f, "acct1", "pco-abc_probe_edge", t0.Add(-10*time.Minute))
	seedAt(f, "acct1", "pco-abc_probe_ahead", t0.Add(time.Hour))
	seedAt(f, "acct1", "pco-abc_probe_ageless", time.Time{})
	seedAt(f, "acct1", "pco-abc-node1", t0.Add(-time.Hour))
	seedAt(f, "acct1", "pco-abc-probe-old", t0.Add(-time.Hour))
	seedAt(f, "acct1", "pco-xyz_probe_old", t0.Add(-time.Hour))
	writer, asked := scripted(answer{us: ours, stored: ours})

	res := reconcilerWith(Clients{"cred1": f}, writer).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, []string{"DeleteTunnel acct1 " + old.ID}, callsTo(f, "DeleteTunnel"))
	require.Equal(t, []string{"Connectors acct1 " + old.ID, "Connectors acct1 " + busy.ID}, callsTo(f, "Connectors"),
		"only probes old enough are asked about their connectors")
	require.Equal(t, []Action{deleteTunnel("pco-abc_probe_old", "")}, withoutDetail(res.Actions))
	require.Equal(t, "in account acct1: probe left behind, created "+t0.Add(-11*time.Minute).Format(time.RFC3339), res.Actions[0].Detail)
	require.Equal(t, 3, *asked, "once for the run, before the configuration read and before the delete")
	require.Len(t, f.TunnelsIn("acct1"), 9)
	require.True(t, res.Tunnels[0].Verified, "the sweep changes nothing about the tunnel of the account")
}

func TestTunnelProbeSweepNeverTouchesOtherTunnels(t *testing.T) {
	// A listing that holds more than its prefix asks for, as a server that
	// ignores the filter would send.
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	var extra []cfapi.Tunnel
	for i, name := range []string{"pco-abc", "pco-abc-node1", "pco-abc-probe-x", "pco-abc_probe_", "pco-abc_probe_X", "pco-abcd_probe_x", "pco-xyz_probe_x"} {
		extra = append(extra, cfapi.Tunnel{ID: fmt.Sprintf("extra-%d", i), Name: name, Status: "inactive", CreatedAt: t0.Add(-time.Hour)})
	}
	s := &spy{API: f, extraTunnels: extra}

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, callsTo(f, "DeleteTunnel", "Connectors"))
	require.Empty(t, res.Actions)
}

func TestTunnelProbeSweepOnlyWhenEnforcing(t *testing.T) {
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	seedAt(f, "acct1", "pco-abc_probe_old", t0.Add(-time.Hour))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Observe)

	require.Empty(t, callsTo(f, "Tunnels", "Connectors", "DeleteTunnel"))
	require.Empty(t, res.Actions)
}

func TestTunnelProbeSweepCoversEveryAccountLookedAt(t *testing.T) {
	f := newFake("acct1", "acct2", "acct3", "acct4")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	known := seedAt(f, "acct2", "pco-abc_probe_a", t0.Add(-time.Hour))
	unread := seedAt(f, "acct3", "pco-abc_probe_b", t0.Add(-time.Hour))
	unplanned := seedAt(f, "acct4", "pco-abc_probe_c", t0.Add(-time.Hour))
	denied := newFake("acct3")
	denied.Deny("tunnel.read")

	res := newReconciler(Clients{"cred1": f, "cred3": denied}, &clock{t0}).Run(context.Background(),
		[]planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct3", "cred3", app)},
		map[string]string{"acct2": "cred1", "acct4": "cred9"}, Enforce)

	require.Equal(t, []string{"DeleteTunnel acct2 " + known.ID}, callsTo(f, "DeleteTunnel"),
		"an account with a tunnel or none; not one whose lookup failed, nor one without a client")
	require.Empty(t, callsTo(denied, "Tunnels", "DeleteTunnel"))
	require.Len(t, res.Problems, 2, "problems: %v", res.Problems)
	require.Contains(t, f.TunnelsIn("acct3"), unread)
	require.Contains(t, f.TunnelsIn("acct4"), unplanned)
}

func TestTunnelProbeSweepFailuresGoOn(t *testing.T) {
	f := newFake("acct1", "acct2")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	f.SeedTunnel("acct2", testTunnel, rulesOf(ours, app))
	unasked := seedAt(f, "acct1", "pco-abc_probe_a", t0.Add(-time.Hour))
	refused := seedAt(f, "acct1", "pco-abc_probe_b", t0.Add(-time.Hour))
	gone := seedAt(f, "acct1", "pco-abc_probe_c", t0.Add(-time.Hour))
	swept := seedAt(f, "acct1", "pco-abc_probe_d", t0.Add(-time.Hour))
	elsewhere := seedAt(f, "acct2", "pco-abc_probe_e", t0.Add(-time.Hour))
	s := &spy{
		API:           f,
		connectorsErr: map[string]error{unasked.ID: errors.New("connection reset by peer")},
		deleteErr:     map[string]error{refused.ID: statusError(http.StatusForbidden), gone.ID: statusError(http.StatusNotFound)},
	}

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).Run(context.Background(),
		[]planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct2", "cred1", app)}, nil, Enforce)

	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, []string{
		"pco-abc_probe_a in account acct1: listing its connectors: connection reset by peer",
		"pco-abc_probe_b in account acct1: deleting the probe tunnel: " + statusError(http.StatusForbidden).Error(),
	}, res.Problems)
	require.Equal(t, []Action{
		deleteTunnel("pco-abc_probe_b", statusError(http.StatusForbidden).Error()),
		deleteTunnel("pco-abc_probe_c", ""),
		deleteTunnel("pco-abc_probe_d", ""),
		deleteTunnel("pco-abc_probe_e", ""),
	}, withoutDetail(res.Actions), "a probe that is gone already counts as deleted")
	require.NotContains(t, f.TunnelsIn("acct1"), swept)
	require.NotContains(t, f.TunnelsIn("acct2"), elsewhere)
}

func TestTunnelProbeSweepListingFails(t *testing.T) {
	f := newFake("acct1", "acct2")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	f.SeedTunnel("acct2", testTunnel, rulesOf(ours, app))
	seedAt(f, "acct1", "pco-abc_probe_a", t0.Add(-time.Hour))
	elsewhere := seedAt(f, "acct2", "pco-abc_probe_b", t0.Add(-time.Hour))
	s := &spy{API: f, tunnelsErr: map[string]error{"acct1": errors.New("connection reset by peer")}}

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).Run(context.Background(),
		[]planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct2", "cred1", app)}, nil, Enforce)

	require.Equal(t, []string{"account acct1: listing probe tunnels: connection reset by peer"}, res.Problems)
	require.Equal(t, []string{"DeleteTunnel acct2 " + elsewhere.ID}, callsTo(f, "DeleteTunnel"))
}

func TestTunnelProbeSweepFencedByWriter(t *testing.T) {
	cases := []struct {
		name    string
		before  answer // what the writer callback answers right before the delete
		verdict WriterVerdict
		says    string
	}{
		{"taken over", answer{us: ours, stored: writerAt(6, "n6")}, WriterStale, "this writer is stale and stops"},
		{"unreadable", answer{err: errors.New("lease lost")}, WriterProceed, "reading the writer identity: lease lost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1", "acct2")
			f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
			f.SeedTunnel("acct2", testTunnel, rulesOf(ours, app))
			seedAt(f, "acct1", "pco-abc_probe_a", t0.Add(-time.Hour))
			seedAt(f, "acct1", "pco-abc_probe_b", t0.Add(-time.Hour))
			seedAt(f, "acct2", "pco-abc_probe_c", t0.Add(-time.Hour))
			writer, asked := scripted(
				answer{us: ours, stored: ours}, // start
				answer{us: ours, stored: ours}, // before the configuration read of acct1
				answer{us: ours, stored: ours}, // and of acct2
				tc.before,
			)

			res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(),
				[]planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct2", "cred1", app)}, nil, Enforce)

			require.Equal(t, tc.verdict, res.Verdict)
			require.Empty(t, callsTo(f, "DeleteTunnel"))
			require.Equal(t, []string{"Tunnels acct1 pco-abc_probe_"}, callsTo(f, "Tunnels"), "the sweep stops")
			require.Equal(t, 4, *asked)
			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], "pco-abc_probe_a in account acct1: ")
			require.Contains(t, res.Problems[0], tc.says)
			require.Empty(t, res.Actions)
		})
	}
}

func TestTunnelProbeSweepNotAfterTheRunStopped(t *testing.T) {
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(5, "zz"), app))
	seedAt(f, "acct1", "pco-abc_probe_a", t0.Add(-time.Hour))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Equal(t, WriterForeign, res.Verdict)
	require.Empty(t, callsTo(f, "Tunnels", "DeleteTunnel"))
}
