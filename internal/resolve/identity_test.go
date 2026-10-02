package resolve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestResolveUntaggedBridge(t *testing.T) {
	s := newScenario(t)

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"}}, res.Candidates)
	require.Equal(t, []probeCall{
		{op: "interfaces"},
		{op: "route", addr: ip("10.20.0.10")},
		{op: "arp", iface: "vmbr0", addr: ip("10.20.0.10")},
		{op: "fdb", iface: "vmbr0", mac: mac0},
		{op: "dial", addr: ip("10.20.0.10"), port: 80},
	}, s.prober.calls)
}

func TestResolveExplicitAddress(t *testing.T) {
	s := newScenario(t)

	res := s.resolve(t, routeFor(ip("10.20.0.10"), ""), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromVia, OK: true, Level: "port"}}, res.Candidates)
	require.Len(t, s.prober.ops("arp"), 1)
	require.Len(t, s.prober.ops("fdb"), 1)
}

func TestResolveVLANInterface(t *testing.T) {
	tests := []struct {
		name      string
		iface     string
		fdbBridge string
		fdbVLAN   int
	}{
		{"vlan interface on a vlan-aware bridge", "vmbr0.10", "vmbr0", 10},
		{"bridge of the vlan", "vmbr0v10", "vmbr0v10", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
			s.prober.ifaces = []HostIface{
				{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")},
				{Name: tt.iface, Addrs: prefixes("10.30.0.2/24")},
			}
			s.prober.arp[arpKey(tt.iface, "10.30.0.10")] = []string{mac0}
			s.prober.fdb[fdbKey(tt.fdbBridge, tt.fdbVLAN, mac0)] = []string{"tap101i0"}

			res := s.resolve(t, webRoute(), nil)

			requireServed(t, res, "10.30.0.10", t0)
			require.Equal(t, []probeCall{{op: "arp", iface: tt.iface, addr: ip("10.30.0.10")}}, s.prober.ops("arp"))
			require.Equal(t, []probeCall{{op: "fdb", iface: tt.fdbBridge, vlan: tt.fdbVLAN, mac: mac0}}, s.prober.ops("fdb"))
		})
	}
}

func TestResolveNotAdjacent(t *testing.T) {
	tests := []struct {
		name   string
		vlan   int
		ifaces []HostIface
		reason string
	}{
		{
			name:   "no interface for the bridge",
			ifaces: []HostIface{{Name: "vmbr1", Addrs: prefixes("10.20.0.2/24")}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "bridge in another network",
			ifaces: []HostIface{{Name: "vmbr0", Addrs: prefixes("10.99.0.2/24")}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "bridge without addresses",
			ifaces: []HostIface{{Name: "vmbr0"}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "untagged nic and only a vlan interface in the network",
			ifaces: []HostIface{{Name: "vmbr0.10", Addrs: prefixes("10.20.0.2/24")}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "tagged nic and only the untagged bridge in the network",
			vlan:   10,
			ifaces: []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")}},
			reason: "node has no address on vmbr0.10 or vmbr0v10 in the guest's network",
		},
		{
			name: "tagged nic and only another vlan in the network",
			vlan: 10,
			ifaces: []HostIface{
				{Name: "vmbr0.20", Addrs: prefixes("10.20.0.2/24")},
				{Name: "vmbr0v20", Addrs: prefixes("10.20.0.3/24")},
			},
			reason: "node has no address on vmbr0.10 or vmbr0v10 in the guest's network",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().NICs[0].VLAN = tt.vlan
			s.prober.ifaces = tt.ifaces

			res := s.resolve(t, webRoute(), nil)

			requireNotServed(t, res, tt.reason)
			require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: tt.reason}}, res.Candidates)
			require.False(t, s.prober.touched(ip("10.20.0.10")))
		})
	}
}

