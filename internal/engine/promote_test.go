package engine

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// behindHost is a host whose store went back below what its agent
// remembers, after it wrote at Cloudflare as generation 1.
func behindHost(t *testing.T) (*env, *fakeLease) {
	t.Helper()
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	require.NotEmpty(t, e.writes())
	l.set(func(st *lease.State) { st.Behind = "an agent follows 4.later: the store went back" })
	require.NoError(t, e.store.SaveAgentEpoch(store.Epoch{InstallID: testInstall, Generation: 4, Nonce: "later"}))
	l.next = planner.Writer{InstallID: testInstall, Generation: 5, Nonce: "drawn"}
	l.saved = e.store.SaveWriter
	return e, l
}

func TestPromoteShowsTheDifferencesAndDrawsNothingUnasked(t *testing.T) {
	e, l := behindHost(t)

	res, err := e.eng.Promote(t.Context(), false)

	require.NoError(t, err)
	require.False(t, res.Drawn)
	require.Contains(t, res.Differences, DifferenceView{
		Subject: "epoch the agent of " + testNode + " remembers", Store: "1.n1", Reported: "4.later",
	})
	require.Empty(t, l.promoted)
	s, err := e.store.Settings()
	require.NoError(t, err)
	require.False(t, s.ObserveOnly)
}

func TestPromoteDrawsAboveTheSentinelsAndTheAgents(t *testing.T) {
	for name, tc := range map[string]struct {
		sentinel int
		want     int
	}{
		"an agent remembers more":      {sentinel: 1, want: 4},
		"Cloudflare holds a later one": {sentinel: 9, want: 9},
	} {
		t.Run(name, func(t *testing.T) {
			e, l := behindHost(t)
			if tc.sentinel > 1 {
				putSentinel(t, e, planner.Writer{InstallID: testInstall, Generation: tc.sentinel, Nonce: "else"})
			}

			res, err := e.eng.Promote(t.Context(), true)

			require.NoError(t, err)
			require.Equal(t, []int{tc.want}, l.promoted)
			require.Equal(t, PromoteResult{Differences: res.Differences, Drawn: true, Generation: 5, Nonce: "drawn"}, res)
			s, err := e.store.Settings()
			require.NoError(t, err)
			require.True(t, s.ObserveOnly, "a new epoch drawn by hand begins observe-only")
			require.True(t, hasLeaseEvent(e.eng, "promoted to generation 5"))
		})
	}
}

func TestPromoteRefusesWhatItCannotSee(t *testing.T) {
	e, l := behindHost(t)
	e.cf.FailNext("tunnel.read", 100, errors.New("timeout"))

	_, err := e.eng.Promote(t.Context(), true)

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "the generation at Cloudflare cannot be read")
	require.Empty(t, l.promoted)
}

func TestPromoteIsForTheHost(t *testing.T) {
	e := newEnv(t)

	_, err := e.eng.Promote(t.Context(), true)

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "pco appliance recover")
}

// A store made in another cluster becomes this cluster's once the admin
// confirms: the node takes the lease then, and the epoch is drawn.
func TestPromoteMakesThisClusterTheStores(t *testing.T) {
	e, l := onHost(t)
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	inst.Cluster = &store.ClusterIdentity{CASHA256: caOld, Name: "old-lab"}
	require.NoError(t, e.store.SaveInstall(inst))
	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, nil }
	l.next = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "drawn"}
	l.saved = e.store.SaveWriter

	res, err := e.eng.Promote(t.Context(), true)

	require.NoError(t, err)
	require.Contains(t, res.Differences, DifferenceView{
		Subject: "cluster", Store: "old-lab (" + caOld + ")", Reported: "lab (" + caNew + ")",
	})
	inst, _, err = e.store.Install()
	require.NoError(t, err)
	require.Equal(t, &store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, inst.Cluster)
	require.True(t, res.Drawn)
}

func TestClusterSettingsWaitForTheUpgradeToFinish(t *testing.T) {
	e := newEnv(t)
	v, err := e.eng.SettingsView()
	require.NoError(t, err)
	s := v.Settings
	s.Cluster.AdmissionMaxMoves = 20
	require.NoError(t, e.store.SaveUpgradeState(store.UpgradeState{From: "0.9.0", Since: t0}))

	_, _, err = e.eng.SaveSettings(t.Context(), v.Rev, s)

	var field *FieldError
	require.ErrorAs(t, err, &field)
	require.Equal(t, "cluster", field.Field)
	require.EqualError(t, field.Err, problemFinishUpgrade)

	require.NoError(t, e.store.DeleteUpgradeState())
	_, _, err = e.eng.SaveSettings(t.Context(), v.Rev, s)
	require.NoError(t, err)
}

// putSentinel puts the sentinel of w into the configuration of the tunnel
// of the install, as a writer of that epoch would.
func putSentinel(t *testing.T, e *env, w planner.Writer) {
	t.Helper()
	ts := e.tunnels()
	require.Len(t, ts, 1)
	cfg, err := e.cf.TunnelConfig(t.Context(), testAccount, ts[0].ID)
	require.NoError(t, err)
	var ingress []planner.IngressRule
	for _, r := range cfg.Ingress {
		if _, ok := planner.ParseSentinel(r.Hostname); ok {
			r.Hostname = planner.SentinelHostname(w)
		}
		ingress = append(ingress, r)
	}
	_, err = e.cf.PutTunnelConfig(t.Context(), testAccount, ts[0].ID, ingress)
	require.NoError(t, err)
}
