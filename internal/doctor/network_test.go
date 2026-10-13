package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeNetEnv is a host that also answers for the gateway and the managed
// network.
type fakeNetEnv struct {
	*fakeEnv
	pinnedErr error
	props     map[string]string // "<unit> <property>" -> value
	propErr   error
	sdn       pve.SDN
	sdnErr    error
	ipv6      bool
	ipv6Err   error
	tableErr  error
	used      int
	size      int
	poolErr   error
	pins      []store.Pin
	pinsErr   error
}

func healthyNetEnv() *fakeNetEnv {
	env := healthyEnv()
	env.enabled = map[string]bool{netnsUnit: true}
	return &fakeNetEnv{
		fakeEnv: env,
		props:   map[string]string{daemonUnit + " After": "network-online.target pve-cluster.service " + netnsUnit + " basic.target"},
		used:    3, size: 65280,
	}
}

func (f *fakeNetEnv) NetnsPinned(context.Context) error { return f.pinnedErr }

func (f *fakeNetEnv) UnitProperty(_ context.Context, unit, property string) (string, error) {
	return f.props[unit+" "+property], f.propErr
}

func (f *fakeNetEnv) SDNPending(context.Context) (pve.SDN, error) { return f.sdn, f.sdnErr }
func (f *fakeNetEnv) PconetIPv6(context.Context) (bool, error)    { return f.ipv6, f.ipv6Err }
func (f *fakeNetEnv) HostTable(context.Context) error             { return f.tableErr }

func (f *fakeNetEnv) ServicePool(context.Context) (used, size int, err error) {
	return f.used, f.size, f.poolErr
}

func (f *fakeNetEnv) Pins(context.Context) ([]store.Pin, time.Time, error) {
	return f.pins, now, f.pinsErr
}

var (
	legAddr     = netip.MustParseAddr("10.50.0.250")
	managedLeg  = netip.MustParseAddr("10.203.118.1")
	managedNet  = netip.MustParsePrefix("10.203.118.0/24")
	guestQemu   = model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	guestLXC    = model.GuestRef{Kind: model.KindLXC, VMID: 202}
	networkLeg1 = engine.NetworkView{
		Network: store.Network{
			ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.PrefixFrom(legAddr, 24), Mark: 1,
			Legs: map[string]netip.Addr{"pve1": legAddr},
		},
		Legs: []engine.LegView{{Node: "pve1", Addr: legAddr, Up: true}}, Targets: 1, Routes: 1,
	}
)

// attachedState is a healthy host with one attached network.
func attachedState() engine.State {
	st := healthyState()
	st.Node = "pve1"
	st.Profile = store.ProfileHost
	st.Networks = []engine.NetworkView{networkLeg1}
	return st
}

// managedState is a healthy install with an attached network and the managed
// network, with one guest attached to it.
func managedState(profile string) engine.State {
	st := attachedState()
	st.Profile = profile
	st.Networks = append(st.Networks, engine.NetworkView{
		Network: store.Network{
			ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge, Prefix: netip.PrefixFrom(managedLeg, 24),
			Mark: 2, Legs: map[string]netip.Addr{"pve1": managedLeg},
		},
		Legs: []engine.LegView{{Node: "pve1", Addr: managedLeg, Up: true}},
	})
	st.Managed = engine.ManagedView{
		Enabled: true, Consent: store.ConsentPerGuest, TagHonoured: true, Subnet: managedNet,
		Leg:  &engine.LegView{Node: "pve1", Addr: managedLeg, Up: true},
		Free: 250,
		Vnet: engine.VnetView{OK: true, ZoneType: "simple", IsolatePorts: profile == store.ProfileHost},
		DHCP: &engine.DHCPView{Bound: true, Since: now.Add(-time.Hour)},
		Attachments: []engine.AttachmentView{{
			Attachment: store.Attachment{Owner: guestLXC.String(), NIC: 1, MAC: "06:50:43:4f:76:02", Addr: netip.MustParseAddr("10.203.118.2"), Consent: "tag"},
			Guest:      &engine.GuestView{GuestRef: guestLXC, Name: "wiki"},
			State:      engine.AttachmentAttached,
		}},
		Waiting: []engine.ManagedWaiting{}, Foreign: []engine.ForeignView{},
	}
	if profile == store.ProfileAppliance {
		st.Identity = &engine.IdentityView{VMID: testVMID, Node: "pve1", OK: true, CheckedAt: now.Add(-5 * time.Second)}
		st.Managed.Filtered = &engine.FilteredView{Holds: true, Backend: "pve-firewall", Offered: true}
	}
	return st
}

// networkCheckKinds are the first words of the checks of the networks.
var networkCheckKinds = []string{
	"dhcp", "filtered", "foreign", "leg", "managed", "netns", "networks", "pconet", "pool", "service", "vnet",
}