func TestResolveTrustedStatic(t *testing.T) {
	const notAdjacent = "node has no address on vmbr0 in the guest's network"
	tests := []struct {
		name   string
		trust  bool
		nic    model.NIC
		rep    []model.ReportedAddr
		route  model.Route
		kernel *fakeRoute // the kernel's route to addr when not the default one
		addr   string
		source CandidateSource
		reason string // empty when accepted
	}{
		{
			name:   "static inside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			addr:   "10.40.0.10",
			source: FromStatic,
		},
		{
			name:   "static through a gateway on another interface",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			kernel: &fakeRoute{iface: "vmbr7"},
			addr:   "10.40.0.10",
			source: FromStatic,
		},
		{
			name:   "static on-link through another interface",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			kernel: &fakeRoute{iface: "vmbr7", onLink: true},
			addr:   "10.40.0.10",
			source: FromStatic,
			reason: "address is on-link through vmbr7, which is not the guest's bridge",
		},
		{
			name:   "static on-link on the guest's bridge without an address there",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			kernel: &fakeRoute{iface: "vmbr0", onLink: true},
			addr:   "10.40.0.10",
			source: FromStatic,
			reason: notAdjacent,
		},
		{
			name:   "static whose route changes with the source address",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			kernel: &fakeRoute{err: fmt.Errorf("route to 10.40.0.10 from 10.20.0.2: %w", ErrRouteDiffers)},
			addr:   "10.40.0.10",
			source: FromStatic,
			reason: "route to 10.40.0.10 changes with the source address",
		},
		{
			name:   "static the kernel has no route to",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			kernel: &fakeRoute{err: fmt.Errorf("route to 10.40.0.10: %w", ErrNoRoute)},
			addr:   "10.40.0.10",
			source: FromStatic,
			reason: "node has no route to 10.40.0.10",
		},
		{
			name:   "static outside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.50.0.10"),
			route:  webRoute(),
			addr:   "10.50.0.10",
			source: FromStatic,
			reason: notAdjacent,
		},
		{
			name:   "trust switched off",
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			addr:   "10.40.0.10",
			source: FromStatic,
			reason: notAdjacent,
		},
		{
			name:   "reported address inside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0),
			rep:    []model.ReportedAddr{rep(mac0, "10.40.0.11")},
			route:  webRoute(),
			addr:   "10.40.0.11",
			source: FromAgent,
			reason: notAdjacent,
		},
		{
			name:   "static address named by the route",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(ip("10.40.0.10"), ""),
			addr:   "10.40.0.10",
			source: FromVia,
		},
		{
			name:   "static address named by via",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(netip.Addr{}, "10.40.0.10"),
			addr:   "10.40.0.10",
			source: FromVia,
		},
		{
			name:   "address named by the route that is not static",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(ip("10.40.0.12"), ""),
			addr:   "10.40.0.12",
			source: FromVia,
			reason: notAdjacent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.settings.TrustStatic = tt.trust
			s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")
			s.web().NICs = []model.NIC{tt.nic}
			s.web().Reported = tt.rep
			if tt.kernel != nil {
				s.prober.routes[tt.addr] = *tt.kernel
			}

			res := s.resolve(t, tt.route, nil)

			require.Empty(t, s.prober.ops("arp"))
			require.Empty(t, s.prober.ops("fdb"))
			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.Equal(t, []CandidateResult{{Addr: ip(tt.addr), Source: tt.source, Reason: tt.reason}}, res.Candidates)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			requireServedAt(t, res, tt.addr, t0, LevelObserved)
			require.Equal(t, []probeCall{{op: "route", addr: ip(tt.addr)}}, s.prober.ops("route"))
			require.Equal(t, []probeCall{{op: "dial", addr: ip(tt.addr), port: 80}}, s.prober.ops("dial"))
		})
	}
}

func TestResolveRejectsSecondMACAnswering(t *testing.T) {
	s := newScenario(t)
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, foreignMAC}

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest")
	require.Empty(t, s.prober.ops("fdb"))
	require.Empty(t, s.prober.ops("dial"))
}

func TestResolveARPAnswers(t *testing.T) {
	tests := []struct {
		name    string
		nics    []model.NIC
		answers []string
		fdb     map[string]string
		reason  string // empty when accepted
	}{
		{
			name:    "foreign MAC",
			answers: []string{foreignMAC},
			reason:  "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		},
		{
			name:    "no answer",
			answers: nil,
			reason:  "no ARP answer on vmbr0",
		},
		{
			name:    "own MAC in another spelling",
			answers: []string{"BC-24-11-00-00-01"},
		},
		{
			name:    "unparsable answer",
			answers: []string{"garbage"},
			reason:  "10.20.0.10 answered by garbage, which is not this guest",
		},
		{
			name: "two NICs of the guest on the bridge",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr0", 0),
			},
			answers: []string{mac1, mac0},
			fdb: map[string]string{
				fdbKey("vmbr0", 0, mac0): "tap101i0",
				fdbKey("vmbr0", 0, mac1): "tap101i1",
			},
		},
		{
			name: "second NIC of the guest on the port of the first",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr0", 0),
			},
			answers: []string{mac0, mac1},
			fdb: map[string]string{
				fdbKey("vmbr0", 0, mac0): "tap101i0",
				fdbKey("vmbr0", 0, mac1): "tap101i0",
			},
			reason: "MAC bc:24:11:00:00:02 is on port tap101i0, not on the guest's own port",
		},
		{
			name: "NIC of the guest on another bridge",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr1", 0),
			},
			answers: []string{mac1},
			reason:  "10.20.0.10 answered by bc:24:11:00:00:02, which is not this guest",
		},
		{
			name: "NIC of the guest in another vlan of the bridge",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr0", 10),
			},
			answers: []string{mac0, mac1},
			reason:  "10.20.0.10 answered by bc:24:11:00:00:02, which is not this guest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			if tt.nics != nil {
				s.web().NICs = tt.nics
			}
			s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = tt.answers
			for k, v := range tt.fdb {
				s.prober.fdb[k] = []string{v}
			}

			res := s.resolve(t, webRoute(), nil)

			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			requireServed(t, res, "10.20.0.10", t0)
		})
	}
}

