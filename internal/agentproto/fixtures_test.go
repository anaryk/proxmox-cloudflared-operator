package agentproto

import (
	"encoding/json"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const (
	sharedToken = "test-run-token-of-the-shared-tunnel"
	pinnedToken = "test-run-token-of-the-pinned-tunnel"
	sharedID    = "5f1c2c9e-8b1a-4d3e-9f6a-2b7c8d9e0f1a"
	pinnedID    = "c0ffee00-1234-4abc-8def-0123456789ab"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func epoch7() Epoch { return Epoch{InstallID: "0123456789ab", Generation: 7, Nonce: "a1b2c3d4"} }

func yes() *bool { b := true; return &b }

func attestation() Attestation {
	return Attestation{
		Node:   "node1",
		Guest:  model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		NIC:    1,
		MAC:    "bc:24:11:aa:bb:01",
		Bridge: "vmbr0",
		VLAN:   20,
		Port:   "tap101i1",
		Since:  t0.Add(-time.Hour),
	}
}

// fullSnapshot sets every field of the snapshot and of the types it holds, so
// that its golden pins each of them.
func fullSnapshot() Snapshot {
	guest := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	att := attestation()
	return Snapshot{
		Protocol: 1,
		Epoch:    epoch7(),
		Seq:      42,
		Node:     "node2",
		State:    Active,
		Settings: Settings{
			PollInterval:     30 * time.Second,
			IdentityMinimum:  "filtered",
			TrustStatic:      true,
			TrustedCIDRs:     []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
			ReverifyInterval: 10 * time.Minute,
		},
		Nodes: []Node{
			{Name: "node2", Addr: addr("192.168.4.12"), Online: true, Local: true, Addrs: []netip.Addr{addr("192.168.4.12"), addr("10.10.0.2")}},
			{Name: "node1", Addr: addr("192.168.4.11"), Online: true, Addrs: []netip.Addr{addr("192.168.4.11"), addr("10.10.0.1")}},
		},
		Deny: DenyList{
			Denied:  []netip.Addr{addr("192.168.4.11"), addr("10.10.0.1"), addr("192.168.4.12"), addr("10.10.0.2")},
			Reasons: map[string]string{"10.0.0.1": "gateway of SDN subnet 10.0.0.0/24 of vnet lab"},
			Soft:    map[string]string{"10.0.0.40": "a guest of another install had it"},
		},
		Guests: []model.Guest{
			{
				Ref:      model.GuestRef{Kind: model.KindLXC, VMID: 202},
				Name:     "clone",
				Node:     "node2",
				Running:  true,
				Identity: "77c0",
				NICs:     []model.NIC{{Index: 0, MAC: "bc:24:11:aa:bb:01", Bridge: "vmbr0", VLAN: 20}},
			},
		},
		Prune: true,
		Tunnels: []Tunnel{
			{
				AccountID: "0123456789abcdef0123456789abcdef",
				TunnelID:  sharedID,
				Name:      "pco-0123456789ab",
				Kind:      TunnelShared,
				Run:       true,
				Token:     NewSecret(sharedToken),
				Verified:  []netip.AddrPort{netip.MustParseAddrPort("10.0.0.21:443"), netip.MustParseAddrPort("10.0.0.22:8080")},
			},
			{
				AccountID: "0123456789abcdef0123456789abcdef",
				TunnelID:  pinnedID,
				Name:      "pco-0123456789ab-node2",
				Kind:      TunnelPinned,
				Run:       true,
				Token:     NewSecret(pinnedToken),
				// Only the wire is pinned here: an agent stops a
				// connector only of a tunnel it is not told to run.
				Stop: true,
			},
		},
		Targets: []Target{
			{
				Key:     "app.example.com",
				Tunnels: []string{sharedID},
				Route: model.Route{
					Hostname: "app.example.com",
					Target:   model.Target{Scheme: model.SchemeHTTPS, Port: 443},
					Options:  model.RouteOptions{NoTLSVerify: true, SNI: "app.internal"},
					Source:   model.SourceAnnotation,
					Guest:    &guest,
				},
				Guest: &model.Guest{
					Ref:      guest,
					Name:     "app",
					Node:     "node1",
					Running:  true,
					Identity: "a3f1",
					NICs:     []model.NIC{{Index: 1, MAC: "bc:24:11:aa:bb:01", Bridge: "vmbr0", VLAN: 20}},
				},
				Addr:          addr("10.0.0.21"),
				Service:       addr("10.203.118.21"),
				Port:          443,
				HostingNode:   "node1",
				MACs:          []string{"bc:24:11:aa:bb:01"},
				Required:      "port",
				Resolve:       true,
				Attestation:   &att,
				AttestedFresh: true,
				Covered:       true,
				AllowNode:     true,
			},
		},
		Networks: json.RawMessage(`[{"bridge":"vmbr1","id":"lan","kind":"leg"}]`),
		Blocks: []Block{
			{Addr: addr("10.0.0.66"), Reason: BlockConflict, Until: t0.Add(10 * time.Minute)},
			{Addr: addr("10.0.0.67"), Reason: BlockAdmin},
		},
		Actions: []Action{
			{ID: "a1", Kind: ActionPVEWrite, Args: json.RawMessage(`{"digest":"9f86d081","guest":{"kind":"qemu","vmid":101},"linkDown":true,"nic":1}`)},
		},
		Endpoints: []string{"192.168.4.11:8644"},
	}
}

func fullStatus() Status {
	att := attestation()
	owner := model.GuestRef{Kind: model.KindLXC, VMID: 202}
	return Status{
		Protocol:          Range{Min: 1, Max: 1},
		Build:             "0.9.0",
		Node:              "node2",
		Kind:              pki.KindAppliance,
		VMID:              9005,
		AgentStartedAt:    t0.Add(-time.Hour),
		WatchRunningSince: t0.Add(-50 * time.Minute),
		EpochSeen:         epoch7(),
		SeqApplied:        42,
		Targets: []TargetReport{
			{
				Key:         "app.example.com",
				Addr:        addr("10.0.0.21"),
				Level:       "port",
				Attested:    true,
				AttestedBy:  "node1",
				Attestation: &att,
				OK:          true,
				Reason:      "the forwarding table of node1 names the port of the guest",
				VerifiedAt:  t0.Add(-5 * time.Second),
				InEgressSet: true,
				Result:      json.RawMessage(`{"addr":"10.0.0.21","level":"port"}`),
			},
		},
		Attestations: []Attestation{att},
		Observations: []Observation{
			{Kind: ObservationConflict, MAC: "bc:24:11:aa:bb:01", Bridge: "vmbr0", VLAN: 20, From: "tap101i1", Port: "veth202i0", Owner: &owner, At: t0.Add(-time.Minute)},
		},
		Connectors: []connector.Status{
			{TunnelID: sharedID, Active: true, Ready: true, Connections: 4, ConnectorID: "e1f2", MetricsAddr: "127.0.0.1:20241", Install: "0123456789ab", TokenRefused: true, MetricsPortHeld: true},
		},
		Egress:       "on",
		LocalBlocks:  []netip.Addr{addr("10.0.0.99")},
		ServiceAddrs: []netip.Addr{addr("10.203.118.21")},
		Addrs:        []netip.Addr{addr("192.168.4.12"), addr("10.10.0.2")},
		Clock:        t0,
		Events:       []json.RawMessage{json.RawMessage(`{"at":"2026-10-05T11:59:00Z","kind":"route","message":"app.example.com is served"}`)},
		EventCursor:  1200,
		Resend:       240,
		Actions: []ActionResult{
			{ID: "a1", OK: true, Data: json.RawMessage(`{"argv":["qm","set","101"]}`)},
			{ID: "a2", Error: "the guest is not on this node"},
		},
		Problems: []string{"the forwarding-table watch is not running"},
		Legs:     []LegReport{{ID: "vmbr1", Up: true}, {ID: "vmbr2.20", Problem: "10.20.0.250 is answered by bc:24:11:00:00:09"}},
		Traffic: []TunnelTraffic{{
			TunnelID: "11111111-1111-4111-8111-111111111111", Cloudflared: "2026.10.0", ConfigVersion: 3, HAConnections: 4,
			Edges:     []connector.Edge{{Connection: 0, Location: "prg01"}, {Connection: 1, Location: "vie06"}},
			RTTMillis: []float64{4.5, 11},
			Stale:     true,
			Samples:   []TrafficSample{{At: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), RPS: 1.5, ErrorsPerSec: 0.25, Concurrent: 2}},
		}},
	}
}

func fullProbe() Probe {
	return Probe{Bridge: "vmbr2", VLAN: 20, Addr: addr("10.20.0.250")}
}

func fullProbeAnswer() ProbeAnswer {
	return ProbeAnswer{
		Interfaces: []ProbeIface{
			{Name: "vmbr2", Bridge: "vmbr2", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.4.12/24")}},
			{Name: "eno2", Master: "vmbr2"},
			{Name: "vmbr2.20", Bridge: "vmbr2", VLAN: 20, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.1/24")}},
			{Name: "leg1", Master: "vmbr1", Bridge: "vmbr1", Via: "vmbr1"},
		},
		AnsweredBy: []string{"bc:24:11:00:00:09"},
		NotProbed:  "the gateway namespace is not up",
	}
}

func fullHeartbeat() Heartbeat {
	return Heartbeat{
		Epoch:         epoch7(),
		LeaseValid:    true,
		LeaseValidFor: 55 * time.Second,
		Seq:           42,
		Nodes: map[string]NodeFreshness{
			"node1": {StatusAge: 2 * time.Second, WatchRunning: true},
			"node2": {StatusAge: 31 * time.Second},
		},
		ReadyReplicas: map[string]int{sharedID: 2, pinnedID: 1},
		EventAck:      1200,
	}
}

func fullAgentView() AgentView {
	return AgentView{
		Node:            "node2",
		Kind:            pki.KindHost,
		Controller:      "node1",
		Remote:          true,
		SilentFor:       3 * time.Second,
		Epoch:           epoch7(),
		Seq:             42,
		SeqAge:          4 * time.Second,
		Connectors:      2,
		EgressTargets:   3,
		StoodDown:       "this node was removed from install 0123456789ab at 2026-10-05 11:00",
		CertFingerprint: "3b4c1d2e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00",
		CertExpires:     t0.Add(90 * 24 * time.Hour),
		Heard:           true,
		ReadyReplicas:   map[string]int{sharedID: 2},
		HeartbeatAge:    time.Second,
		Problems:        []string{"the controller is silent"},
	}
}

func fullPVEWrite() PVEWrite {
	return PVEWrite{
		Guest:    model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		NIC:      1,
		Bridge:   "vmbr1",
		Tag:      20,
		MAC:      "06:50:43:4f:00:15",
		Name:     "eth1",
		IP:       netip.MustParsePrefix("10.203.118.21/24"),
		Firewall: yes(),
		LinkDown: yes(),
		Remove:   true,
		Digest:   "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b",
	}
}

func fullRefusal() Refusal {
	return Refusal{Code: RefusalRemoved, Message: "this node was removed from install 0123456789ab"}
}
