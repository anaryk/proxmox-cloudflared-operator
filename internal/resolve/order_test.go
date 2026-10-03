package resolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// trustedFirst is web-1 with a trusted static address in a routed network
// listed before its address on vmbr0, which answers on its own port.
func trustedFirst(t *testing.T) *scenario {
	s := newScenario(t)
	s.settings.TrustStatic = true
	s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.40.0.10", "10.20.0.10")}
	return s
}

func TestResolveServesTheCandidateProvenAtTheHighestLevel(t *testing.T) {
	s := trustedFirst(t)

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.40.0.10"), Source: FromStatic, OK: true, Level: "observed"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
	}, res.Candidates)
}

// trustedOnly is web-1 on this node with four trusted static addresses in a
// routed network and none the node has an address next to.
func trustedOnly(t *testing.T) *scenario {
	s := newScenario(t)
	s.settings.TrustStatic = true
	s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.40.0.10", "10.40.0.11", "10.40.0.12", "10.40.0.13")}
	return s
}

func TestResolveTakesTheFirstOfEqualLevels(t *testing.T) {
	// A trusted static address is proven at observed at most, so once the
	// first passes the others cannot change the result and are not tried,
	// although port could be proven for an address next to the node.
	for _, prev := range []*Binding{nil, atLevel(boundTo("10.40.0.10"), LevelObserved)} {
		t.Run("trusted static addresses, bound: "+boolWord(prev != nil), func(t *testing.T) {
			s := trustedOnly(t)

			res := s.resolve(t, webRoute(), prev)

			requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
			require.Equal(t, []CandidateResult{
				{Addr: ip("10.40.0.10"), Source: FromStatic, OK: true, Level: "observed"},
				{Addr: ip("10.40.0.11"), Source: FromStatic, Reason: "not tried"},
				{Addr: ip("10.40.0.12"), Source: FromStatic, Reason: "not tried"},
				{Addr: ip("10.40.0.13"), Source: FromStatic, Reason: "not tried"},
			}, res.Candidates)
			require.Equal(t, []probeCall{
				{op: "interfaces"},
				{op: "route", addr: ip("10.40.0.10")},
				{op: "dial", addr: ip("10.40.0.10"), port: 80},
			}, s.prober.calls, "the cost of one candidate")
		})
	}
	t.Run("a guest on another node, where nothing beats observed", func(t *testing.T) {
		s := newScenario(t)
		s.web().Node = "pve2"
		s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10", "10.20.0.11")}
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}

		res := s.resolve(t, webRoute(), nil)

		requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "observed"},
			{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: "not tried"},
		}, res.Candidates)
		require.False(t, s.prober.touched(ip("10.20.0.11")))
	})
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// After a trusted static address passes, a later address next to the node is
// still tried, as it can be proven at port; another trusted one is not.
func TestResolveTriesOnlyCandidatesThatCanProveMore(t *testing.T) {
	s := trustedFirst(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.40.0.10", "10.40.0.11", "10.20.0.10")}

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.40.0.10"), Source: FromStatic, OK: true, Level: "observed"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
		{Addr: ip("10.40.0.11"), Source: FromStatic, Reason: "not tried"},
	}, res.Candidates)
	require.False(t, s.prober.touched(ip("10.40.0.11")))
}

func TestResolveStopsAtPort(t *testing.T) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10", "10.40.0.10")}
	s.settings.TrustStatic = true
	s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
		{Addr: ip("10.40.0.10"), Source: FromStatic, Reason: "not tried"},
	}, res.Candidates)
	require.Equal(t, []probeCall{
		{op: "interfaces"},
		{op: "route", addr: ip("10.20.0.10")},
		{op: "arp", iface: "vmbr0", addr: ip("10.20.0.10")},
		{op: "fdb", iface: "vmbr0", mac: mac0},
		{op: "dial", addr: ip("10.20.0.10"), port: 80},
	}, s.prober.calls, "an on-link first candidate costs what it did")
}

// Level comes first: a binding to a lower candidate that has settled stays
// only while no other candidate is proven higher.
func TestResolveBindingMovesToAHigherLevel(t *testing.T) {
	s := trustedFirst(t)
	prev := boundSince(atLevel(boundTo("10.40.0.10"), LevelObserved), t0.Add(-time.Hour))

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.40.0.10"), Source: FromStatic, OK: true, Level: "observed"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
	}, res.Candidates)

	t.Run("and stays while the higher one fails", func(t *testing.T) {
		s := trustedFirst(t)
		delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))

		res := s.resolve(t, webRoute(), prev)

		requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
	})
}

func TestResolveServesWhatWasProvenBeforeACancel(t *testing.T) {
	s := trustedFirst(t)
	ctx, cancel := context.WithCancel(t.Context())
	s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.10"), cancel

	res := s.resolveCtx(ctx, webRoute(), atLevel(boundTo("10.40.0.10"), LevelObserved))

	requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
}

func TestResolveViaNarrowsTheCandidatesBeforeTheLevel(t *testing.T) {
	s := trustedFirst(t)

	res := s.resolve(t, routeFor(ip("10.40.0.10"), ""), nil)

	requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
	require.Len(t, res.Candidates, 1)
}

func TestResolveCandidatesReportTheirLevel(t *testing.T) {
	s, prev := stickyScenario(t)

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: "port 80: connection refused", Level: "port"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "not tried"},
	}, res.Candidates, "the identity of a candidate whose port fails is proven all the same")
}

// A proof kept while the wire cannot be asked claims port only where the
// forwarding table can still place the guest's MACs.
func TestResolveKeptProofIsCappedAwayFromThisNode(t *testing.T) {
	arpFails := func(s *scenario) { s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed") }
	tests := []struct {
		name  string
		setup func(s *scenario)
		level Level
	}{
		{name: "prober error, guest on this node", setup: arpFails, level: LevelPort},
		{name: "prober error, guest on another node", setup: func(s *scenario) { arpFails(s); s.web().Node = "pve2" }, level: LevelObserved},
		{
			name: "no node known and the MAC on the uplink",
			setup: func(s *scenario) {
				s.nodes = nil
				s.web().Node = "pve2"
				s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}
			},
			level: LevelObserved,
		},
		{
			name:  "state unknown, guest on another node",
			setup: func(s *scenario) { s.web().Node, s.web().Running, s.web().StatusUnknown = "pve2", false, true },
			level: LevelObserved,
		},
		{
			name:  "state unknown, guest on this node",
			setup: func(s *scenario) { s.web().Running, s.web().StatusUnknown = false, true },
			level: LevelPort,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			tt.setup(s)
			prev := atLevel(boundTo("10.20.0.10"), LevelPort)

			res := s.resolve(t, webRoute(), prev)

			require.Equal(t, ip("10.20.0.10"), res.Target.Addr)
			require.False(t, res.Target.Withdrawn, res.Target.Reason)
			require.Equal(t, tt.level, res.Level)
			require.Equal(t, LevelPort, res.Binding.Level, "the binding keeps the level its proof was made at")
		})
	}
}