func TestResolveForwardingTable(t *testing.T) {
	tests := []struct {
		name   string
		ref    model.GuestRef
		ports  []string // nil: MAC not learned
		reason string   // empty when accepted
	}{
		{name: "tap of the guest", ref: web1Ref, ports: []string{"tap101i0"}},
		{name: "firewall bridge of the guest", ref: web1Ref, ports: []string{"fwpr101p0"}},
		{name: "veth of the container", ref: ct200Ref, ports: []string{"veth200i0"}},
		{
			name: "tap of another guest", ref: web1Ref, ports: []string{"tap102i0"},
			reason: "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
		},
		{
			name: "tap of another NIC of the guest", ref: web1Ref, ports: []string{"tap101i1"},
			reason: "MAC bc:24:11:00:00:01 is on port tap101i1, not on the guest's own port",
		},
		{
			name: "tap of a guest whose vmid starts the same", ref: web1Ref, ports: []string{"tap1010i0"},
			reason: "MAC bc:24:11:00:00:01 is on port tap1010i0, not on the guest's own port",
		},
		{
			name: "uplink", ref: web1Ref, ports: []string{"eno1"},
			reason: "MAC bc:24:11:00:00:01 is on port eno1, not on the guest's own port",
		},
		{
			name: "not learned", ref: web1Ref,
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
		{
			name: "own port and the uplink", ref: web1Ref, ports: []string{"tap101i0", "eno1"},
			reason: "MAC bc:24:11:00:00:01 is on several ports: tap101i0, eno1",
		},
		{
			name: "own port and another guest's", ref: web1Ref, ports: []string{"tap102i0", "tap101i0"},
			reason: "MAC bc:24:11:00:00:01 is on several ports: tap102i0, tap101i0",
		},
		{
			name: "own tap and own firewall bridge", ref: web1Ref, ports: []string{"tap101i0", "fwpr101p0"},
			reason: "MAC bc:24:11:00:00:01 is on several ports: tap101i0, fwpr101p0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().Ref = tt.ref
			delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))
			if tt.ports != nil {
				s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = tt.ports
			}

			res := s.resolve(t, routeTo(tt.ref), nil)

			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			owner := tt.ref.String()
			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: owner}, res.Target)
			require.NotNil(t, res.Binding)
			require.Equal(t, owner, res.Binding.Owner)
			require.Equal(t, owner, res.Binding.Guest)
		})
	}
}

func TestResolveForwardingTableNode(t *testing.T) {
	tests := []struct {
		name      string
		guestNode string
		localNode string
		nodes     []string // nil: pve1 and pve2
		checked   bool
		reason    string // when checked; the usual one when empty
	}{
		{name: "guest on this node", guestNode: localNode, localNode: localNode, checked: true},
		{name: "guest on another node", guestNode: "pve2", localNode: localNode, checked: false},
		{name: "local node unknown", guestNode: "pve2", localNode: "", checked: true},
		{name: "guest node unknown", guestNode: "", localNode: localNode, checked: true},
		{name: "local node names no node of the cluster", guestNode: "pve2", localNode: "pve9", checked: true},
		{name: "local node in another spelling", guestNode: "pve2", localNode: "PVE1", checked: true},
		{
			name: "no nodes known", guestNode: "pve2", localNode: localNode, nodes: []string{}, checked: true,
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0 (no cluster nodes known)",
		},
		{name: "guest on a node that is not listed", guestNode: "pve7", localNode: localNode, checked: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().Node = tt.guestNode
			s.settings.LocalNode = tt.localNode
			if tt.nodes != nil {
				s.nodes = nil
				for _, n := range tt.nodes {
					s.nodes = append(s.nodes, inventory.Node{Name: n})
				}
			}
			delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))

			res := s.resolve(t, webRoute(), nil)

			require.Len(t, s.prober.ops("arp"), 1, "ARP always applies")
			require.Len(t, s.prober.ops("fdb"), 1, "the forwarding table is read either way")
			if tt.checked {
				requireNotServed(t, res, cmp.Or(tt.reason, "MAC bc:24:11:00:00:01 not seen on bridge vmbr0"))
				return
			}
			requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
		})
	}
}

func TestResolveGuestMovedToAnotherNode(t *testing.T) {
	t.Run("answering", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
		require.Len(t, s.prober.ops("arp"), 1)
		require.Len(t, s.prober.ops("fdb"), 1)
	})

	t.Run("not answering ARP", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "no ARP answer on vmbr0", withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("fdb"))
		require.Empty(t, s.prober.ops("dial"))
	})
}

