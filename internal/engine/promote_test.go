package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
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

	res, err := e.eng.Promote(t.Context(), PromoteRequest{})

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

			res, err := confirmed(t, e, PromoteRequest{Confirm: true})

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

	_, err := confirmed(t, e, PromoteRequest{Confirm: true})

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "the generation at Cloudflare cannot be read")
	require.Empty(t, l.promoted)
}

func TestPromoteIsForTheHost(t *testing.T) {
	e := newEnv(t)

	_, err := e.eng.Promote(t.Context(), PromoteRequest{Confirm: true})

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "pco appliance recover")
}

// A store made in another cluster becomes this cluster's once the admin
// confirms with the name of the cluster it was made in: the identity is
// saved only once this node holds the lease, with the new epoch.
func TestPromoteMakesThisClusterTheStoresWithItsName(t *testing.T) {
	e, l := onHost(t)
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	inst.Cluster = &store.ClusterIdentity{CASHA256: caOld, Name: "old-lab"}
	inst.Restored = &store.Restored{BundleMadeAt: time.Unix(1700000000, 0).UTC(), TakenOverBy: testNode}
	require.NoError(t, e.store.SaveInstall(inst))
	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, nil }
	l.next = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "drawn"}
	l.saved = e.store.SaveWriter

	for _, typed := range []string{"", "lab", "old"} {
		_, err := confirmed(t, e, PromoteRequest{Confirm: true, Cluster: typed})
		require.ErrorIs(t, err, ErrRefused, typed)
		require.ErrorContains(t, err, "old-lab", typed)
	}
	require.Empty(t, l.promoted)
	stored, _, err := e.store.Install()
	require.NoError(t, err)
	require.Equal(t, caOld, stored.Cluster.CASHA256, "nothing saved")

	res, err := confirmed(t, e, PromoteRequest{Confirm: true, Cluster: "old-lab"})

	require.NoError(t, err)
	require.Contains(t, res.Differences, DifferenceView{
		Subject: "cluster", Store: "old-lab (" + caOld + ")", Reported: "lab (" + caNew + ")",
	})
	inst, _, err = e.store.Install()
	require.NoError(t, err)
	require.Equal(t, &store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, inst.Cluster)
	require.Nil(t, inst.Restored, "the restore is over: the other nodes of this cluster join it as usual")
	require.True(t, res.Drawn)
}

// A store restored in the cluster it was made in is no longer one once it
// is promoted.
func TestPromoteEndsTheRestoreOfAStoreOfThisCluster(t *testing.T) {
	e, l := behindHost(t)
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	inst.Restored = &store.Restored{BundleMadeAt: time.Unix(1700000000, 0).UTC()}
	require.NoError(t, e.store.SaveInstall(inst))

	res, err := confirmed(t, e, PromoteRequest{Confirm: true})

	require.NoError(t, err)
	require.True(t, res.Drawn)
	require.NotEmpty(t, l.promoted)
	inst, _, err = e.store.Install()
	require.NoError(t, err)
	require.Nil(t, inst.Restored)
}

// A store whose cluster had no name is promoted with its fingerprint; and
// while this node does not hold the lease, the sentinels are read only once
// the admin confirmed.
func TestPromoteOfAnUnnamedClustersStore(t *testing.T) {
	e, l := onHost(t)
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	inst.Cluster = &store.ClusterIdentity{CASHA256: caOld}
	require.NoError(t, e.store.SaveInstall(inst))
	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew}, nil }
	held := l.State()
	l.set(func(st *lease.State) { *st = lease.State{Waiting: "the lock is held"} })
	e.eng.d.NewClient = holdingOnly(l, e.newClient)

	res, err := e.eng.Promote(t.Context(), PromoteRequest{})
	require.NoError(t, err)
	require.Contains(t, res.Differences, DifferenceView{
		Subject: "sentinels at Cloudflare", Store: "1.n1", Reported: "read once confirmed: this node does not hold the lease yet",
	})

	l.next = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "drawn"}
	l.saved = e.store.SaveWriter
	l.set(func(st *lease.State) { *st = held })
	res, err = e.eng.Promote(t.Context(), PromoteRequest{Confirm: true, Cluster: caOld, Shown: res.Differences})
	require.NoError(t, err)
	require.True(t, res.Drawn)
}

// holdingOnly is the client factory of the daemon: none without the lease.
func holdingOnly(l *fakeLease, made ClientFactory) ClientFactory {
	return func(c store.Credential) (cfapi.API, error) {
		if !l.State().Holder {
			return nil, errors.New("this node does not hold a valid lease")
		}
		return made(c)
	}
}

// When the store and the agents agree and nothing holds the controller,
// there is nothing to promote: a new epoch would rewrite every sentinel for
// nothing, so it takes --force.
func TestPromoteRefusesWhenNothingApplies(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	l.next = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "drawn"}
	l.saved = e.store.SaveWriter

	res, err := e.eng.Promote(t.Context(), PromoteRequest{})
	require.NoError(t, err)
	require.Empty(t, res.Differences)

	_, err = confirmed(t, e, PromoteRequest{Confirm: true})
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "nothing differs")
	require.Empty(t, l.promoted)

	res, err = confirmed(t, e, PromoteRequest{Confirm: true, Force: true})
	require.NoError(t, err)
	require.True(t, res.Drawn)
}

