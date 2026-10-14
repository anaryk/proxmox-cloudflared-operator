package resolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// classifyingProber is a fakeProber that also knows the kinds of the ports
// of its bridges, as the host prober does.
type classifyingProber struct {
	*fakeProber
	kinds    map[string]map[string]PortKind // by bridge, then port
	kindsErr error
}

func (p *classifyingProber) PortKinds(_ context.Context, bridge string) (map[string]PortKind, error) {
	if err := p.record(probeCall{op: "kinds", iface: bridge}); err != nil {
		return nil, err
	}
	if p.kindsErr != nil {
		return nil, p.kindsErr
	}
	return p.kinds[bridge], nil
}

// remoteScenario is web-1 running on pve2 while the check runs on pve1: its
// MAC answers ARP on vmbr0 and is learned on the uplink eno1. pve2 attests
// it on tap101i0, freshly, and every online node runs a watch.
func remoteScenario(t *testing.T) (*scenario, *classifyingProber) {
	s := newScenario(t)
	s.web().Node = "pve2"
	s.settings.Attested = true
	s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}
	p := &classifyingProber{fakeProber: s.prober, kinds: map[string]map[string]PortKind{
		"vmbr0": {"eno1": PortUplink, "eno2": PortUplink, "tap102i0": PortGuest, "vethpco3": PortLeg, "dummy0": PortUnknown},
	}}
	return s, p
}

func freshAttestation() *Attestation {
	return &Attestation{
		Node: "pve2", MAC: mac0, Bridge: "vmbr0", Port: "tap101i0",
		StatusAge: 5 * time.Second, HeardAt: t0.Add(-time.Second), StableSince: t0.Add(-10 * time.Minute),
	}
}

func (s *scenario) verifyRequest() VerifyRequest {
	return VerifyRequest{
		Route: webRoute(), Guest: *s.web(), Addr: ip("10.20.0.10"), Port: 80, MACs: []string{mac0},
		Snap: s.snapshot(), Deny: s.deny, Required: s.required, Attestation: freshAttestation(), Covered: true,
	}
}

func (s *scenario) verify(t *testing.T, p Prober, req VerifyRequest) VerifyResult {
	t.Helper()
	return NewResolver(p, s.settings, s.clock.now).VerifyAddress(t.Context(), req)
}

func TestVerifyAddressOfAGuestOfThisNode(t *testing.T) {
	s := newScenario(t)
	p := &classifyingProber{fakeProber: s.prober}

	res := s.verify(t, p, s.verifyRequest())

	requireServed(t, res.Result, "10.20.0.10", t0)
	require.False(t, res.Attested)
	require.Equal(t, "tap101i0", res.Binding.Port)
	require.Empty(t, p.ops("kinds"), "the guest's own port proves it here")
}

