package resolve

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	mac0 = "bc:24:11:00:00:01"
	mac1 = "bc:24:11:00:00:02"
	mac2 = "bc:24:11:00:00:03"
)

var web1Ref = model.GuestRef{Kind: model.KindQEMU, VMID: 101}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func ips(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, ip(s))
	}
	return out
}

func guestWith(nics []model.NIC, reported ...model.ReportedAddr) model.Guest {
	return model.Guest{Ref: web1Ref, Name: "web-1", NICs: nics, Reported: reported}
}

func rep(mac, addr string) model.ReportedAddr {
	return model.ReportedAddr{Iface: "eth0", MAC: mac, Addr: ip(addr)}
}

func routeFor(target netip.Addr, via string) model.Route {
	ref := web1Ref
	return model.Route{
		Hostname: "web.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Addr: target, Port: 80},
		Options:  model.RouteOptions{Via: via},
		Source:   model.SourceAnnotation,
		Guest:    &ref,
	}
}

type want struct {
	addr   string
	nic    int
	source CandidateSource
}

func requireCandidates(t *testing.T, g model.Guest, got []Candidate, wants []want) {
	t.Helper()
	require.Len(t, got, len(wants))
	for i, w := range wants {
		require.Equal(t, ip(w.addr), got[i].Addr, "candidate %d address", i)
		require.Equal(t, w.source, got[i].Source, "candidate %d source", i)
		nic, ok := g.NIC(w.nic)
		require.True(t, ok, "test wants unknown NIC %d", w.nic)
		require.Equal(t, nic, got[i].NIC, "candidate %d NIC", i)
	}
}

func TestCandidatesStaticBeforeReported(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")},
			{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.30.0.10")},
		},
		rep(mac0, "10.20.0.11"),
		rep(mac1, "10.30.0.11"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.20.0.10", 0, FromStatic},
		{"10.30.0.10", 1, FromStatic},
		{"10.20.0.11", 0, FromAgent},
		{"10.30.0.11", 1, FromAgent},
	})
}

func TestCandidatesNICIndexOrderNotSliceOrder(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 2, MAC: mac2, Bridge: "vmbr0", Static: ips("10.20.0.12")},
			{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")},
		},
		rep(mac2, "10.20.0.22"),
		rep(mac0, "10.20.0.20"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.20.0.10", 0, FromStatic},
		{"10.20.0.12", 2, FromStatic},
		{"10.20.0.20", 0, FromAgent},
		{"10.20.0.22", 2, FromAgent},
	})
	require.Equal(t, 2, g.NICs[0].Index, "the guest must not be reordered")
}

func TestCandidatesReportedMatchedByMAC(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0"},
			{Index: 1, MAC: mac1, Bridge: "vmbr1"},
		},
		rep(mac1, "10.30.0.11"),
		rep("BC:24:11:00:00:01", "10.20.0.11"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.20.0.11", 0, FromAgent},
		{"10.30.0.11", 1, FromAgent},
	})
}

