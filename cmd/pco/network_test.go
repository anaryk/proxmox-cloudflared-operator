package main

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// someNetworks are vmbr1, its leg up and serving www.example.com, and VLAN
// 20 of vmbr2, attached over a route of the node on a bridge with an uplink,
// its leg refused by duplicate-address detection.
func someNetworks() []engine.NetworkDetail {
	return []engine.NetworkDetail{
		{
			NetworkView: engine.NetworkView{
				Network: store.Network{
					ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
					Legs: map[string]netip.Addr{"pve1": netip.MustParseAddr("10.77.0.250")}, LegMAC: "02:50:43:4f:00:01", LegMACs: map[string]string{"pve1": "02:50:43:4f:00:01"}, Mark: 1,
					CreatedAt: t0.Add(-time.Hour), Rev: 2,
				},
				Legs:    []engine.LegView{{Node: "pve1", Addr: netip.MustParseAddr("10.77.0.250"), Up: true}},
				Targets: 1, Routes: 1,
			},
			Services: []store.ServiceAddr{{
				Network: "vmbr1", Addr: netip.MustParseAddr("10.77.0.11"), Service: netip.MustParseAddr("198.18.1.0"), AllocatedAt: t0.Add(-30 * time.Minute),
			}},
			Served: []string{"www.example.com"},
		},
		{
			NetworkView: engine.NetworkView{
				Network: store.Network{
					ID: "vmbr2.20", Kind: store.NetworkAttach, Bridge: "vmbr2", VLAN: 20, Prefix: netip.MustParsePrefix("192.168.4.0/24"),
					Legs: map[string]netip.Addr{"pve1": netip.MustParseAddr("192.168.4.249")}, LegMAC: "02:50:43:4f:00:02", LegMACs: map[string]string{"pve1": "02:50:43:4f:00:02"}, Mark: 2,
					Uplink: true, OverlapOK: true, CreatedAt: t0.Add(-time.Minute), Rev: 1,
				},
				Legs: []engine.LegView{{Node: "pve1", Addr: netip.MustParseAddr("192.168.4.249"), Problem: "address in use by 52:54:00:12:34:56"}},
			},
			Services: []store.ServiceAddr{},
			Served:   []string{},
		},
	}
}

func someCandidates() []engine.CandidateNetwork {
	web := engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"}
	return []engine.CandidateNetwork{
		{Bridge: "lab", Vnet: true, Uplink: true, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{}},
		{Bridge: "pconet", Vnet: true, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{}, Refused: "pconet is pco's managed network, which is not attached"},
		{Bridge: "pcot1", Guests: []engine.GuestAddr{{Guest: web, Addr: netip.MustParseAddr("10.50.0.11")}}, Prefixes: []netip.Prefix{netip.MustParsePrefix("10.50.0.0/24")}},
		{Bridge: "vmbr0", Uplink: true, Direct: true, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{}},
		{Bridge: "vmbr1", Attached: "vmbr1", Guests: []engine.GuestAddr{{Guest: web, Addr: netip.MustParseAddr("10.77.0.11")}},
			Prefixes: []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")}},
		{Bridge: "vmbr2", VLAN: 20, VLANAware: true, Uplink: true, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{}},
	}
}

// freeProbe is what the daemon answers a probe of vmbr1 with: the address it
// proposes is free.
func freeProbe() engine.ProbeResult {
	return engine.ProbeResult{
		Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.254"), Free: true, AnsweredBy: []string{},
		Note: "a /24 is assumed: only 10.77.0.11 is known on vmbr1; 10.77.0.254 is the highest address of 10.77.0.0/24 that no guest is known to have",
	}
}

func daemonWithNetworks(t *testing.T) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: healthyState(), networks: someNetworks(), candidates: someCandidates(), probe: freeProbe()}
	return newRunner(t, serveFake(t, e)), e
}

func TestNetworkListGolden(t *testing.T) {
	r, _ := daemonWithNetworks(t)

	res := r.run("", "network", "list")

	require.NoError(t, res.err)
	requireGolden(t, "network_list.golden", res.out)

	t.Run("none", func(t *testing.T) {
		r, _ := daemonWith(t, healthyState())

		res := r.run("", "network", "list")

		require.NoError(t, res.err)
		require.Equal(t, "No network is attached; pco network candidates lists the bridges and VLANs one may be attached to.\n", res.out)
	})
}

func TestNetworkListJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"id":"vmbr1","kind":"attach","legStates":[],"routes":0}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/networks": {200, raw}}))

	res := r.run("", "--json", "network", "list")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

func TestNetworkCandidatesGolden(t *testing.T) {
	r, _ := daemonWithNetworks(t)

	res := r.run("", "network", "candidates")

	require.NoError(t, res.err)
	requireGolden(t, "network_candidates.golden", res.out)
}

func TestNetworkProbe(t *testing.T) {
	t.Run("a free address it proposes", func(t *testing.T) {
		r, e := daemonWithNetworks(t)

		res := r.run("", "network", "probe", "vmbr1")

		require.NoError(t, res.err)
		require.Equal(t, "10.77.0.254 on vmbr1, in 10.77.0.0/24, is free: nothing answered the probe.\n"+
			"  a /24 is assumed: only 10.77.0.11 is known on vmbr1; 10.77.0.254 is the highest address of 10.77.0.0/24 that no guest is known to have\n",
			res.out)
		require.Equal(t, []string{"probe vmbr1:0 prefix=invalid Prefix address=invalid IP"}, e.called())
	})
	t.Run("an address with its length, in use", func(t *testing.T) {
		r, e := daemonWithNetworks(t)
		e.probe = engine.ProbeResult{
			Free: false, AnsweredBy: []string{"52:54:00:12:34:56"}, Uplink: true,
			Warning: "the leg's MAC becomes visible on the LAN of vmbr2", Overlap: "10.20.0.0/24 overlaps the node's route on vmbr9",
		}

		res := r.run("", "network", "probe", "vmbr2:20", "--address", "10.20.0.250/24")

		require.ErrorIs(t, res.err, errReported, "something answered: exit 1")
		require.Equal(t, "10.20.0.250 on vmbr2 VLAN 20, in 10.20.0.0/24, is in use: 52:54:00:12:34:56 answered the probe.\n"+
			"Uplink: vmbr2 VLAN 20 has a physical uplink: the leg's MAC becomes visible on the LAN of vmbr2.\n"+
			"Overlap: 10.20.0.0/24 overlaps the node's route on vmbr9.\n", res.out)
		require.Equal(t, []string{"probe vmbr2:20 prefix=10.20.0.0/24 address=10.20.0.250"}, e.called())
	})
	t.Run("a prefix of its own", func(t *testing.T) {
		r, e := daemonWithNetworks(t)

		res := r.run("", "network", "probe", "vmbr1", "--prefix", "10.77.0.0/23", "--address", "10.77.1.5/24")

		require.NoError(t, res.err)
		require.Equal(t, []string{"probe vmbr1:0 prefix=10.77.0.0/23 address=10.77.1.5"}, e.called(), "--prefix wins over the length of the address")
	})
	t.Run("as JSON", func(t *testing.T) {
		const raw = `{"bridge":"vmbr1","prefix":"10.77.0.0/24","address":"10.77.0.254","free":true,"answeredBy":[],"uplink":false}`
		r := newRunner(t, serveRaw(t, map[string]rawReply{"POST /v1/networks/probe": {200, raw}}))

		res := r.run("", "--json", "network", "probe", "vmbr1")

		require.NoError(t, res.err)
		require.Equal(t, indented(t, raw), res.out)
	})
	t.Run("what it does not take", func(t *testing.T) {
		r, e := daemonWithNetworks(t)
		for _, args := range [][]string{
			{"vmbr1", "--address", "fd00::1"},
			{"vmbr1", "--address", "10.77.0"},
			{"vmbr1", "--prefix", "10.77.0.5/24"},
			{"vmbr1:x"},
		} {
			res := r.run("", append([]string{"network", "probe"}, args...)...)

			require.Error(t, res.err, args)
		}
		require.Empty(t, e.called())
	})
}