func TestVerifyAddressOfAGuestOfAnotherNode(t *testing.T) {
	t.Run("attested and covered", func(t *testing.T) {
		s, p := remoteScenario(t)

		res := s.verify(t, p, s.verifyRequest())

		requireServedAt(t, res.Result, "10.20.0.10", t0, LevelPort)
		require.True(t, res.Attested)
		require.Equal(t, "pve2", res.Binding.AttestedBy, "the node whose attestation it stands on")
		require.Equal(t, "vmbr0", res.Binding.Bridge)
		require.Equal(t, "eno1", res.Binding.Port)
		require.Equal(t, []probeCall{{op: "kinds", iface: "vmbr0"}}, p.ops("kinds"))
	})

	observed := []struct {
		name  string
		setup func(*scenario, *classifyingProber, *VerifyRequest)
		kinds bool // whether the kinds of the ports are read
	}{
		{name: "without coverage", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Covered = false }},
		{name: "no attestation", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Attestation = nil }},
		{name: "attestation of another MAC", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Attestation.MAC = mac1 }},
		{name: "attestation from another node", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Attestation.Node = "pve3" }},
		{name: "attestation of this node", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Attestation.Node = localNode }},
		{name: "attestation of another guest's port", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Attestation.Port = "tap102i0" }},
		{name: "attestation of another card's port", setup: func(_ *scenario, _ *classifyingProber, r *VerifyRequest) { r.Attestation.Port = "tap101i1" }},
		{name: "not offered by the settings", setup: func(s *scenario, _ *classifyingProber, _ *VerifyRequest) { s.settings.Attested = false }},
		{name: "unknown kind of port", kinds: true, setup: func(s *scenario, _ *classifyingProber, _ *VerifyRequest) {
			s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"dummy0"}
		}},
		{name: "port the kinds do not list", kinds: true, setup: func(s *scenario, _ *classifyingProber, _ *VerifyRequest) {
			s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno9"}
		}},
		{name: "kinds cannot be read", kinds: true, setup: func(_ *scenario, p *classifyingProber, _ *VerifyRequest) {
			p.kindsErr = errors.New("dump interrupted")
		}},
		{name: "MAC not learned", setup: func(s *scenario, _ *classifyingProber, _ *VerifyRequest) {
			delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))
		}},
	}
	for _, tt := range observed {
		t.Run(tt.name, func(t *testing.T) {
			s, p := remoteScenario(t)
			req := s.verifyRequest()
			tt.setup(s, p, &req)

			res := s.verify(t, p, req)

			requireServedAt(t, res.Result, "10.20.0.10", t0, LevelObserved)
			require.False(t, res.Attested)
			require.Empty(t, res.Binding.Port)
			require.Len(t, p.ops("fdb"), 1, "the forwarding table is read either way")
			require.Equal(t, tt.kinds, len(p.ops("kinds")) > 0)
		})
	}

	t.Run("the attestation must be fresh, stable and of the card", func(t *testing.T) {
		tests := []struct {
			name     string
			change   func(*Attestation)
			attested bool
		}{
			{name: "status at the limit of its age", change: func(a *Attestation) { a.StatusAge = 30 * time.Second }, attested: true},
			{name: "status too old", change: func(a *Attestation) { a.StatusAge = 30*time.Second + time.Nanosecond }},
			{name: "status age below zero", change: func(a *Attestation) { a.StatusAge = -time.Second }},
			{name: "heartbeat at the limit of its age", change: func(a *Attestation) { a.HeardAt = t0.Add(-15 * time.Second) }, attested: true},
			{name: "heartbeat too old", change: func(a *Attestation) { a.HeardAt = t0.Add(-15*time.Second - time.Nanosecond) }},
			{name: "heartbeat in the future", change: func(a *Attestation) { a.HeardAt = t0.Add(time.Second) }},
			{name: "heartbeat never heard", change: func(a *Attestation) { a.HeardAt = time.Time{} }},
			{name: "stable for the proof age", change: func(a *Attestation) { a.StableSince = t0.Add(-DefaultMaxProofAge) }, attested: true},
			{name: "stable for less", change: func(a *Attestation) { a.StableSince = t0.Add(-DefaultMaxProofAge + time.Second) }},
			{name: "stable since a time in the future", change: func(a *Attestation) { a.StableSince = t0.Add(time.Minute) }},
			{name: "no stable time", change: func(a *Attestation) { a.StableSince = time.Time{} }},
			{name: "no bridge said", change: func(a *Attestation) { a.Bridge = "" }, attested: true},
			{name: "another bridge", change: func(a *Attestation) { a.Bridge = "vmbr1" }},
			{name: "another VLAN", change: func(a *Attestation) { a.VLAN = 20 }},
			{name: "MAC in another spelling", change: func(a *Attestation) { a.MAC = "BC-24-11-00-00-01" }, attested: true},
			{name: "no MAC", change: func(a *Attestation) { a.MAC = "" }},
			{name: "MAC that does not parse", change: func(a *Attestation) { a.MAC = "not-a-mac" }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s, p := remoteScenario(t)
				req := s.verifyRequest()
				tt.change(req.Attestation)

				res := s.verify(t, p, req)

				require.Equal(t, tt.attested, res.Attested)
				require.Equal(t, map[bool]Level{true: LevelPort, false: LevelObserved}[tt.attested], res.Level)
			})
		}
	})

	t.Run("the bridge of a VLAN as the card's or as Proxmox makes it", func(t *testing.T) {
		for _, at := range []struct {
			bridge string
			vlan   int
		}{{"vmbr0", 10}, {"vmbr0v10", 0}} {
			s, p := remoteScenario(t)
			s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr0v10", Addrs: prefixes("10.30.0.2/24")})
			s.prober.arp[arpKey("vmbr0v10", "10.30.0.10")] = []string{mac0}
			s.prober.fdb[fdbKey("vmbr0v10", 0, mac0)] = []string{"eno1.10"}
			p.kinds["vmbr0v10"] = map[string]PortKind{"eno1.10": PortUplink}
			req := s.verifyRequest()
			req.Guest, req.Snap, req.Addr = *s.web(), s.snapshot(), ip("10.30.0.10")
			req.Attestation.Bridge, req.Attestation.VLAN = at.bridge, at.vlan

			res := s.verify(t, p, req)

			require.True(t, res.Attested, "%s VLAN %d", at.bridge, at.vlan)
		}
	})

	t.Run("a prober that cannot classify ports", func(t *testing.T) {
		s, _ := remoteScenario(t)

		res := s.verify(t, s.prober, s.verifyRequest())

		requireServedAt(t, res.Result, "10.20.0.10", t0, LevelObserved)
		require.False(t, res.Attested)
	})

	failing := []struct {
		name   string
		ports  []string
		reason string
	}{
		{
			name: "on a local guest's port", ports: []string{"tap102i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port tap102i0 but the guest runs on pve2",
		},
		{
			name: "on the guest's own former port", ports: []string{"tap101i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port tap101i0 but the guest runs on pve2",
		},
		{
			name: "on two ports", ports: []string{"eno1", "eno2"},
			reason: "MAC bc:24:11:00:00:01 is on several ports: eno1, eno2",
		},
		{
			name: "on the uplink and a local guest's port", ports: []string{"eno1", "tap102i0"},
			reason: "MAC bc:24:11:00:00:01 is on local port tap102i0 but the guest runs on pve2",
		},
		{
			name: "on a leg", ports: []string{"vethpco3"},
			reason: "MAC bc:24:11:00:00:01 is on port vethpco3, a leg of pco, not on an uplink",
		},
	}
	for _, tt := range failing {
		t.Run(tt.name, func(t *testing.T) {
			s, p := remoteScenario(t)
			s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = tt.ports

			res := s.verify(t, p, s.verifyRequest())

			requireNotServed(t, res.Result, tt.reason)
			require.False(t, res.Attested)
			require.Empty(t, p.ops("dial"))
		})
	}

	t.Run("without an attestation the kinds the names tell still apply", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"vethpco3"}
		req := s.verifyRequest()
		req.Attestation = nil

		res := s.verify(t, p, req)

		requireNotServed(t, res.Result, "MAC bc:24:11:00:00:01 is on port vethpco3, a leg of pco, not on an uplink")
		require.Empty(t, p.ops("kinds"))
	})

	t.Run("every MAC that answered must be attested", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"eno1"}
		req := s.verifyRequest()
		req.Guest, req.MACs = *s.web(), []string{mac0, mac1}

		res := s.verify(t, p, req)

		requireServedAt(t, res.Result, "10.20.0.10", t0, LevelObserved)
		require.False(t, res.Attested)
	})
}