// networkFindings are the findings of the checks of the networks.
func networkFindings(findings []Finding) []Finding {
	return slices.DeleteFunc(slices.Clone(findings), func(f Finding) bool {
		kind, _, _ := strings.Cut(f.Check, " ")
		return !slices.Contains(networkCheckKinds, kind)
	})
}

// netFinding runs the doctor on a host with networks, changed by the two
// functions, and returns the finding of one check.
func netFinding(t *testing.T, check string, st engine.State, env *fakeNetEnv) Finding {
	t.Helper()
	findings := Run(t.Context(), st, env)
	i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == check })
	require.GreaterOrEqual(t, i, 0, "no finding of %q in %v", check, networkFindings(findings))
	return findings[i]
}

// A node that reaches every guest directly has no network to check, and no
// line of them.
func TestADirectInstallHasNoChecksOfTheNetworks(t *testing.T) {
	env := healthyNetEnv()
	env.enabled = nil

	require.Empty(t, networkFindings(Run(t.Context(), healthyState(), env)))
}

func TestTheChecksOfAHealthyHostWithNetworks(t *testing.T) {
	findings := Run(t.Context(), managedState(store.ProfileHost), healthyNetEnv())

	require.Equal(t, []Finding{
		{Check: "dhcp", Level: LevelOK, Detail: "bound since 2026-10-01T11:00:00Z; no request from a MAC without a lease"},
		{Check: "leg pconet", Level: LevelOK, Detail: "leg2 is up with 10.203.118.1 on pconet"},
		{Check: "leg vmbr1", Level: LevelOK, Detail: "leg1 is up with 10.50.0.250 on vmbr1"},
		{Check: "managed", Level: LevelOK, Detail: "consent per-guest, the tag cf-tunnel-managed is honoured; 1 guest attached; 250 addresses of 10.203.118.0/24 free"},
		{Check: "netns", Level: LevelOK, Detail: "the gateway namespace pco is pinned and as pco left it, and pco-netns.service starts it at boot before pco"},
		{Check: "networks", Level: LevelOK, Detail: "2 of the 15 networks a node reaches are attached"},
		{Check: "pconet", Level: LevelOK, Detail: "IPv6 is off on the bridge pconet"},
		{Check: "service pool", Level: LevelOK, Detail: "3 of 65280 service addresses given out (0 %)"},
		{Check: "vnet", Level: LevelOK, Detail: "zone pco of type simple, isolate-ports on, no subnet; no change of the SDN waits to be applied"},
	}, networkFindings(findings))
}

func TestTheChecksOfAHealthyApplianceWithTheManagedNetwork(t *testing.T) {
	env := healthyApplianceEnv()
	env.app.resources = []pve.Resource{{Kind: model.KindLXC, VMID: 202, Node: "pve1", Pool: "managed"}}
	st := managedState(store.ProfileAppliance)
	st.Managed.PoolGrant = "managed"

	findings := Run(t.Context(), st, env)

	require.Equal(t, []Finding{
		{Check: "dhcp", Level: LevelOK, Detail: "bound since 2026-10-01T11:00:00Z; no request from a MAC without a lease"},
		{Check: "filtered pconet", Level: LevelOK, Detail: "the guests on pconet are isolated by their firewall (pve-firewall)"},
		{Check: "leg pconet", Level: LevelOK, Detail: "leg2 is up with 10.203.118.1 on pconet"},
		{Check: "leg vmbr1", Level: LevelOK, Detail: "leg1 is up with 10.50.0.250 on vmbr1"},
		{Check: "managed", Level: LevelOK, Detail: "consent per-guest, the tag cf-tunnel-managed is honoured; 1 guest attached; 250 addresses of 10.203.118.0/24 free"},
		{Check: "managed firewall", Level: LevelOK, Detail: "the datacenter firewall is on, and pve-firewall runs on the node on the iptables backend"},
		{Check: "networks", Level: LevelOK, Detail: "2 of the 15 networks a node reaches are attached"},
		{Check: "pool grant", Level: LevelOK, Detail: "every guest of pool managed has consent for the managed network"},
		{Check: "vnet", Level: LevelOK, Detail: "zone pco of type simple, isolate-ports off, no subnet"},
	}, networkFindings(findings), "the fake appliance answers nothing of the gateway")
}

