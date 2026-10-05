package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// tagged is guest web-1, publishing www.example.com, with a NIC of another
// MAC first and the NIC the resolver binds on VLAN 20.
func tagged() model.Guest {
	g := guest(101, "web-1", "www.example.com -> :8080")
	g.NICs = []model.NIC{
		{Index: 0, MAC: "bc:24:11:00:00:09", Bridge: "vmbr1"},
		{Index: 1, MAC: testMAC, Bridge: "vmbr0", VLAN: 20},
	}
	return g
}

func TestThePathOfTheWinner(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(tagged()))
	e.res.place("vmbr0", map[string]string{testMAC: "tap101i1"})

	st := e.cycle()

	require.Equal(t, &PathView{Node: testNode, Bridge: "vmbr0", VLAN: 20, Port: "tap101i1", MAC: testMAC, VerifiedAt: t0},
		route(st, "www.example.com").Path)
}

func TestAProofThatPlacedTheMACNowhereHasAPathWithoutABridge(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(tagged()))

	st := e.cycle()

	require.Equal(t, &PathView{Node: testNode, VLAN: 20, MAC: testMAC, VerifiedAt: t0}, route(st, "www.example.com").Path)
}

func TestOnlyABoundWinnerHasAPath(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(tagged(), guest(102, "web-2", "www.example.com -> :8080")))
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "nas.example.com", ManualID: "nas", Source: model.SourceManual,
		Target: model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.0.50"), Port: 5000},
	}))

	st := e.cycle()

	for _, r := range st.Routes {
		switch r.Owner {
		case "qemu/101":
			require.NotNil(t, r.Path, "the winner")
		case "qemu/102":
			require.Equal(t, planner.StateConflict, r.State)
			require.Nil(t, r.Path, "the loser")
		case "manual/nas":
			require.Equal(t, planner.StateActive, r.State)
			require.Nil(t, r.Path, "a manual route is not bound")
		default:
			t.Fatalf("unexpected route %s of %s", r.Hostname, r.Owner)
		}
	}
}

// A manual route that names a guest has the guest's address proven, and
// bound: it has a path like the route of a guest.
func TestAManualRouteThatNamesAGuestHasAPath(t *testing.T) {
	e := newEnv(t)
	g := tagged()
	g.Description = ""
	g.NICs[1].Bridge, g.NICs[1].VLAN = "vmbr1", 30
	e.inv.set(snapshot(g))
	e.res.place("vmbr1", map[string]string{testMAC: "tap101i1"})
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "web.example.com", ManualID: "web", Source: model.SourceManual, Guest: &g.Ref,
		Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080},
	}))

	st := e.cycle()

	web := route(st, "web.example.com")
	require.Equal(t, "manual/web", web.Owner)
	require.Equal(t, &PathView{Node: testNode, Bridge: "vmbr1", VLAN: 30, Port: "tap101i1", MAC: testMAC, VerifiedAt: t0}, web.Path)
}

// A binding withdrawn when its guest stopped keeps where and when its last
// proof was made.
func TestAWithdrawnBindingKeepsThePathOfItsLastProof(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(tagged()))
	e.res.place("vmbr0", map[string]string{testMAC: "tap101i1"})
	proven := route(e.cycle(), "www.example.com").Path
	require.Equal(t, &PathView{Node: testNode, Bridge: "vmbr0", VLAN: 20, Port: "tap101i1", MAC: testMAC, VerifiedAt: t0}, proven)

	e.res.stop("www.example.com", "the guest is stopped")
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	r := route(st, "www.example.com")
	require.Equal(t, planner.StateWithdrawn, r.State)
	require.Equal(t, proven, r.Path)
}

func TestThePathOfABinding(t *testing.T) {
	since := t0.Add(-48 * time.Hour)
	b := resolve.Binding{
		Owner: "qemu/101", Hostname: "www.example.com", Guest: "qemu/101", Addr: guestAddr, MAC: testMAC,
		VerifiedAt: t0, Since: since, Bridge: "vmbr0", Port: "tap101i1",
	}
	snap := inventory.Snapshot{Guests: []model.Guest{tagged()}}
	untaggedNIC := tagged()
	untaggedNIC.NICs[1].VLAN = 0
	otherMAC := tagged()
	otherMAC.NICs[1].MAC = "bc:24:11:00:00:0a"

	for _, tt := range []struct {
		name  string
		owner string
		snap  inventory.Snapshot
		want  *PathView
	}{
		{"a tagged NIC", "qemu/101", snap,
			&PathView{Node: testNode, Bridge: "vmbr0", VLAN: 20, Port: "tap101i1", MAC: testMAC, VerifiedAt: t0, Since: since}},
		{"an untagged NIC", "qemu/101", inventory.Snapshot{Guests: []model.Guest{untaggedNIC}},
			&PathView{Node: testNode, Bridge: "vmbr0", Port: "tap101i1", MAC: testMAC, VerifiedAt: t0, Since: since}},
		{"no NIC with the MAC", "qemu/101", inventory.Snapshot{Guests: []model.Guest{otherMAC}},
			&PathView{Node: testNode, Bridge: "vmbr0", Port: "tap101i1", MAC: testMAC, VerifiedAt: t0, Since: since}},
		{"a guest the snapshot does not have", "qemu/101", inventory.Snapshot{},
			&PathView{Node: testNode, Bridge: "vmbr0", Port: "tap101i1", MAC: testMAC, VerifiedAt: t0, Since: since}},
		{"a binding of another owner", "qemu/102", snap, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, pathOf(testNode, tt.owner, &b, tt.snap))
		})
	}
	require.Nil(t, pathOf(testNode, "qemu/101", nil, snap), "no binding")
}

func TestACloneOfTheStateOwnsItsPaths(t *testing.T) {
	st := populatedState()
	c := st.clone()

	c.Routes[0].Path.VLAN = 30

	require.Equal(t, populatedState(), st)
}