func TestNetworkAttachProbesShowsAndAsks(t *testing.T) {
	const shown = "10.77.0.250 on pcot1, in 10.77.0.0/24, is free: nothing answered the probe.\n"
	for _, tt := range []struct {
		name     string
		in       string
		args     []string
		attached bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithNetworks(t)
			e.probe.Note = ""

			res := r.tty().run(tt.in, append([]string{"network", "attach", "pcot1", "--address", "10.77.0.250/24"}, tt.args...)...)

			probe := "probe pcot1:0 prefix=10.77.0.0/24 address=10.77.0.250"
			if !tt.attached {
				require.ErrorIs(t, res.err, errAborted)
				require.Equal(t, shown, res.out)
				require.Equal(t, []string{probe}, e.called())
				return
			}
			require.NoError(t, res.err)
			require.Equal(t, []string{probe, "attach pcot1:0 prefix=10.77.0.0/24 address=10.77.0.250 confirmUplink=false overlapOK=false"}, e.called())
			require.Equal(t, shown+"Attached network pcot1: the leg leg1 with 10.77.0.250/24, mark 1. "+
				"The next cycle raises it; pco network list shows how it is.\n", res.out)
			if len(tt.args) == 0 {
				require.Equal(t, "Attach network pcot1 with the leg 10.77.0.250/24? [y/N] ", res.errOut)
			}
		})
	}
}

func TestNetworkAttachOverARouteOfTheNode(t *testing.T) {
	overlapping := func(e *fakeEngine) {
		e.probe = engine.ProbeResult{Free: true, AnsweredBy: []string{}, Overlap: "192.168.4.0/24 overlaps the node's route on vmbr0"}
	}

	t.Run("refused without --overlap-ok", func(t *testing.T) {
		r, e := daemonWithNetworks(t)
		overlapping(e)

		res := r.tty().run("", "network", "attach", "pcot1", "--address", "192.168.4.249/24")

		require.EqualError(t, res.err, "192.168.4.0/24 overlaps the node's route on vmbr0; pass --overlap-ok if the guests there are meant to be reached through the leg")
		requireGolden(t, "network_attach_overlap.golden", res.out+"--- error\n"+res.err.Error()+"\n")
		require.Equal(t, []string{"probe pcot1:0 prefix=192.168.4.0/24 address=192.168.4.249"}, e.called(), "nothing is asked, nothing attached")
	})
	t.Run("accepted with it", func(t *testing.T) {
		r, e := daemonWithNetworks(t)
		overlapping(e)

		res := r.run("", "network", "attach", "pcot1", "--address", "192.168.4.249/24", "--overlap-ok", "--yes")

		require.NoError(t, res.err)
		require.Contains(t, e.called(), "attach pcot1:0 prefix=192.168.4.0/24 address=192.168.4.249 confirmUplink=false overlapOK=true")
	})
	t.Run("the refusal of the daemon", func(t *testing.T) {
		r, e := daemonWithNetworks(t)
		e.networkErr = &engine.FieldError{Field: "overlapOK",
			Err: fmt.Errorf("10.50.0.0/24 overlaps the node's route on vmbr9; %s", engine.OverlapHint)}

		res := r.run("", "network", "attach", "pcot1", "--address", "10.50.0.249/24", "--yes")

		require.ErrorIs(t, res.err, engine.ErrInvalid)
		require.EqualError(t, res.err, "10.50.0.0/24 overlaps the node's route on vmbr9; pass --overlap-ok if the guests there are meant to be reached through the leg")
	})
}

func TestNetworkAttachToABridgeWithAnUplink(t *testing.T) {
	uplinked := func(e *fakeEngine) {
		e.probe = engine.ProbeResult{Free: true, AnsweredBy: []string{}, Uplink: true, Warning: "the leg's MAC becomes visible on the LAN of vmbr2"}
	}
	r, e := daemonWithNetworks(t)
	uplinked(e)

	res := r.tty().run("", "network", "attach", "vmbr2:30", "--address", "10.30.0.250/24")

	require.EqualError(t, res.err, "vmbr2 VLAN 30 has a physical uplink: the leg's MAC becomes visible on the LAN of vmbr2; "+
		"confirm it with --confirm-uplink, with an address reserved for pco on that LAN")
	require.Len(t, e.called(), 1, "nothing is asked, nothing attached")

	r, e = daemonWithNetworks(t)
	uplinked(e)
	res = r.run("", "network", "attach", "vmbr2:30", "--address", "10.30.0.250/24", "--confirm-uplink", "--yes")
	require.NoError(t, res.err)
	require.Contains(t, e.called(), "attach vmbr2:30 prefix=10.30.0.0/24 address=10.30.0.250 confirmUplink=true overlapOK=false")
}