func TestResolveDuplicateMAC(t *testing.T) {
	const dupMAC0 = "MAC bc:24:11:00:00:01 is also configured on qemu/102"

	t.Run("on a running guest", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, dupMAC0)
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("on a stopped guest", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(false, nicOn(0, mac0, "vmbr0", 0))

		res := s.resolve(t, webRoute(), nil)

		requireServed(t, res, "10.20.0.10", t0)
	})

	t.Run("on a guest whose state is unknown", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(false, nicOn(0, mac0, "vmbr1", 0))
		s.guests[1].StatusUnknown = true

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, dupMAC0)
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("in another spelling", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(true, nicOn(0, "BC:24:11:00:00:01", "vmbr1", 0))

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, dupMAC0)
	})

	t.Run("earlier binding wins on the guest's own port", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Len(t, s.prober.ops("fdb"), 1)
	})

	t.Run("earlier binding on another port", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(true, nicOn(0, mac0, "vmbr0", 0))
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap102i0"}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
			withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("earlier binding without the forwarding table", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.addDB1(true, nicOn(0, mac0, "vmbr0", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, dupMAC0, withdrawnAt(boundTo("10.20.0.10"), t0))
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("earlier binding on a trusted static address", func(t *testing.T) {
		s := newScenario(t)
		s.settings.TrustStatic = true
		s.settings.TrustedCIDRs = prefixes("10.20.0.0/24")
		s.prober.ifaces = []HostIface{{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")}}
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, dupMAC0, withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("dial"))
	})

	t.Run("binding of the other guest does not help", func(t *testing.T) {
		s := newScenario(t)
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))
		prev := boundTo("10.20.0.10")
		prev.Owner = db1Ref.String()

		res := s.resolve(t, webRoute(), prev)

		requireNotServed(t, res, dupMAC0)
	})

	t.Run("on another answering NIC of the guest", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
		s.addDB1(true, nicOn(0, mac1, "vmbr1", 0))
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"tap101i1"}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "MAC bc:24:11:00:00:02 is also configured on qemu/102", withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("dial"))
	})
}

func TestResolveBothGuestsBoundToOneMAC(t *testing.T) {
	setup := func(node string) (*scenario, model.Route, *Binding) {
		s := newScenario(t)
		s.web().Node = node
		s.addDB1(true, nicOn(0, mac0, "vmbr0", 0, "10.20.0.20"))
		s.guests[1].Node = node
		s.prober.arp[arpKey("vmbr0", "10.20.0.20")] = []string{mac0}
		dbRoute := routeTo(db1Ref)
		dbRoute.Hostname = "db.example.com"
		dbBound := &Binding{
			Owner: db1Ref.String(), Hostname: "db.example.com", Guest: db1Ref.String(),
			Addr: ip("10.20.0.20"), MAC: mac0, VerifiedAt: t0.Add(-time.Hour),
		}
		return s, dbRoute, dbBound
	}

	t.Run("on this node the forwarding table decides", func(t *testing.T) {
		s, dbRoute, dbBound := setup(localNode)

		web := s.resolve(t, webRoute(), boundTo("10.20.0.10"))
		db := s.resolve(t, dbRoute, dbBound)

		requireServed(t, web, "10.20.0.10", t0)
		require.Equal(t, planner.ResolvedTarget{
			Addr: ip("10.20.0.20"), Withdrawn: true, Owner: db1Ref.String(),
			Reason: "MAC bc:24:11:00:00:01 is on port tap101i0, not on the guest's own port",
		}, db.Target)
		require.Equal(t, withdrawnAt(dbBound, t0), db.Binding)
	})

	t.Run("on another node both are withdrawn", func(t *testing.T) {
		s, dbRoute, dbBound := setup("pve2")

		web := s.resolve(t, webRoute(), boundTo("10.20.0.10"))
		db := s.resolve(t, dbRoute, dbBound)

		requireWithdrawn(t, web, "MAC bc:24:11:00:00:01 is also configured on qemu/102", withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Equal(t, planner.ResolvedTarget{
			Addr: ip("10.20.0.20"), Withdrawn: true, Owner: db1Ref.String(),
			Reason: "MAC bc:24:11:00:00:01 is also configured on qemu/101",
		}, db.Target)
		require.Empty(t, s.prober.ops("arp"))
	})
}

func TestResolveTriesAtMostSixteenCandidates(t *testing.T) {
	s := newScenario(t)
	var static []string
	for i := range 20 {
		static = append(static, fmt.Sprintf("10.20.0.%d", 100+i))
	}
	s.web().NICs[0].Static = ips(static...)

	res := s.resolve(t, webRoute(), boundTo("10.20.0.119"))

	requireWithdrawn(t, res, "no ARP answer on vmbr0; 4 more candidates not tried", withdrawnAt(boundTo("10.20.0.119"), t0))
	require.Len(t, s.prober.ops("arp"), 16)
	require.Len(t, res.Candidates, 20)
	require.Equal(t, ip("10.20.0.119"), res.Candidates[0].Addr, "the bound address comes first")
	for i, c := range res.Candidates {
		if i < 16 {
			require.Equal(t, "no ARP answer on vmbr0", c.Reason, "candidate %d", i)
			continue
		}
		require.Equal(t, "not tried: too many candidates", c.Reason, "candidate %d", i)
		require.False(t, s.prober.touched(c.Addr))
	}
}

func TestResolveCapIsReported(t *testing.T) {
	s := newScenario(t)
	var static []string
	for i := range 17 {
		static = append(static, fmt.Sprintf("10.20.0.%d", 100+i))
	}
	s.web().NICs[0].Static = ips(static...)

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "no ARP answer on vmbr0; 1 more candidate not tried")
	require.Len(t, s.prober.ops("arp"), 16)
}