func TestCandidatesIgnoreReportedWithoutNIC(t *testing.T) {
	g := guestWith(
		[]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0"}},
		rep(mac2, "172.17.0.2"),
		rep("", "10.20.0.50"),
		rep(mac0, "10.20.0.11"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{{"10.20.0.11", 0, FromAgent}})
}

func TestCandidatesEmptyMACNICMatchesNothing(t *testing.T) {
	g := guestWith(
		[]model.NIC{{Index: 0, MAC: "", Bridge: "vmbr0"}},
		rep("", "10.20.0.50"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCandidatesDropDuplicatesOnTheSameNIC(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10", "10.20.0.10")},
			{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.20.0.10", "10.30.0.10")},
		},
		rep(mac0, "10.20.0.10"),
		rep(mac0, "10.20.0.11"),
		rep(mac1, "10.20.0.11"),
		rep(mac0, "10.20.0.11"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.20.0.10", 0, FromStatic},
		{"10.20.0.10", 1, FromStatic},
		{"10.30.0.10", 1, FromStatic},
		{"10.20.0.11", 0, FromAgent},
		{"10.20.0.11", 1, FromAgent},
	})
}

func TestCandidatesDropDuplicatesWithNICSelector(t *testing.T) {
	g := guestWith(
		[]model.NIC{{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.30.0.10")}},
		rep(mac1, "10.30.0.10"),
		rep(mac1, "10.30.0.11"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, "net1"), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.30.0.10", 1, FromStatic},
		{"10.30.0.11", 1, FromAgent},
	})
}

func TestCandidatesSkipUnusableAddresses(t *testing.T) {
	g := guestWith(
		[]model.NIC{{
			Index: 0, MAC: mac0, Bridge: "vmbr0",
			Static: ips("fd00::10", "0.0.0.0", "224.0.0.5", "::ffff:10.20.0.99", "10.20.0.10"),
		}},
		rep(mac0, "fe80::1"),
		rep(mac0, "0.0.0.0"),
		rep(mac0, "239.1.2.3"),
		rep(mac0, "10.20.0.11"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.20.0.10", 0, FromStatic},
		{"10.20.0.11", 0, FromAgent},
	})
}

func TestCandidatesInvalidAddressIgnored(t *testing.T) {
	g := guestWith(
		[]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: []netip.Addr{{}}}},
		model.ReportedAddr{Iface: "eth0", MAC: mac0},
	)
	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCandidatesGuestWithoutAddresses(t *testing.T) {
	tests := []struct {
		name  string
		guest model.Guest
	}{
		{"no nics", guestWith(nil)},
		{"nics without addresses", guestWith([]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0"}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Candidates(routeFor(netip.Addr{}, ""), tt.guest)
			require.NoError(t, err)
			require.Empty(t, got)
		})
	}
}

func TestCandidatesViaNIC(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")},
			{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.30.0.10")},
		},
		rep(mac0, "10.20.0.11"),
		rep(mac1, "10.30.0.11"),
		rep(mac2, "172.17.0.2"),
		rep("", "10.30.0.50"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, "net1"), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{
		{"10.30.0.10", 1, FromStatic},
		{"10.30.0.11", 1, FromAgent},
	})
}

func TestCandidatesViaNICWithoutAddresses(t *testing.T) {
	g := guestWith([]model.NIC{{Index: 1, MAC: mac1, Bridge: "vmbr1"}})
	got, err := Candidates(routeFor(netip.Addr{}, "net1"), g)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCandidatesViaUnknownNIC(t *testing.T) {
	g := guestWith([]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")}})
	got, err := Candidates(routeFor(netip.Addr{}, "net9"), g)
	require.EqualError(t, err, "guest has no net9")
	require.Nil(t, got)
}

func TestCandidatesViaAddress(t *testing.T) {
	nics := []model.NIC{
		{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")},
		{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.30.0.10")},
	}
	reported := []model.ReportedAddr{
		rep(mac1, "10.30.0.11"),
		rep(mac2, "172.17.0.2"),
		rep("", "10.40.0.50"),
	}
	tests := []struct {
		name  string
		via   string
		wants []want
	}{
		{"static on the second nic", "10.30.0.10", []want{{"10.30.0.10", 1, FromVia}}},
		{"static on the first nic", "10.20.0.10", []want{{"10.20.0.10", 0, FromVia}}},
		{"reported with the MAC of a nic", "10.30.0.11", []want{{"10.30.0.11", 1, FromVia}}},
		{"reported with an unknown MAC", "172.17.0.2", []want{{"172.17.0.2", 0, FromVia}, {"172.17.0.2", 1, FromVia}}},
		{"reported without a MAC", "10.40.0.50", []want{{"10.40.0.50", 0, FromVia}, {"10.40.0.50", 1, FromVia}}},
		{"unknown address", "10.20.0.99", []want{{"10.20.0.99", 0, FromVia}, {"10.20.0.99", 1, FromVia}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := guestWith(nics, reported...)
			got, err := Candidates(routeFor(netip.Addr{}, tt.via), g)
			require.NoError(t, err)
			requireCandidates(t, g, got, tt.wants)
		})
	}
}

func TestCandidatesViaAddressPrefersStaticOverReported(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0"},
			{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.20.0.10")},
		},
		rep(mac0, "10.20.0.10"),
	)
	got, err := Candidates(routeFor(netip.Addr{}, "10.20.0.10"), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{{"10.20.0.10", 1, FromVia}})
}

func TestCandidatesGuessedAddressOnEveryBridgedNIC(t *testing.T) {
	g := guestWith([]model.NIC{
		{Index: 3, MAC: mac2, Bridge: "vmbr2"},
		{Index: 1, MAC: mac1, Bridge: "vmbr1"},
		{Index: 0, MAC: mac0},
	})
	got, err := Candidates(routeFor(netip.Addr{}, "10.20.0.99"), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{{"10.20.0.99", 1, FromVia}, {"10.20.0.99", 3, FromVia}})
}

func TestCandidatesNamedAddressStaticOnTwoNICs(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0"},
			{Index: 1, MAC: mac1, Bridge: "vmbr1", Static: ips("10.20.0.10")},
			{Index: 2, MAC: mac2, Bridge: "vmbr2", Static: ips("10.20.0.10")},
		},
		rep(mac0, "10.20.0.10"),
	)
	got, err := Candidates(routeFor(ip("10.20.0.10"), ""), g)
	require.NoError(t, err)
	requireCandidates(t, g, got, []want{{"10.20.0.10", 1, FromVia}})
}

func TestCandidatesViaAddressNeedsANIC(t *testing.T) {
	tests := []struct {
		name  string
		guest model.Guest
		err   string
	}{
		{"no nic", guestWith(nil), "guest has no network interface"},
		{
			"no nic with a bridge",
			guestWith([]model.NIC{{Index: 0, MAC: mac0}}),
			"guest has no network interface with a bridge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Candidates(routeFor(netip.Addr{}, "10.20.0.99"), tt.guest)
			require.EqualError(t, err, tt.err)
			require.Nil(t, got)
		})
	}
}

func TestCandidatesExplicitAddress(t *testing.T) {
	g := guestWith(
		[]model.NIC{
			{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")},
			{Index: 1, MAC: mac1, Bridge: "vmbr1"},
		},
		rep(mac1, "10.30.0.11"),
	)
	tests := []struct {
		name  string
		addr  string
		wants []want
	}{
		{"static", "10.20.0.10", []want{{"10.20.0.10", 0, FromVia}}},
		{"reported", "10.30.0.11", []want{{"10.30.0.11", 1, FromVia}}},
		{"unknown", "10.50.0.1", []want{{"10.50.0.1", 0, FromVia}, {"10.50.0.1", 1, FromVia}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Candidates(routeFor(ip(tt.addr), ""), g)
			require.NoError(t, err)
			requireCandidates(t, g, got, tt.wants)
		})
	}
}

func TestCandidatesExplicitAddressWithoutNIC(t *testing.T) {
	got, err := Candidates(routeFor(ip("10.20.0.10"), ""), guestWith(nil))
	require.EqualError(t, err, "guest has no network interface")
	require.Nil(t, got)
}

func TestCandidatesErrors(t *testing.T) {
	g := guestWith([]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")}})
	noGuest := routeFor(netip.Addr{}, "")
	noGuest.Guest = nil

	tests := []struct {
		name  string
		route model.Route
		err   string
	}{
		{"route without a guest", noGuest, "route has no guest"},
		{"address and via", routeFor(ip("10.20.0.10"), "net0"), "route has both an address and via"},
		{"address and via address", routeFor(ip("10.20.0.10"), "10.20.0.10"), "route has both an address and via"},
		{"via is neither", routeFor(netip.Addr{}, "eth0"), `invalid via "eth0"`},
		{"via nic is not a number", routeFor(netip.Addr{}, "netx"), `invalid via "netx"`},
		{"via nic is signed", routeFor(netip.Addr{}, "net-1"), `invalid via "net-1"`},
		{"via is IPv6", routeFor(netip.Addr{}, "fd00::1"), "address fd00::1 is not a usable IPv4 address"},
		{"via is unspecified", routeFor(netip.Addr{}, "0.0.0.0"), "address 0.0.0.0 is not a usable IPv4 address"},
		{"explicit IPv6", routeFor(ip("fd00::1"), ""), "address fd00::1 is not a usable IPv4 address"},
		{"explicit multicast", routeFor(ip("224.0.0.1"), ""), "address 224.0.0.1 is not a usable IPv4 address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Candidates(tt.route, g)
			require.EqualError(t, err, tt.err)
			require.Nil(t, got)
		})
	}
}

func TestCandidatesGuestMustMatchRoute(t *testing.T) {
	g := guestWith([]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10")}})
	g.Ref = model.GuestRef{Kind: model.KindQEMU, VMID: 102}

	got, err := Candidates(routeFor(netip.Addr{}, ""), g)

	require.EqualError(t, err, "guest does not match the route")
	require.Nil(t, got)
}

func TestCandidatesShareNoStaticSlice(t *testing.T) {
	routes := []struct {
		name  string
		route model.Route
	}{
		{"discovered", routeFor(netip.Addr{}, "")},
		{"via nic", routeFor(netip.Addr{}, "net0")},
		{"explicit", routeFor(ip("10.20.0.10"), "")},
	}
	for _, tt := range routes {
		t.Run(tt.name, func(t *testing.T) {
			g := guestWith([]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips("10.20.0.10", "10.20.0.11")}})

			got, err := Candidates(tt.route, g)
			require.NoError(t, err)
			require.NotEmpty(t, got)

			g.NICs[0].Static[1] = ip("10.20.0.88")
			for _, c := range got {
				require.Equal(t, ips("10.20.0.10", "10.20.0.11"), c.NIC.Static)
				c.NIC.Static[0] = ip("10.20.0.99")
			}
			require.Equal(t, ips("10.20.0.10", "10.20.0.88"), g.NICs[0].Static)
		})
	}
}