// While no controller refreshes it, an attestation goes on holding up an
// address that stood at port on it, as long as the wire shows what it
// showed; it never raises an address to port or admits one.
func TestVerifyAddressHoldsAnAttestationOverASilence(t *testing.T) {
	stood := func(t *testing.T) *Binding {
		t.Helper()
		s, p := remoteScenario(t)
		res := s.verify(t, p, s.verifyRequest())
		require.True(t, res.Attested)
		return res.Binding
	}
	tests := []struct {
		name         string
		after        time.Duration // since the proof that stood
		change       func(*scenario, *VerifyRequest)
		attested     bool
		stale        bool
		withoutStood bool
	}{
		{name: "a takeover's silence", after: 3 * time.Minute, attested: true},
		{name: "longer than a proof may stand", after: DefaultMaxProofAge + time.Second, attested: true, stale: true},
		{name: "a last heartbeat that does not name the node", after: 5 * time.Second, attested: true,
			change: func(_ *scenario, r *VerifyRequest) { r.Attestation.Recalled = true }},
		{name: "no address stood on it", after: time.Minute, withoutStood: true},
		{name: "recalled for an address that did not stand on it", after: 5 * time.Second, withoutStood: true,
			change: func(_ *scenario, r *VerifyRequest) { r.Attestation.Recalled = true }},
		{name: "a withdrawn binding", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Prev.Withdrawn = true }},
		{name: "a binding below port", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Prev.Level = LevelObserved }},
		{name: "the node's status stale in the last heartbeat", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Attestation.StatusAge = maxStatusAge + time.Second }},
		{name: "a heartbeat never heard", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Attestation.HeardAt = time.Time{} }},
		{name: "a heartbeat dated later", after: time.Minute,
			change: func(s *scenario, r *VerifyRequest) { r.Attestation.HeardAt = s.clock.t.Add(time.Second) }},
		{name: "the MAC on another uplink", after: time.Minute,
			change: func(s *scenario, _ *VerifyRequest) { s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno2"} }},
		{name: "no coverage", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Covered = false }},
		{name: "at the ceiling of a hold-over", after: MaxHoldOver - time.Second, attested: true, stale: true},
		{name: "past the ceiling of a hold-over", after: MaxHoldOver},
		{name: "a binding proven longer ago than the ceiling", after: time.Minute,
			change: func(s *scenario, r *VerifyRequest) { r.Prev.VerifiedAt = s.clock.t.Add(-MaxHoldOver - time.Second) }},
		{name: "a binding that stood on another node's attestation", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Prev.AttestedBy = "pve3" }},
		{name: "a binding that stood on no attestation", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Prev.AttestedBy = "" }},
		{name: "a binding of another guest", after: time.Minute,
			change: func(_ *scenario, r *VerifyRequest) { r.Prev.Guest = "qemu/999" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := stood(t)
			s, p := remoteScenario(t)
			s.clock.t = t0.Add(tt.after)
			req := s.verifyRequest()
			if !tt.withoutStood {
				req.Prev = b
			}
			if tt.change != nil {
				tt.change(s, &req)
			}

			res := s.verify(t, p, req)

			require.Equal(t, tt.attested, res.Attested)
			require.Equal(t, tt.stale, res.Stale)
			require.Equal(t, map[bool]Level{true: LevelPort, false: LevelObserved}[tt.attested], res.Level)
			if tt.attested {
				require.Equal(t, s.clock.t, res.Binding.VerifiedAt, "proven anew on the wire")
				require.Equal(t, "pve2", res.Binding.AttestedBy)
			} else {
				require.Empty(t, res.Binding.AttestedBy, "a binding below port stood on no attestation")
			}
		})
	}

	t.Run("what the wire shows against the guest still withdraws", func(t *testing.T) {
		b := stood(t)
		s, p := remoteScenario(t)
		s.clock.t = t0.Add(time.Minute)
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap102i0"}
		req := s.verifyRequest()
		req.Prev = b

		res := s.verify(t, p, req)

		require.True(t, res.Target.Withdrawn)
		require.False(t, res.Attested)
	})
}