func TestTheNetnsCheck(t *testing.T) {
	const restart = "systemctl restart pco-netns"
	for _, tt := range []struct {
		name   string
		state  func(*engine.State)
		env    func(*fakeNetEnv)
		expect Finding
	}{
		{name: "not pinned", env: func(e *fakeNetEnv) { e.pinnedErr = errors.New("/run/netns/pco does not exist") },
			expect: fail("netns", "the gateway namespace pco is not pinned (/run/netns/pco does not exist): the legs and the service addresses are gone", restart)},
		{name: "could not be brought back", state: func(st *engine.State) {
			st.Problems = []string{"the gateway namespace was changed outside pco and could not be brought back: the gateway namespace is not pinned; systemctl restart pco-netns"}
		}, expect: fail("netns", "the gateway namespace was changed outside pco and could not be brought back: the gateway namespace is not pinned; systemctl restart pco-netns", restart)},
		{name: "could not be checked", state: func(st *engine.State) {
			st.Problems = []string{"checking the gateway namespace: netlink: permission denied"}
		}, expect: fail("netns", "checking the gateway namespace: netlink: permission denied", restart)},
		{name: "the host table is missing", env: func(e *fakeNetEnv) { e.tableErr = gateway.ErrNotLoaded },
			expect: fail("netns", "the table inet pco_host is not loaded: the node's own services are not kept from the managed network and the transit link", restart)},
		{name: "the host table cannot be read", env: func(e *fakeNetEnv) { e.tableErr = errors.New("nft: exit status 1") },
			expect: warn("netns", "the table inet pco_host could not be read: nft: exit status 1", "nft list table inet pco_host")},
		{name: "a stop action", env: func(e *fakeNetEnv) {
			e.props[netnsUnit+" ExecStop"] = "{ path=/usr/bin/pco ; argv[]=/usr/bin/pco netns down }"
		},
			expect: fail("netns", "the loaded pco-netns.service has an ExecStop line: an old pco-netns.service tears the gateway down on restart",
				"reinstall the package, then systemctl daemon-reload")},
		{name: "not enabled", env: func(e *fakeNetEnv) { e.enabled = nil },
			expect: warn("netns", "pco-netns.service is not enabled: after a boot no gateway is up for the guests behind the legs", "systemctl enable pco-netns")},
		{name: "systemd does not say", env: func(e *fakeNetEnv) { e.enabledErr = errors.New("bus timeout") },
			expect: warn("netns", "systemd did not say whether pco-netns.service is enabled: bus timeout", "systemctl is-enabled pco-netns.service")},
		{name: "pco.service from before the upgrade", env: func(e *fakeNetEnv) { e.props[daemonUnit+" After"] = "network-online.target pve-cluster.service" },
			expect: warn("netns", "the loaded pco.service is not ordered after pco-netns.service: a unit file from before an upgrade",
				"systemctl daemon-reload; reinstall the package if it stays")},
		{name: "the order cannot be read", env: func(e *fakeNetEnv) { e.propErr = errors.New("bus timeout") },
			expect: warn("netns", "systemd did not say how pco-netns.service and pco.service are loaded: bus timeout", "systemctl show pco-netns.service pco.service")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := attachedState(), healthyNetEnv()
			if tt.state != nil {
				tt.state(&st)
			}
			if tt.env != nil {
				tt.env(env)
			}
			require.Equal(t, tt.expect, netFinding(t, "netns", st, env))
		})
	}
}

// A namespace unit that is enabled on a node with no network yet is only
// looked at for its unit files.
func TestTheNetnsCheckWithoutANetwork(t *testing.T) {
	env := healthyNetEnv()
	env.pinnedErr = errors.New("/run/netns/pco does not exist")

	require.Equal(t, ok("netns", "pco-netns.service is enabled and starts before pco; no network is attached"),
		netFinding(t, "netns", healthyState(), env))
}

func TestTheLegCheck(t *testing.T) {
	for _, tt := range []struct {
		name   string
		leg    engine.LegView
		expect Finding
	}{
		{"down", engine.LegView{Node: "pve1", Addr: legAddr, Problem: "address in use by 52:54:00:12:34:56"},
			fail("leg vmbr1", "leg1 on vmbr1 is not up: address in use by 52:54:00:12:34:56", "pco network show vmbr1")},
		{"up with a problem", engine.LegView{Node: "pve1", Addr: legAddr, Up: true, Problem: "no carrier, DAD not run"},
			warn("leg vmbr1", "leg1 is up with 10.50.0.250 on vmbr1, but no carrier, DAD not run", "pco network show vmbr1")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := attachedState()
			st.Networks[0].Legs = []engine.LegView{tt.leg}
			require.Equal(t, tt.expect, netFinding(t, "leg vmbr1", st, healthyNetEnv()))
		})
	}
}

// A network whose leg is on another node has nothing to check here.
func TestALegOnAnotherNodeIsNotChecked(t *testing.T) {
	st := attachedState()
	st.Networks[0].Legs = []engine.LegView{}

	findings := Run(t.Context(), st, healthyNetEnv())

	require.False(t, slices.ContainsFunc(findings, func(f Finding) bool { return f.Check == "leg vmbr1" }))
}