func TestResolveDeniedAddressNeverProbed(t *testing.T) {
	node := ip("10.20.0.2")

	t.Run("only candidate", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.2")
		s.prober.arp[arpKey("vmbr0", "10.20.0.2")] = []string{mac0}

		res := s.resolve(t, webRoute(), nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Nil(t, res.Binding)
		require.Equal(t, []CandidateResult{{Addr: node, Source: FromStatic, Reason: "address of a cluster node"}}, res.Candidates)
		require.Empty(t, s.prober.calls)
	})

	t.Run("named by the route", func(t *testing.T) {
		s := newScenario(t)

		res := s.resolve(t, routeFor(node, ""), nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Empty(t, s.prober.calls)
	})

	t.Run("before a good candidate", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.2")
		s.web().Reported = []model.ReportedAddr{rep(mac0, "169.254.10.10"), rep(mac0, "10.20.0.10"), rep(mac0, "10.20.0.11")}
		s.prober.arp[arpKey("vmbr0", "169.254.10.10")] = []string{mac0}

		res := s.resolve(t, webRoute(), nil)

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{
			{Addr: node, Source: FromStatic, Reason: "address of a cluster node"},
			{Addr: ip("169.254.10.10"), Source: FromAgent, Reason: "link-local address"},
			{Addr: ip("10.20.0.10"), Source: FromAgent, OK: true, Level: "port"},
			{Addr: ip("10.20.0.11"), Source: FromAgent, Reason: "not tried"},
		}, res.Candidates)
		require.False(t, s.prober.touched(node))
		require.False(t, s.prober.touched(ip("169.254.10.10")))
		require.False(t, s.prober.touched(ip("10.20.0.11")))
	})

	t.Run("bound address became denied", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.2", "10.20.0.10")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.2"))

		requireServed(t, res, "10.20.0.10", t0)
		require.False(t, s.prober.touched(node))
	})

	t.Run("bound address became denied and nothing else verifies", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.2")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.2"))

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Nil(t, res.Binding)
		require.Empty(t, s.prober.calls)
	})
}

func TestResolveNodeAddress(t *testing.T) {
	const reason = "address of this node"
	tests := []struct {
		name   string
		static []string
		prev   *Binding
		served string // empty when nothing is served
	}{
		{name: "address of the bridge", static: []string{"10.20.0.2"}},
		{name: "address of another interface", static: []string{"10.30.0.2"}},
		{name: "bound address became the node's", static: []string{"10.20.0.2"}, prev: boundTo("10.20.0.2")},
		{name: "before a good candidate", static: []string{"10.30.0.2", "10.20.0.10"}, served: "10.20.0.10"},
		{
			name: "bound address became the node's before a good candidate", static: []string{"10.20.0.2", "10.20.0.10"},
			prev: boundTo("10.20.0.2"), served: "10.20.0.10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.deny = denylist(t, nil, nil)
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})
			s.web().NICs[0].Static = ips(tt.static...)
			for _, a := range tt.static {
				s.prober.arp[arpKey("vmbr0", a)] = []string{mac0}
			}

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, CandidateResult{Addr: ip(tt.static[0]), Source: FromStatic, Reason: reason}, res.Candidates[0])
			require.False(t, s.prober.touched(ip(tt.static[0])))
			if tt.served != "" {
				requireServed(t, res, tt.served, t0)
				return
			}
			require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: reason}, res.Target)
			require.Nil(t, res.Binding)
		})
	}
}

func TestResolveCandidatesErrorRejects(t *testing.T) {
	s := newScenario(t)

	res := s.resolve(t, routeFor(netip.Addr{}, "net9"), boundTo("10.20.0.10"))

	require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "guest has no net9"}, res.Target)
	require.Nil(t, res.Binding)
	require.Empty(t, res.Candidates)
	require.Empty(t, s.prober.calls)
}