// The reads at Cloudflare have a deadline of their own, inside what the
// command line waits for the answer.
func TestPromoteBoundsItsReadsAtCloudflare(t *testing.T) {
	e, _ := behindHost(t)
	asked := &timeouts{}
	e.eng.timeout = asked.withTimeout

	_, err := e.eng.Promote(t.Context(), PromoteRequest{})

	require.NoError(t, err)
	require.Equal(t, 1, asked.count(promoteReads))
	require.LessOrEqual(t, promoteReads, 45*time.Second)
	require.Less(t, promoteWait, 60*time.Second, "the command line waits a minute")
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

// confirmed is a promotion confirmed after the differences were shown, as
// pco promote asks.
func confirmed(t *testing.T, e *env, req PromoteRequest) (PromoteResult, error) {
	t.Helper()
	shown, err := e.eng.Promote(t.Context(), PromoteRequest{})
	require.NoError(t, err)
	req.Shown = shown.Differences
	return e.eng.Promote(t.Context(), req)
}

// What differs is read again once confirmed: when it is not what the admin
// was shown, nothing is drawn, also with --force, which only lets a
// promotion through when nothing differs.
func TestPromoteRefusesWhenWhatDiffersChangedSinceItWasShown(t *testing.T) {
	e, l := behindHost(t)
	shown, err := e.eng.Promote(t.Context(), PromoteRequest{})
	require.NoError(t, err)
	require.NoError(t, e.store.SaveAgentEpoch(store.Epoch{InstallID: testInstall, Generation: 6, Nonce: "newer"}))

	for _, force := range []bool{false, true} {
		_, err = e.eng.Promote(t.Context(), PromoteRequest{Confirm: true, Force: force, Shown: shown.Differences})
		require.ErrorIs(t, err, ErrRefused)
		require.ErrorContains(t, err, "changed since it was shown")
	}
	putSentinel(t, e, planner.Writer{InstallID: testInstall, Generation: 9, Nonce: "else"})
	_, err = confirmed(t, e, PromoteRequest{Confirm: true})
	require.NoError(t, err)
	require.Equal(t, []int{9}, l.promoted, "a sentinel above the agents, read after the confirmation")

	t.Run("a sentinel that moved", func(t *testing.T) {
		e, l := behindHost(t)
		shown, err := e.eng.Promote(t.Context(), PromoteRequest{})
		require.NoError(t, err)
		putSentinel(t, e, planner.Writer{InstallID: testInstall, Generation: 9, Nonce: "else"})

		_, err = e.eng.Promote(t.Context(), PromoteRequest{Confirm: true, Shown: shown.Differences})
		require.ErrorContains(t, err, "changed since it was shown")
		require.Empty(t, l.promoted)
	})
}

// restoredElsewhere is a host whose store was restored from a bundle of the
// cluster old-lab and taken into this one, after it wrote its tunnel.
func restoredElsewhere(t *testing.T) (*env, *fakeLease) {
	t.Helper()
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	inst.Cluster = &store.ClusterIdentity{CASHA256: caOld, Name: "old-lab"}
	inst.Restored = &store.Restored{BundleMadeAt: time.Unix(1700000000, 0).UTC(), TakenOverBy: testNode}
	require.NoError(t, e.store.SaveInstall(inst))
	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, nil }
	l.next = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "drawn"}
	l.saved = e.store.SaveWriter
	return e, l
}

// The promotion of a restored install asks the cycles for a new secret of
// every tunnel, which the first cycle that may write gives: the run tokens
// on the disks of the lost install stop working without anyone asking.
func TestPromoteOfARestoredInstallAsksForNewSecrets(t *testing.T) {
	e, _ := restoredElsewhere(t)

	res, err := confirmed(t, e, PromoteRequest{Confirm: true, Cluster: "old-lab"})

	require.NoError(t, err)
	require.True(t, res.Drawn)
	require.True(t, res.RotatesSecrets)
	rot, found, err := e.store.RestoreRotation()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, e.clock.now(), rot.Since)

	t.Run("not for a store that was not restored", func(t *testing.T) {
		e, _ := behindHost(t)
		res, err := confirmed(t, e, PromoteRequest{Confirm: true})
		require.NoError(t, err)
		require.False(t, res.RotatesSecrets)
		_, found, err := e.store.RestoreRotation()
		require.NoError(t, err)
		require.False(t, found)
	})
}

// A restored install is not promoted in a new cluster while a connector of
// it is connected at Cloudflare: the cluster it was made in still runs it.
// What cannot be read refuses as well.
func TestPromoteOfARestoredInstallRefusesWhileTheOldClusterRunsIt(t *testing.T) {
	e, l := restoredElsewhere(t)
	id := e.tunnels()[0].ID
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: "old-1", Connections: 4, OriginIP: "203.0.113.7"}})

	_, err := confirmed(t, e, PromoteRequest{Confirm: true, Cluster: "old-lab"})

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "the cluster the bundle was made in still runs install "+testInstall)
	require.ErrorContains(t, err, "203.0.113.7")
	require.Empty(t, l.promoted)
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	require.Equal(t, caOld, inst.Cluster.CASHA256, "nothing saved")
	require.NotNil(t, inst.Restored)

	e.cf.SetConnectors(testAccount, id, nil)
	made := e.eng.d.NewClient
	e.eng.d.NewClient = func(c store.Credential) (cfapi.API, error) {
		api, err := made(c)
		return connectorsUnread{api}, err
	}
	_, err = confirmed(t, e, PromoteRequest{Confirm: true, Cluster: "old-lab"})
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "cannot tell")
	require.Empty(t, l.promoted)

	e.eng.d.NewClient = made
	_, err = confirmed(t, e, PromoteRequest{Confirm: true, Cluster: "old-lab"})
	require.NoError(t, err)
	require.NotEmpty(t, l.promoted)
}

type connectorsUnread struct{ cfapi.API }

func (connectorsUnread) Connectors(context.Context, string, string) ([]cfapi.Connector, error) {
	return nil, errors.New("cloudflare api: HTTP 500: internal")
}