func TestTheLegOfTheManagedNetworkIsFixedFromItsStatus(t *testing.T) {
	st := managedState(store.ProfileHost)
	st.Networks[1].Legs[0].Up, st.Networks[1].Legs[0].Problem = false, "bridge pconet is absent"

	require.Equal(t, fail("leg pconet", "leg2 on pconet is not up: bridge pconet is absent", "pco managed status"),
		netFinding(t, "leg pconet", st, healthyNetEnv()))
}

func TestTheNetworksCheckWarnsNearTheLimit(t *testing.T) {
	st := attachedState()
	for i := 2; i <= gateway.MaxMark-2; i++ {
		n := networkLeg1
		n.ID, n.Mark, n.Legs = fmt.Sprintf("vmbr%d", i), i, []engine.LegView{}
		st.Networks = append(st.Networks, n)
	}

	require.Equal(t, warn("networks", "13 of the 15 networks a node reaches are attached",
		"detach a network that serves nothing: pco network list shows what each serves"), netFinding(t, "networks", st, healthyNetEnv()))
}

func TestTheServicePoolCheck(t *testing.T) {
	const fix = "a target behind a leg keeps its service address while its address stays; " +
		"find the targets whose addresses change often, since no address is given twice"
	for _, tt := range []struct {
		name   string
		used   int
		err    error
		expect Finding
	}{
		{"below 80 %", 52223, nil, ok("service pool", "52223 of 65280 service addresses given out (79 %)")},
		{"80 %", 52224, nil, warn("service pool", "52224 of 65280 service addresses given out (80 %); when none is left, a new target behind a leg is held", fix)},
		{"95 %", 62016, nil, fail("service pool", "62016 of 65280 service addresses given out (95 %); when none is left, a new target behind a leg is held", fix)},
		{"unreadable", 0, errors.New("pmxcfs: timeout"), warn("service pool", "the pool of service addresses could not be read: pmxcfs: timeout", fixProblems)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyNetEnv()
			env.used, env.poolErr = tt.used, tt.err
			require.Equal(t, tt.expect, netFinding(t, "service pool", attachedState(), env))
		})
	}
}

func TestThePconetCheck(t *testing.T) {
	for _, tt := range []struct {
		name   string
		env    func(*fakeNetEnv)
		expect Finding
	}{
		{"IPv6 on", func(e *fakeNetEnv) { e.ipv6 = true },
			fail("pconet", "IPv6 is on on the bridge pconet: managed guests reach the node at its link-local address", "systemctl restart pco-netns")},
		{"unreadable", func(e *fakeNetEnv) { e.ipv6Err = errors.New("permission denied") },
			warn("pconet", "whether IPv6 is off on the bridge pconet could not be read: permission denied", "sysctl net.ipv6.conf.pconet.disable_ipv6")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyNetEnv()
			tt.env(env)
			require.Equal(t, tt.expect, netFinding(t, "pconet", managedState(store.ProfileHost), env))
		})
	}
}

func TestTheManagedCheck(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*engine.ManagedView)
		expect Finding
	}{
		{"refused", func(m *engine.ManagedView) {
			m.Refused = "the datacenter firewall is off; no guest rule is in force until it is turned on"
		},
			fail("managed", "the managed network cannot run on this node: the datacenter firewall is off; no guest rule is in force until it is turned on; "+
				"nothing is attached until it can, and attached guests keep their cards", "put right what it names; pco managed status shows it")},
		{"the leg is down", func(m *engine.ManagedView) { m.Leg.Up, m.Leg.Problem = false, "bridge pconet is absent" },
			fail("managed", "the leg of the managed network is not up: bridge pconet is absent", "pco managed status")},
		{"automatic attaches stopped", func(m *engine.ManagedView) {
			m.Consent = store.ConsentAuto
			m.AutoStopped = "managed.consent is auto but the gate tag cf-tunnel is not registered as admin-only; automatic attaches are stopped; attached guests stay attached"
		}, warn("managed", "managed.consent is auto but the gate tag cf-tunnel is not registered as admin-only; automatic attaches are stopped; attached guests stay attached",
			"add the gate tag to registered-tags in the options of the datacenter")},
		{"the consent tag not registered", func(m *engine.ManagedView) { m.TagHonoured = false },
			warn("managed", "the tag cf-tunnel-managed is not registered as admin-only, so it gives no guest consent; only approvals do",
				"add cf-tunnel-managed to registered-tags in the options of the datacenter, or approve guests with pco managed approve")},
		{"few addresses", func(m *engine.ManagedView) { m.Free = 9 },
			warn("managed", "9 addresses of 10.203.118.0/24 are free", "detach the guests that no longer need a card on the managed network")},
		{"automatic, with the tag not registered", func(m *engine.ManagedView) { m.Consent, m.TagHonoured, m.AutoAllowed = store.ConsentAuto, false, true },
			ok("managed", "consent auto, the gate tag is registered; 1 guest attached; 250 addresses of 10.203.118.0/24 free")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := managedState(store.ProfileHost)
			tt.change(&st.Managed)
			require.Equal(t, tt.expect, netFinding(t, "managed", st, healthyNetEnv()))
		})
	}
}