func TestNetworkAttachOfAnAddressInUse(t *testing.T) {
	r, e := daemonWithNetworks(t)
	e.probe = engine.ProbeResult{AnsweredBy: []string{"52:54:00:12:34:56"}}

	res := r.tty().run("", "network", "attach", "pcot1", "--address", "10.50.0.11/24")

	require.EqualError(t, res.err, "10.50.0.11 is in use on pcot1; choose another address, which pco network probe pcot1 proposes")
	require.Len(t, e.called(), 1)
}

func TestNetworkAttachNeedsTheAddressWithItsLength(t *testing.T) {
	r, e := daemonWithNetworks(t)

	res := r.run("", "network", "attach", "pcot1", "--address", "10.50.0.249")
	require.EqualError(t, res.err, `--address "10.50.0.249": want the address of the leg with the length of the guests' prefix, as 10.77.0.250/24`)
	res = r.run("", "network", "attach", "pcot1")
	require.ErrorContains(t, res.err, `required flag(s) "address" not set`)
	res = r.run("", "--json", "network", "attach", "pcot1", "--address", "10.50.0.249/24")
	require.ErrorContains(t, res.err, "--json has no meaning")

	require.Empty(t, e.called())
}

func TestTheHelpOfAttachSaysWhatTheFlagsAreFor(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")

	res := r.run("", "network", "attach", "--help")

	require.NoError(t, res.err)
	help := strings.Join(strings.Fields(res.out), " ")
	require.Contains(t, help, "--overlap-ok attach a prefix that overlaps a route of the node on another interface")
	require.Contains(t, help, "unless --overlap-ok says that the guests there are meant to be reached through the leg")
	require.Contains(t, help, "--confirm-uplink attach to a bridge with a physical uplink")
}

func TestNetworkDetachShowsTheRoutesAndAsks(t *testing.T) {
	const shown = "Network vmbr1: 10.77.0.0/24 on vmbr1, the leg at 10.77.0.250.\n" +
		"1 route is served through it:\n" +
		"  www.example.com\n" +
		"Detaching it removes the leg and releases 1 service address; the routes through it are no longer served that way.\n"
	for _, tt := range []struct {
		name     string
		in       string
		args     []string
		detached bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "n\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithNetworks(t)

			res := r.tty().run(tt.in, append([]string{"network", "detach", "vmbr1"}, tt.args...)...)

			if !tt.detached {
				require.ErrorIs(t, res.err, errAborted)
				require.Equal(t, shown, res.out)
				require.Empty(t, e.called())
				return
			}
			require.NoError(t, res.err)
			require.Equal(t, shown+"Detached network vmbr1.\n", res.out)
			require.Equal(t, []string{"detach vmbr1 rev=2 confirm=true"}, e.called(), "the routes were shown: the detach confirms them")
		})
	}
}

func TestNetworkDetachOfANetworkThatServesNothing(t *testing.T) {
	r, e := daemonWithNetworks(t)

	res := r.run("", "network", "detach", "vmbr2.20", "--yes")

	require.NoError(t, res.err)
	require.Equal(t, "Network vmbr2.20: 192.168.4.0/24 on vmbr2 VLAN 20, the leg at 192.168.4.249.\n"+
		"No route is served through it.\n"+
		"Detaching it removes the leg and releases 0 service addresses; the routes through it are no longer served that way.\n"+
		"Detached network vmbr2.20.\n", res.out)
	require.Equal(t, []string{"detach vmbr2.20 rev=1 confirm=false"}, e.called())

	res = r.run("", "network", "detach", "vmbr7", "--yes")
	require.ErrorIs(t, res.err, engine.ErrNotFound)
	require.EqualError(t, res.err, "not found: network vmbr7 is not attached")
}

func TestNetworkShowGolden(t *testing.T) {
	r, _ := daemonWithNetworks(t)

	up := r.run("", "network", "show", "vmbr1")
	overlap := r.run("", "network", "show", "vmbr2.20")

	require.NoError(t, up.err)
	require.NoError(t, overlap.err)
	requireGolden(t, "network_show.golden", up.out+"---\n"+overlap.out)
	require.Contains(t, overlap.out, "overlap-ok:")
	require.NotContains(t, up.out, "overlap-ok")
}

func TestNetworkShowJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `{"id":"vmbr1","kind":"attach","services":[],"served":[]}`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/networks/vmbr1": {200, raw}}))

	res := r.run("", "--json", "network", "show", "vmbr1")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}