func TestResolveNoCandidates(t *testing.T) {
	s := newScenario(t)
	s.web().NICs[0].Static = nil

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "no candidate address")
	require.Empty(t, s.prober.calls)
}

func TestResolveDialFailureWithoutBinding(t *testing.T) {
	s := newScenario(t)
	s.prober.dialErr[ip("10.20.0.10")] = errDial

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "port 80: connection refused")
}

func TestResolveListsEveryCandidate(t *testing.T) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{
		nicOn(0, mac0, "vmbr0", 0, "10.20.0.12", "10.20.0.2"),
		nicOn(1, mac1, "vmbr1", 0, "10.30.0.10"),
	}
	s.web().Reported = []model.ReportedAddr{rep(mac0, "10.20.0.13")}
	s.prober.arp[arpKey("vmbr0", "10.20.0.13")] = []string{mac0}
	s.prober.dialErr[ip("10.20.0.13")] = errDial

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "no ARP answer on vmbr0")
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.12"), Source: FromStatic, Reason: "no ARP answer on vmbr0"},
		{Addr: ip("10.20.0.2"), Source: FromStatic, Reason: "address of a cluster node"},
		{Addr: ip("10.30.0.10"), Source: FromStatic, Reason: "node has no address on vmbr1 in the guest's network"},
		{Addr: ip("10.20.0.13"), Source: FromAgent, Reason: "port 80: connection refused", Level: "port"},
	}, res.Candidates)
	require.Len(t, s.prober.ops("interfaces"), 1, "interfaces are listed once per call")
}

func TestResolveAllowNodeIgnoredForGuests(t *testing.T) {
	tests := []struct {
		name   string
		static string
		reason string
	}{
		{"address of a cluster node", "10.20.0.2", "address of a cluster node"},
		{"address of a host interface", "10.30.0.2", "address of this node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})
			s.web().NICs[0].Static = ips(tt.static)
			route := webRoute()
			route.Options.AllowNode = true

			res := s.resolve(t, route, nil)

			require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: tt.reason}, res.Target)
			require.False(t, s.prober.touched(ip(tt.static)))
		})
	}
}

func TestResolveTooManyClaimants(t *testing.T) {
	const reason = "too many stations claim 10.20.0.10 on vmbr0"
	flood := func(s *scenario) {
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = fmt.Errorf("too many stations claim 10.20.0.10 on vmbr0: %w", ErrTooManyClaimants)
	}

	t.Run("bound address is withdrawn at once", func(t *testing.T) {
		s := newScenario(t)
		flood(s)

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, reason, withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("dial"))
	})

	t.Run("bound address gives way to a verified alternative", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
		flood(s)

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.11", t0)
	})

	t.Run("candidate without a binding", func(t *testing.T) {
		s := newScenario(t)
		flood(s)

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, reason)
	})
}

func TestResolveFollowsTheKernelRoute(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(s *scenario)
		addr   string
		reason string // empty when accepted
	}{
		{
			name: "route through the ARP interface",
			addr: "10.20.0.10",
		},
		{
			name:   "route through another interface",
			setup:  func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{iface: "vmbr1", onLink: true} },
			addr:   "10.20.0.10",
			reason: "route to 10.20.0.10 leaves through vmbr1, not vmbr0",
		},
		{
			name:   "route through a gateway on the ARP interface",
			setup:  func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{iface: "vmbr0"} },
			addr:   "10.20.0.10",
			reason: "route to 10.20.0.10 leaves through a gateway",
		},
		{
			name:   "route through a gateway on another interface",
			setup:  func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{iface: "eno1"} },
			addr:   "10.20.0.10",
			reason: "route to 10.20.0.10 leaves through eno1, not vmbr0",
		},
		{
			name: "no route",
			setup: func(s *scenario) {
				s.prober.routes["10.20.0.10"] = fakeRoute{err: fmt.Errorf("route to 10.20.0.10: %w", ErrNoRoute)}
			},
			addr:   "10.20.0.10",
			reason: "node has no route to 10.20.0.10",
		},
		{
			name: "route that changes with the source address",
			setup: func(s *scenario) {
				s.prober.routes["10.20.0.10"] = fakeRoute{err: fmt.Errorf("route to 10.20.0.10 from 10.20.0.2: %w", ErrRouteDiffers)}
			},
			addr:   "10.20.0.10",
			reason: "route to 10.20.0.10 changes with the source address",
		},
		{
			// The guest proves the address on its own bridge, but the host
			// sends traffic for it to the more specific network of another one.
			name: "more specific network on another bridge",
			setup: func(s *scenario) {
				s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.20.0.130/25")})
				s.web().NICs[0].Static = ips("10.20.0.140")
				s.prober.arp[arpKey("vmbr0", "10.20.0.140")] = []string{mac0}
			},
			addr:   "10.20.0.140",
			reason: "route to 10.20.0.140 leaves through vmbr1, not vmbr0",
		},
		{
			name: "VLAN interface",
			setup: func(s *scenario) {
				s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
				s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr0.10", Addrs: prefixes("10.30.0.2/24")})
				s.prober.arp[arpKey("vmbr0.10", "10.30.0.10")] = []string{mac0}
				s.prober.fdb[fdbKey("vmbr0", 10, mac0)] = []string{"tap101i0"}
			},
			addr: "10.30.0.10",
		},
		{
			name: "VLAN routed through the untagged bridge",
			setup: func(s *scenario) {
				s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
				s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr0.10", Addrs: prefixes("10.30.0.2/24")})
				s.prober.arp[arpKey("vmbr0.10", "10.30.0.10")] = []string{mac0}
				s.prober.routes["10.30.0.10"] = fakeRoute{iface: "vmbr0", onLink: true}
			},
			addr:   "10.30.0.10",
			reason: "route to 10.30.0.10 leaves through vmbr0, not vmbr0.10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			if tt.setup != nil {
				tt.setup(s)
			}

			res := s.resolve(t, webRoute(), nil)

			require.Equal(t, []probeCall{{op: "route", addr: ip(tt.addr)}}, s.prober.ops("route"))
			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.False(t, s.prober.touched(ip(tt.addr)), "nothing is sent for an address the host would not reach there")
				return
			}
			requireServed(t, res, tt.addr, t0)
		})
	}
}