func TestTheVnetCheck(t *testing.T) {
	const fixVnet = "put pconet back as pco made it: zone pco of type simple, isolate-ports on, no subnet; pco managed status shows what differs"
	for _, tt := range []struct {
		name   string
		change func(*engine.State, *fakeNetEnv)
		expect Finding
	}{
		{"changed", func(st *engine.State, _ *fakeNetEnv) {
			st.Managed.Vnet = engine.VnetView{ZoneType: "simple", Subnets: 1, Why: []string{"it has a subnet", "isolate-ports is off"}}
		}, fail("vnet", "the vnet pconet was changed outside pco: it has a subnet; isolate-ports is off; nothing is attached until it is as pco made it", fixVnet)},
		{"changes of others pending", func(_ *engine.State, e *fakeNetEnv) {
			e.sdn = pve.SDN{
				Zones:   []pve.SDNZone{{Name: "pco", Type: "simple", State: pve.SDNChanged}, {Name: "lab", Type: "vlan", State: pve.SDNNew}},
				Vnets:   []pve.SDNVnet{{Name: "pconet", Zone: "pco", State: pve.SDNChanged}, {Name: "lab10", Zone: "lab", Tag: 10, State: pve.SDNNew}},
				Subnets: []pve.SDNSubnet{{Vnet: "pconet", CIDR: "10.1.0.0/24", State: pve.SDNNew}},
			}
		}, warn("vnet", "the SDN has changes that are not applied and are not pco's: zone lab (new); vnet lab10 (new); the next apply, on any node, applies them along",
			"apply them, or undo them, in Datacenter > SDN")},
		{"the pending changes unreadable", func(_ *engine.State, e *fakeNetEnv) { e.sdnErr = errors.New("403 Forbidden") },
			warn("vnet", "the changes of the SDN that wait to be applied could not be read: 403 Forbidden", "the proxmox check says why Proxmox does not answer")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := managedState(store.ProfileHost), healthyNetEnv()
			tt.change(&st, env)
			require.Equal(t, tt.expect, netFinding(t, "vnet", st, env))
		})
	}
}

func TestTheDHCPCheck(t *testing.T) {
	for _, tt := range []struct {
		name   string
		dhcp   *engine.DHCPView
		expect Finding
	}{
		{"not bound", &engine.DHCPView{}, fail("dhcp", "the DHCP server of the managed network is not bound: virtual machines on pconet get no address",
			"journalctl -u pco says why; systemctl restart pco")},
		{"not started", nil, warn("dhcp", "the DHCP server of the managed network has not said how it is", "journalctl -u pco says why it does not start")},
		{"requests of unknown MACs", &engine.DHCPView{Bound: true, Since: now.Add(-time.Hour), Unknown: 3},
			ok("dhcp", "bound since 2026-10-01T11:00:00Z; 3 requests from MACs without a lease since the start, not answered")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := managedState(store.ProfileHost)
			st.Managed.DHCP = tt.dhcp
			require.Equal(t, tt.expect, netFinding(t, "dhcp", st, healthyNetEnv()))
		})
	}
}