func TestVerifyAddressChecksWhatAnyCheckDoes(t *testing.T) {
	t.Run("a denied address is rejected without a probe", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.deny = denylist(t, ips("10.20.0.2"), prefixes("10.20.0.0/24"))
		req := s.verifyRequest()

		res := s.verify(t, p, req)

		require.True(t, res.Target.Rejected)
		require.Nil(t, res.Binding)
		require.False(t, p.touched(ip("10.20.0.10")))
	})

	t.Run("a MAC the resolving node did not see answer", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
		req := s.verifyRequest()
		req.Guest = *s.web()

		res := s.verify(t, p, req)

		requireNotServed(t, res.Result, "10.20.0.10 answered by bc:24:11:00:00:02, not by the MACs the address was chosen with")
	})

	t.Run("an address that is not the guest's", func(t *testing.T) {
		s, p := remoteScenario(t)
		req := s.verifyRequest()
		req.Addr = ip("10.20.0.77")

		res := s.verify(t, p, req)

		requireNotServed(t, res.Result, "10.20.0.77 is not an address of the guest")
		require.Empty(t, p.calls)
	})

	t.Run("a MAC the guest does not have", func(t *testing.T) {
		s, p := remoteScenario(t)
		req := s.verifyRequest()
		req.MACs = []string{foreignMAC}

		res := s.verify(t, p, req)

		requireNotServed(t, res.Result, "no card of the guest has the MAC bc:24:11:ff:ff:01 for 10.20.0.10")
	})

	t.Run("a dial failure without a binding", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.verify(t, p, s.verifyRequest())

		require.Equal(t, planner.ResolvedTarget{Reason: "port 80: connection refused"}, res.Target)
		require.Nil(t, res.Binding)
	})

	t.Run("a dial failure keeps the binding", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.prober.dialErr[ip("10.20.0.10")] = errDial
		req := s.verifyRequest()
		req.Prev = boundTo("10.20.0.10")

		res := s.verify(t, p, req)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
		require.Equal(t, timePtr(t0), res.Binding.FailingSince)
		require.Equal(t, LevelPort, res.Binding.Level)
	})

	t.Run("lost identity withdraws the binding", func(t *testing.T) {
		s, p := remoteScenario(t)
		delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))
		req := s.verifyRequest()
		req.Prev = boundTo("10.20.0.10")

		res := s.verify(t, p, req)

		requireWithdrawn(t, res.Result, "no ARP answer on vmbr0", withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("a stopped guest withdraws the binding", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.web().Running = false
		req := s.verifyRequest()
		req.Guest, req.Snap, req.Prev = *s.web(), s.snapshot(), boundTo("10.20.0.10")

		res := s.verify(t, p, req)

		require.True(t, res.Target.Withdrawn)
		require.Empty(t, p.calls)
	})

	t.Run("the port of the request is dialled", func(t *testing.T) {
		s, p := remoteScenario(t)
		req := s.verifyRequest()
		req.Port = 8443

		_ = s.verify(t, p, req)

		require.Equal(t, []probeCall{{op: "dial", addr: ip("10.20.0.10"), port: 8443}}, p.ops("dial"))
	})

	t.Run("a ceiling below port", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.settings.Ceiling = func(HostIface) Level { return LevelObserved }

		res := s.verify(t, p, s.verifyRequest())

		requireServedAt(t, res.Result, "10.20.0.10", t0, LevelObserved)
		require.Empty(t, p.ops("fdb"))
		require.Empty(t, p.ops("kinds"))
	})

	t.Run("no forwarding table", func(t *testing.T) {
		s, p := remoteScenario(t)
		s.settings.NoForwardingTable = true

		res := s.verify(t, p, s.verifyRequest())

		requireServedAt(t, res.Result, "10.20.0.10", t0, LevelObserved)
		require.Empty(t, p.ops("fdb"))
	})

	t.Run("a route without a guest is checked as Resolve checks it", func(t *testing.T) {
		s, p := remoteScenario(t)
		req := s.verifyRequest()
		req.Route = model.Route{Hostname: "db.example.com", Target: model.Target{Scheme: model.SchemeHTTP, Addr: ip("10.20.0.50"), Port: 5432}, Source: model.SourceManual}

		res := s.verify(t, p, req)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Reachable: true, Owner: req.Route.Owner()}, res.Target)
		require.Equal(t, LevelManual, res.Level)
	})

	t.Run("an attested binding is not carried as port by Resolve", func(t *testing.T) {
		s, p := remoteScenario(t)
		attested := s.verify(t, p, s.verifyRequest()).Binding
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")
		s.clock.t = t0.Add(time.Minute)

		res := s.resolve(t, webRoute(), attested)

		require.Equal(t, LevelObserved, res.Level)
	})
}

func TestVerifyAddressOfAnAttestedGuestHonoursTheContext(t *testing.T) {
	s, p := remoteScenario(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := NewResolver(p, s.settings, s.clock.now).VerifyAddress(ctx, s.verifyRequest())

	require.Equal(t, planner.ResolvedTarget{Reason: reasonCancelled}, res.Target)
	require.Empty(t, p.calls)
}