func TestResolveRemoteGuestOnALocalPort(t *testing.T) {
	tests := []struct {
		name   string
		ports  []string
		reason string // empty when accepted
	}{
		{name: "not learned"},
		{name: "learned on the uplink", ports: []string{"eno1"}},
		{name: "learned on a bond", ports: []string{"bond0"}},
		{name: "learned on two uplinks", ports: []string{"eno1", "eno2"}},
		{name: "port that only looks like a tap", ports: []string{"tap101"}},
		{name: "tap without a NIC number", ports: []string{"tap101i"}},
		{name: "tap with a name after the number", ports: []string{"tap101ix"}},
		{name: "firewall link of a guest", ports: []string{"fwln101i0"}},
		{name: "veth of a container runtime", ports: []string{"veth3f2a1c"}},
		{
			name: "tap of a local guest", ports: []string{"tap102i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port tap102i0 but the guest runs on pve2",
		},
		{
			name: "firewall bridge of a local guest", ports: []string{"fwpr102p1"},
			reason: "MAC bc:24:11:00:00:01 is on local port fwpr102p1 but the guest runs on pve2",
		},
		{
			name: "veth of a local container", ports: []string{"veth200i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port veth200i0 but the guest runs on pve2",
		},
		{
			name: "the guest's own former tap", ports: []string{"tap101i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port tap101i0 but the guest runs on pve2",
		},
		{
			name: "uplink and a local guest", ports: []string{"eno1", "tap102i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port tap102i0 but the guest runs on pve2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().Node = "pve2"
			s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = tt.ports

			res := s.resolve(t, webRoute(), nil)

			require.Equal(t, []probeCall{{op: "fdb", iface: "vmbr0", mac: mac0}}, s.prober.ops("fdb"))
			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
		})
	}

	t.Run("every MAC that answered is checked", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"tap102i0"}

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, "MAC bc:24:11:00:00:02 is on local port tap102i0 but the guest runs on pve2")
	})

	t.Run("bound address is withdrawn", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap102i0"}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "MAC bc:24:11:00:00:01 is on local port tap102i0 but the guest runs on pve2",
			withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("forwarding table of a VLAN bridge", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
		s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr0v10", Addrs: prefixes("10.30.0.2/24")})
		s.prober.arp[arpKey("vmbr0v10", "10.30.0.10")] = []string{mac0}
		s.prober.fdb[fdbKey("vmbr0v10", 0, mac0)] = []string{"tap102i0"}

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, "MAC bc:24:11:00:00:01 is on local port tap102i0 but the guest runs on pve2")
	})

	t.Run("forwarding table cannot be read", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.prober.fdbErr = errors.New("dump interrupted")

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, "forwarding table of vmbr0: dump interrupted")
	})
}