func TestTheManagedGuestChecks(t *testing.T) {
	detachAt := now.Add(8 * time.Minute)
	for _, tt := range []struct {
		name   string
		change func(*engine.ManagedView)
		expect Finding
	}{
		{"pending reboot", func(m *engine.ManagedView) {
			m.Attachments[0].State, m.Attachments[0].Detail = engine.AttachmentPendingReboot, "waits for the restart of the guest"
		}, warn("managed guest lxc/202", "lxc/202 (wiki): net1 is added and takes effect at the next restart of the guest",
			"restart the guest: a virtual machine without network hotplug takes a new card only then")},
		{"awaiting DHCP", func(m *engine.ManagedView) { m.Attachments[0].State = engine.AttachmentAwaitingDHCP },
			warn("managed guest lxc/202", "lxc/202 (wiki): net1 waits for the guest to ask DHCP for 10.203.118.2",
				"have the guest ask DHCP on that card, as its distribution configures it")},
		{"needs a grant", func(m *engine.ManagedView) {
			m.Attachments[0].State = engine.AttachmentNeedsGrant
			m.Attachments[0].GrantCmd = "pveum acl modify /vms/202 --users pco@pve --tokens 'pco@pve!vm9240' --roles PCOManaged"
		}, warn("managed guest lxc/202", "lxc/202 (wiki): pco's token may not change the cards of the guest",
			"pveum acl modify /vms/202 --users pco@pve --tokens 'pco@pve!vm9240' --roles PCOManaged")},
		{"detaching", func(m *engine.ManagedView) {
			m.Attachments[0].DetachAt = &detachAt
			m.Attachments[0].State = engine.AttachmentDetaching
			m.Attachments[0].Detail = "goes at 2026-10-01T12:08:00Z: the tag cf-tunnel-managed was removed"
		}, warn("managed guest lxc/202", "lxc/202 (wiki): net1 goes at 2026-10-01T12:08:00Z: the tag cf-tunnel-managed was removed",
			"give the guest its consent back before then to keep the card")},
		{"detach pending", func(m *engine.ManagedView) {
			m.Attachments[0].State = engine.AttachmentDetachPending
			m.Attachments[0].Detail = "the tag cf-tunnel-managed was removed; the card is removed once the configuration no longer lists it"
		}, warn("managed guest lxc/202", "lxc/202 (wiki): net1: the tag cf-tunnel-managed was removed; the card is removed once the configuration no longer lists it",
			"restart the guest: a virtual machine without network hotplug drops the card only then")},
		{"unverified", func(m *engine.ManagedView) {
			m.Attachments[0].State, m.Attachments[0].Detail = engine.AttachmentUnverified, "pct set: timeout"
		}, warn("managed guest lxc/202", "lxc/202 (wiki): net1 could not be added or checked in the last cycle: pct set: timeout", fixProblems)},
		{"migrated", func(m *engine.ManagedView) {
			m.Attachments[0].State, m.Attachments[0].Detail = engine.AttachmentMigrated, "on node pve2"
		}, fail("managed guest lxc/202", "lxc/202 (wiki) is on node pve2, not on this node: it keeps its card and its address, and the managed network does not reach it there",
			"migrate it back: a guest on the managed network cannot move to another node yet")},
		{"waits for a grant", func(m *engine.ManagedView) {
			m.Attachments = nil
			m.Waiting = []engine.ManagedWaiting{{GuestView: engine.GuestView{GuestRef: guestLXC, Name: "wiki"},
				Why: "needs-grant: pveum acl modify /vms/202 --users pco@pve --tokens 'pco@pve!vm9240' --roles PCOManaged"}}
		}, warn("managed guest lxc/202", "lxc/202 (wiki) waits for a card: pco's token may not change the cards of the guest",
			"pveum acl modify /vms/202 --users pco@pve --tokens 'pco@pve!vm9240' --roles PCOManaged")},
		{"waits for the firewall confirmation", func(m *engine.ManagedView) {
			m.Attachments = nil
			m.Waiting = []engine.ManagedWaiting{{GuestView: engine.GuestView{GuestRef: guestLXC, Name: "wiki"},
				Why: "needs-firewall-confirmation: turning on the firewall of lxc/202 (wiki) also filters net0 (firewall=1); " +
					"run pco managed approve lxc/202 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest"}}
		}, warn("managed guest lxc/202", "lxc/202 (wiki) waits for a card: turning on the firewall of lxc/202 (wiki) also filters net0 (firewall=1)",
			"pco managed approve lxc/202 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := managedState(store.ProfileHost)
			tt.change(&st.Managed)
			require.Equal(t, tt.expect, netFinding(t, "managed guest lxc/202", st, healthyNetEnv()))
		})
	}
}

// A guest that waits for anything but a grant or a confirmation, as one
// without consent, is no finding: pco managed status lists them.
func TestOtherWaitsAreNoFinding(t *testing.T) {
	st := managedState(store.ProfileHost)
	st.Managed.Waiting = []engine.ManagedWaiting{{GuestView: engine.GuestView{GuestRef: guestQemu}, Why: "guest is on node pve2, not pve1"}}

	findings := networkFindings(Run(t.Context(), st, healthyNetEnv()))

	require.False(t, slices.ContainsFunc(findings, func(f Finding) bool { return strings.HasPrefix(f.Check, "managed guest") }))
}

func TestTheForeignCardChecks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		action string
		expect *Finding
	}{
		{"reported", engine.ForeignReported, &Finding{Check: "foreign card qemu/101", Level: LevelFail,
			Detail: "net2 (bc:24:11:00:00:09) of qemu/101 (db) is on pconet and no attachment explains it: pco may not change the guest",
			Fix:    "take the card off the guest, or grant pco role PCOManaged on it so that pco sets it link down"}},
		{"set link down", engine.ForeignLinkDown, &Finding{Check: "foreign card qemu/101", Level: LevelWarn,
			Detail: "net2 (bc:24:11:00:00:09) of qemu/101 (db) is on pconet and no attachment explains it: it was set link down",
			Fix:    "take the card off the guest; with consent, pco replaces it by a card of its own"}},
		{"replaced", engine.ForeignReplaced, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := managedState(store.ProfileHost)
			st.Managed.Foreign = []engine.ForeignView{{GuestView: engine.GuestView{GuestRef: guestQemu, Name: "db"}, NIC: 2, MAC: "bc:24:11:00:00:09",
				Action: tt.action, Why: "pco may not change the guest"}}
			if tt.action == engine.ForeignLinkDown {
				st.Managed.Foreign[0].Why = "it was set link down"
			}

			findings := Run(t.Context(), st, healthyNetEnv())

			i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == "foreign card qemu/101" })
			if tt.expect == nil {
				require.Negative(t, i)
				return
			}
			require.GreaterOrEqual(t, i, 0)
			require.Equal(t, *tt.expect, findings[i])
		})
	}
}

