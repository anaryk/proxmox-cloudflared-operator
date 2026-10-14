package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// takeOver has a new controller process on this node take the lease under
// a new epoch, as one that takes over after another was lost, with what
// the agents do meanwhile set by during, and runs its cycles until its
// wait for the agents is over and it serves snapshots of its epoch.
func (w *world) takeOver(t *testing.T, during func()) {
	t.Helper()
	next := planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	require.NoError(t, w.store.SaveWriter(next))
	if during != nil {
		during()
	}
	w.lease.set(func(st *lease.State) { st.Writer, st.Record.Writer = next, next })
	w.eng = w.newEngine()
	w.watchHere(t)
	for range 20 {
		w.tick(10 * time.Second)
		if s, ok := w.r.lastOf("pve3"); ok && s.Epoch.Generation == 2 {
			return
		}
	}
	t.Fatal("the controller that took over serves no snapshot")
}

// dbOnPve3 is the target of db in the snapshot last served to pve3, and
// whether it lists it.
func (w *world) dbOnPve3() (agentproto.Target, bool) {
	s, _ := w.r.lastOf("pve3")
	for _, tg := range s.Targets {
		if tg.Key == "db.example.com" {
			return tg, true
		}
	}
	return agentproto.Target{}, false
}

// After a takeover, a replica that served a guest of another node on that
// node's attestation keeps it while the node, neither down nor gone, has not
// reported to the new controller yet and coverage is not judged: the new
// snapshot gives it back what it reported it served on, address,
// attestation and coverage, and nothing more.
func TestATakeoverCarriesTheAttestationAReplicaServedOn(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	require.Equal(t, "pve2", w.targetOf(t, "pve3", "db.example.com").Attestation.Node)

	w.takeOver(t, func() { w.acts["pve2"].silent = true })

	tg, listed := w.dbOnPve3()
	require.True(t, listed)
	require.NotNil(t, tg.Attestation, "carried while pve2 has not reported")
	require.Equal(t, dbAttested("pve2"), *tg.Attestation)
	require.Equal(t, dbAddr, tg.Addr)
	require.Equal(t, []string{dbCard}, tg.MACs)
	require.True(t, tg.Covered)
	require.False(t, tg.Resolve)

	t.Run("until coverage is judged, which pins the route to its node", func(t *testing.T) {
		for range 8 {
			w.tick(10 * time.Second)
		}
		tg, listed := w.dbOnPve3()
		require.False(t, listed && tg.Attestation != nil)
	})
}

// Once the hosting node reports to the new controller, its own word
// decides: an attestation it does not give is not carried.
func TestATakeoverCarriesNothingOnceTheHostingNodeReports(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.takeOver(t, func() { w.acts["pve2"].silent = true })
	tg, _ := w.dbOnPve3()
	require.NotNil(t, tg.Attestation)

	w.acts["pve2"].silent = false
	w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations = nil }
	w.tick(time.Second)
	w.tick(time.Second)

	require.Nil(t, w.attestationOn(t, "pve3", "db.example.com"))
	require.Equal(t, "pve3", w.attestationOn(t, "pve2", "app.example.com").Node, "a fresh attestation goes as before")
}

// What a takeover carries is only what nothing contradicts.
func TestATakeoverCarriesNoAttestationAgainstTheEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during func(w *world)
	}{
		{"the hosting node reports without the attestation", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations = nil }
		}},
		{"the hosting node is down", func(w *world) {
			w.acts["pve2"].silent = true
			*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2}, {Name: "pve3", ID: 3, Online: true}}
		}},
		{"another node attests the card", func(w *world) {
			w.acts["pve2"].silent = true
			w.acts["pve3"].edit = func(st *agentproto.Status) { st.Attestations = append(st.Attestations, dbAttested("pve3")) }
		}},
		{"the replica did not reach the target", func(w *world) {
			w.acts["pve2"].silent = true
			w.acts["pve3"].fail = map[string]string{"db.example.com": "identity failed"}
		}},
		{"the replica reports an attestation of another node", func(w *world) {
			w.acts["pve2"].silent = true
			w.acts["pve3"].edit = func(st *agentproto.Status) {
				for i := range st.Targets {
					if st.Targets[i].Attestation != nil {
						at := dbAttested("pve1")
						st.Targets[i].Attestation = &at
					}
				}
			}
		}},
		{"the replica reports an attestation of another card", func(w *world) {
			w.acts["pve2"].silent = true
			w.acts["pve3"].edit = func(st *agentproto.Status) {
				for i := range st.Targets {
					if st.Targets[i].Attestation != nil {
						st.Targets[i].Attestation.MAC = "bc:24:11:99:99:99"
					}
				}
			}
		}},
		{"the address is blocked", func(w *world) {
			w.acts["pve2"].silent = true
			w.acts["pve3"].edit = dbConflict
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := threeNodes(t)
			w.settle()

			w.takeOver(t, func() { tc.during(w) })

			require.Nil(t, w.attestationOn(t, "pve3", "db.example.com"))
		})
	}
}

// A replica that reports an address the hosting node never chose gets
// nothing back for it.
func TestATakeoverCarriesNoAddressTheReplicaMadeUp(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.takeOver(t, func() {
		w.acts["pve2"].silent = true
		w.acts["pve3"].addrs = map[string]netip.Addr{"db.example.com": netip.MustParseAddr("10.0.0.99")}
	})

	tg, _ := w.dbOnPve3()
	require.NotEqual(t, netip.MustParseAddr("10.0.0.99"), tg.Addr)
}