func TestResolveRejectsAddressesOfEveryNode(t *testing.T) {
	const reason = "address of a cluster node"
	nodes := []inventory.Node{
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true, Ifaces: []pve.NodeIface{
			{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")},
		}},
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true, Ifaces: []pve.NodeIface{
			{Name: "vmbr0", Addrs: prefixes("10.20.0.3/24")},
			{Name: "vmbr1", Addrs: prefixes("10.20.0.33/24")},
		}},
		{Name: "pve3", Addr: ip("10.20.0.4")},
	}
	tests := []struct {
		name   string
		static []string
		prev   *Binding
		served string // empty when nothing is served
	}{
		{name: "cluster address of another node", static: []string{"10.20.0.3"}},
		{name: "interface address of another node", static: []string{"10.20.0.33"}},
		{name: "cluster address of an offline node", static: []string{"10.20.0.4"}},
		{name: "before a good candidate", static: []string{"10.20.0.3", "10.20.0.10"}, served: "10.20.0.10"},
		{name: "bound address became a node's", static: []string{"10.20.0.33"}, prev: boundTo("10.20.0.33")},
		{
			name: "bound address became a node's before a good candidate", static: []string{"10.20.0.33", "10.20.0.10"},
			prev: boundTo("10.20.0.33"), served: "10.20.0.10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.deny = denylist(t, nil, nil)
			s.nodes = nodes
			s.web().NICs[0].Static = ips(tt.static...)
			for _, a := range tt.static {
				s.prober.arp[arpKey("vmbr0", a)] = []string{mac0}
			}

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, CandidateResult{Addr: ip(tt.static[0]), Source: FromStatic, Reason: reason}, res.Candidates[0])
			require.False(t, s.prober.touched(ip(tt.static[0])))
			if tt.served != "" {
				requireServed(t, res, tt.served, t0)
				return
			}
			require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: reason}, res.Target)
			require.Nil(t, res.Binding)
		})
	}

	t.Run("bound address of a stopped guest", func(t *testing.T) {
		s := newScenario(t)
		s.deny = denylist(t, nil, nil)
		s.nodes = nodes
		s.web().Running = false

		res := s.resolve(t, webRoute(), boundTo("10.20.0.3"))

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: reason}, res.Target)
		require.Nil(t, res.Binding)
	})

	t.Run("bound address on a cancelled call", func(t *testing.T) {
		s := newScenario(t)
		s.deny = denylist(t, nil, nil)
		s.nodes = nodes
		s.web().NICs[0].Static = ips("10.20.0.3")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res := s.resolveCtx(ctx, webRoute(), boundTo("10.20.0.3"))

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: reason}, res.Target)
		require.Nil(t, res.Binding)
		require.Empty(t, s.prober.calls)
	})
}

func TestResolveTriesEveryNICForAGuessedAddress(t *testing.T) {
	setup := func(t *testing.T) *scenario {
		s := newScenario(t)
		s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0), {Index: 1, MAC: mac2}, nicOn(2, mac1, "vmbr1", 0)}
		s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})
		s.prober.arp[arpKey("vmbr1", "10.30.0.10")] = []string{mac1}
		s.prober.fdb[fdbKey("vmbr1", 0, mac1)] = []string{"tap101i2"}
		return s
	}
	for name, route := range map[string]model.Route{
		"explicit address": routeFor(ip("10.30.0.10"), ""),
		"via address":      routeFor(netip.Addr{}, "10.30.0.10"),
	} {
		t.Run(name, func(t *testing.T) {
			s := setup(t)

			res := s.resolve(t, route, nil)

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.30.0.10"), Reachable: true, Owner: webOwner}, res.Target)
			require.Equal(t, mac1, res.Binding.MAC)
			require.Equal(t, []CandidateResult{
				{Addr: ip("10.30.0.10"), Source: FromVia, Reason: "node has no address on vmbr0 in the guest's network"},
				{Addr: ip("10.30.0.10"), Source: FromVia, OK: true, Level: "port"},
			}, res.Candidates)
		})
	}

	t.Run("no NIC proves it", func(t *testing.T) {
		s := setup(t)
		s.prober.arp[arpKey("vmbr1", "10.30.0.10")] = []string{foreignMAC}

		res := s.resolve(t, routeFor(ip("10.30.0.10"), ""), nil)

		requireNotServed(t, res, "node has no address on vmbr0 in the guest's network")
		require.Len(t, res.Candidates, 2)
		require.Equal(t, "10.30.0.10 answered by bc:24:11:ff:ff:01, which is not this guest", res.Candidates[1].Reason)
	})
}

func TestResolveConcurrentCalls(t *testing.T) {
	s := newScenario(t)
	s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
	r := NewResolver(s.prober, s.settings, s.clock.now)
	snap := s.snapshot()
	prev := failingAt(boundTo("10.20.0.10"), t0.Add(-time.Hour))
	results := make([]Result, 16)

	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() { results[i] = r.Resolve(t.Context(), webRoute(), snap, prev, s.deny) })
	}
	wg.Wait()

	for _, res := range results {
		requireServed(t, res, "10.20.0.11", t0)
	}
	require.Equal(t, failingAt(boundTo("10.20.0.10"), t0.Add(-time.Hour)), prev, "the shared binding is not changed")
}