func TestTheFilteredCheckOfTheAppliance(t *testing.T) {
	for _, tt := range []struct {
		name     string
		filtered *engine.FilteredView
		expect   Finding
	}{
		{"not offered", &engine.FilteredView{Backend: "proxmox-firewall", Why: []string{"the nftables backend installs no guest rules", "the second"}},
			warn("filtered pconet", "the filtered level is not offered on this node: the nftables backend installs no guest rules; the routes on pconet are proven at observed",
				"set nftables: 0 in the firewall options of the node")},
		{"does not hold", &engine.FilteredView{Backend: "pve-firewall", Offered: true, Why: []string{"one", "two", "three", "four"}},
			fail("filtered pconet", "the guests on pconet are not proven isolated: one; two; three; the routes on pconet are proven at observed",
				"pco managed status lists every reason")},
		{"not judged", nil, warn("filtered pconet", "not judged yet", "wait for the next cycle")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env, st := healthyApplianceEnv(), managedState(store.ProfileAppliance)
			st.Managed.Filtered = tt.filtered
			require.Equal(t, tt.expect, findingOf(t, Run(t.Context(), st, env), "filtered pconet"))
		})
	}
}

func TestTheManagedFirewallCheckOfTheAppliance(t *testing.T) {
	env, st := healthyApplianceEnv(), managedState(store.ProfileAppliance)
	st.Managed.Refused = "pve-firewall is stopped on node pve1; no guest rule is in force until it runs"

	require.Equal(t, fail("managed firewall", "pve-firewall is stopped on node pve1; no guest rule is in force until it runs; the routes on pconet are proven at observed",
		"put right what it names; nothing is attached until it is, and attached guests keep their cards"), findingOf(t, Run(t.Context(), st, env), "managed firewall"))
}

func TestThePoolGrantCheckOfTheAppliance(t *testing.T) {
	env, st := healthyApplianceEnv(), managedState(store.ProfileAppliance)
	st.Managed.PoolGrant = "managed"
	st.Managed.Waiting = []engine.ManagedWaiting{{GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 103}}, Why: "needs-grant"}}
	env.app.resources = []pve.Resource{
		{Kind: model.KindLXC, VMID: 202, Node: "pve1", Pool: "managed"},
		{Kind: model.KindQEMU, VMID: 101, Name: "db", Node: "pve2", Pool: "managed"},
		{Kind: model.KindQEMU, VMID: 103, Node: "pve1", Pool: "managed"},
		{Kind: model.KindQEMU, VMID: 104, Node: "pve1", Pool: "other"},
		{Kind: model.KindQEMU, VMID: 105, Node: "pve1", Pool: "managed", Tags: []string{"cf-tunnel"}},
		{Kind: model.KindQEMU, VMID: 106, Node: "pve1", Pool: "managed", Tags: []string{"cf-tunnel-managed"}},
		{Kind: model.KindQEMU, VMID: 900, Node: "pve1", Pool: "managed", Template: true},
		{Kind: model.KindLXC, VMID: testVMID, Node: "pve1", Pool: "managed"},
	}
	const note = "is in pool managed: pco's token can change its cards and its firewall; no consent for the managed network today"
	poolGrant := func(findings []Finding) []Finding {
		return slices.DeleteFunc(findings, func(f Finding) bool { return !strings.HasPrefix(f.Check, "pool grant") })
	}

	require.Equal(t, []Finding{
		ok("pool grant qemu/101", "qemu/101 (db) "+note),
		ok("pool grant qemu/105", "qemu/105 "+note),
	}, poolGrant(Run(t.Context(), st, env)), "the tag cf-tunnel-managed consents; the gate tag does not under per-guest consent")

	st.Managed.Consent, st.Managed.GateTag = store.ConsentAuto, "cf-tunnel"
	require.Equal(t, []Finding{ok("pool grant qemu/101", "qemu/101 (db) "+note)}, poolGrant(Run(t.Context(), st, env)),
		"the gate tag admits a guest under automatic consent, whether it needs the network yet or not")

	st.Managed.Consent, st.Managed.GateTag, st.Managed.TagHonoured = store.ConsentPerGuest, "", false
	require.Equal(t, []Finding{
		ok("pool grant qemu/101", "qemu/101 (db) "+note),
		ok("pool grant qemu/105", "qemu/105 "+note),
		ok("pool grant qemu/106", "qemu/106 "+note),
	}, poolGrant(Run(t.Context(), st, env)), "a tag that is not registered consents to nothing")

	env.app.resErr = errors.New("500 Internal Server Error")
	require.Equal(t, warn("pool grant", "the guests could not be read: 500 Internal Server Error", fixAPI),
		findingOf(t, Run(t.Context(), st, env), "pool grant"))
}

func findingOf(t *testing.T, findings []Finding, check string) Finding {
	t.Helper()
	i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == check })
	require.GreaterOrEqual(t, i, 0, "no finding of %q in %v", check, networkFindings(findings))
	return findings[i]
}

// Before the first cycle the state has no network, and only an enabled
// namespace unit is looked at.
func TestNoStateNoNetworkChecks(t *testing.T) {
	env := healthyNetEnv()
	st := engine.State{}

	require.Equal(t, []Finding{ok("netns", "pco-netns.service is enabled and starts before pco; no network is attached")},
		networkFindings(Run(t.Context(), st, env)))
}

// Every condition of the managed check that holds has a finding of its own,
// so that one does not hide another.
func TestTheManagedCheckNamesEveryCondition(t *testing.T) {
	st := managedState(store.ProfileHost)
	st.Managed.Refused = "pve-firewall is stopped on node pve1"
	st.Managed.Leg.Up, st.Managed.Leg.Problem = false, "bridge pconet is absent"
	st.Managed.TagHonoured = false
	st.Managed.Free = 9

	findings := slices.DeleteFunc(Run(t.Context(), st, healthyNetEnv()), func(f Finding) bool { return f.Check != "managed" })

	require.Equal(t, []Finding{
		fail("managed", "the managed network cannot run on this node: pve-firewall is stopped on node pve1; nothing is attached until it can, and attached guests keep their cards",
			"put right what it names; pco managed status shows it"),
		fail("managed", "the leg of the managed network is not up: bridge pconet is absent", "pco managed status"),
		warn("managed", "the tag cf-tunnel-managed is not registered as admin-only, so it gives no guest consent; only approvals do",
			"add cf-tunnel-managed to registered-tags in the options of the datacenter, or approve guests with pco managed approve"),
		warn("managed", "9 addresses of 10.203.118.0/24 are free", "detach the guests that no longer need a card on the managed network"),
	}, findings)
}

// In the appliance a token that may not read the changes of the SDN is no
// fault of the vnet: what pco made is checked all the same.
func TestTheVnetCheckOfTheApplianceWithoutTheRightToReadPendingChanges(t *testing.T) {
	m := managedState(store.ProfileAppliance).Managed
	for _, tt := range []struct {
		name   string
		err    error
		expect Finding
	}{
		{"forbidden", fmt.Errorf("fetching sdn zones: %w", &pve.APIError{Status: 403, Message: "Permission check failed (/sdn, SDN.Audit)"}),
			ok("vnet", "zone pco of type simple, isolate-ports off, no subnet; the changes of the SDN that wait to be applied were not read: "+
				"pco's token may not read them (fetching sdn zones: proxmox api: HTTP 403: Permission check failed (/sdn, SDN.Audit))")},
		{"another error", errors.New("timeout"),
			warn("vnet", "the changes of the SDN that wait to be applied could not be read: timeout", fixAPI)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyNetEnv()
			env.sdnErr = tt.err
			require.Equal(t, tt.expect, checkVnet(t.Context(), m, env, true))
		})
	}
}

// A pool of no size was not read as it is.
func TestAServicePoolOfNoSizeWasNotRead(t *testing.T) {
	env := healthyNetEnv()
	env.used, env.size = 0, 0

	require.Equal(t, warn("service pool", "the pool of service addresses was not read: it has no address", fixProblems),
		netFinding(t, "service pool", attachedState(), env))
}

// A namespace that is gone on a node whose unit still has a stop action is
// put right by the new unit first: a restart of the old one tears it down
// again.
func TestTheNetnsFixOfANamespaceThatIsGoneUnderAnOldUnit(t *testing.T) {
	env := healthyNetEnv()
	env.pinnedErr = errors.New("/run/netns/pco does not exist")
	env.props[netnsUnit+" ExecStop"] = "{ path=/usr/bin/pco ; argv[]=/usr/bin/pco netns down }"

	require.Equal(t, fail("netns", "the gateway namespace pco is not pinned (/run/netns/pco does not exist): the legs and the service addresses are gone; "+
		"the loaded pco-netns.service has an ExecStop line, which tears the gateway down on restart",
		"reinstall the package, then systemctl daemon-reload and systemctl restart pco-netns"), netFinding(t, "netns", attachedState(), env))
}
