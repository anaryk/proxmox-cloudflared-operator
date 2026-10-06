package applianceinstall

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// managedNode is an installed appliance lxc/100 on a node whose guest
// firewall can isolate the managed network: the datacenter firewall on, the
// iptables backend, pve-firewall running.
func managedNode(t *testing.T) *testEnv {
	t.Helper()
	e := installed(t)
	e.node.firewall = true
	return e
}

func (e *testEnv) managedEnable(o ManagedOptions) error {
	e.t.Helper()
	return e.in.ManagedEnableCommand(e.t.Context(), o)
}

// ranSince are the commands from the first that starts with prefix on.
func (f *fakeNode) ranSince(prefix string) []string {
	i := slices.IndexFunc(f.ran, func(l string) bool { return strings.HasPrefix(l, prefix) })
	if i < 0 {
		return nil
	}
	return f.ran[i:]
}

// The node makes the zone and the vnet without isolated ports, each noted in
// the appliance's manifest before it is made, under the lock, and grants them
// through the helper of grant-network.
func TestManagedEnableMakesTheVnetAndGrantsIt(t *testing.T) {
	e := managedNode(t)

	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}), e.ask.text())

	require.Equal(t, map[string]*sdnObject{"pco": {typ: "simple"}}, e.node.sdn.zones)
	require.Equal(t, map[string]*sdnObject{"pconet": {typ: "vnet", zone: "pco"}}, e.node.sdn.vnets, "isolate-ports 0 in the appliance")
	require.Equal(t, 1, e.node.sdn.applies)
	require.Empty(t, e.node.sdn.lock, "the lock is released")
	require.Equal(t, []string{
		"/sdn/zones/pco/pconet token pco@pve!vm100 PCOSDN",
		"/sdn/zones/pco/pconet user pco@pve PCOSDN",
		"/vms/100 token pco@pve!vm100 PCOManaged",
		"/vms/100 user pco@pve PCOManaged",
	}, e.node.pcoLines(), "for the user and the token, on the vnet and never on the zone")
	m := e.node.manifest(100)
	require.True(t, m.CreatedZone)
	require.True(t, m.CreatedVnet)
	require.Equal(t, []setup.NetworkGrant{{Zone: "pco", VNet: "pconet", CreatedRoles: []string{"PCOManaged", "PCOSDN"}}}, m.Appliance.Grants)

	sdn := e.node.ranSince("pvesh create /cluster/sdn/lock")
	require.Equal(t, []string{
		"pvesh create /cluster/sdn/lock --allow-pending 1 --output-format json",
		"pvesh get /cluster/sdn/zones --pending 1 --output-format json",
		"pvesh get /cluster/sdn/vnets --pending 1 --output-format json",
		"pct exec 100 --keep-env 0 -- pco appliance manifest sdn --created-zone=true",
		"pvesh create /cluster/sdn/zones --zone pco --type simple --lock-token lock-1",
		"pct exec 100 --keep-env 0 -- pco appliance manifest sdn --created-vnet=true",
		"pvesh create /cluster/sdn/vnets --vnet pconet --zone pco --isolate-ports 0 --lock-token lock-1",
		"pvesh set /cluster/sdn --lock-token lock-1",
		"pvesh delete /cluster/sdn/lock --lock-token lock-1",
	}, sdn[:9])
	require.Contains(t, sdn, "pveum acl modify /sdn/zones/pco/pconet --users pco@pve --tokens pco@pve!vm100 --roles PCOSDN")
	require.Contains(t, e.ask.text(), "next: pct exec 100 -- pco network managed enable")
	require.Empty(t, entries(t, e.journals), "no journal is left")
}

// The firewall of the node is read before anything is made; each refusal
// names its fix.
func TestManagedEnableRefusesANodeWhoseFirewallCannotIsolate(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(e *testEnv)
		want  string
	}{
		{"the nftables backend", func(e *testEnv) { e.node.nodeFirewall["nftables"] = 1 },
			"Managed in the appliance is not supported with the nftables firewall backend (nftables: 1): it installed no guest rules " +
				"in the lab; set nftables: 0 in the node's firewall options"},
		{"the datacenter firewall off", func(e *testEnv) { e.node.firewall = false },
			"Managed in the appliance needs the datacenter firewall; it is off: turn it on under Datacenter, Firewall, Options, " +
				"with rules that keep your own access, and run this again"},
		{"pve-firewall stopped", func(e *testEnv) { e.node.fwService = "inactive" },
			"the node's firewall service pve-firewall is not running (inactive): start it with systemctl start pve-firewall, " +
				"and run this again"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := managedNode(t)
			tt.setup(e)

			err := e.managedEnable(ManagedOptions{VMID: 100, Yes: true})

			require.EqualError(t, err, tt.want)
			for _, line := range e.node.ran {
				require.False(t, strings.HasPrefix(line, "pvesh create") || strings.HasPrefix(line, "pveum acl modify") ||
					strings.HasPrefix(line, "pveum role add"), "nothing is made: %s", line)
			}
			require.Empty(t, e.node.sdn.zones)
		})
	}
}

// The option is compared by its value: an explicit 0 is the iptables backend.
func TestManagedEnableTakesAnExplicitIptablesBackend(t *testing.T) {
	e := managedNode(t)
	e.node.nodeFirewall["nftables"] = 0
	e.node.nodeFirewall["enable"] = 1

	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}), e.ask.text())
}

// --pool adds one pair of lines on the pool and one grant in the manifest,
// and says what it hands the token.
func TestManagedEnableWithAPool(t *testing.T) {
	e := managedNode(t)
	e.node.pools["web"] = &fakePool{Members: []int{200}}

	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Pool: "web", Yes: true}), e.ask.text())

	require.Equal(t, 1, e.node.count("pveum acl modify /pool/web --users pco@pve --tokens pco@pve!vm100 --roles PCOManaged"))
	require.Equal(t, 1, e.node.count("pveum acl modify /pool/"))
	require.True(t, e.node.has("/pool/web", "user", "pco@pve", "PCOManaged"))
	require.True(t, e.node.has("/pool/web", "token", "pco@pve!vm100", "PCOManaged"))
	grants := e.node.manifest(100).Appliance.Grants
	require.Len(t, grants, 2)
	require.Equal(t, setup.NetworkGrant{Pool: "web"}, grants[1])
	require.Contains(t, e.ask.text(), "pco@pve!vm100 may now change the network cards and the firewall of every guest in pool web")
}

func TestManagedEnableRefusesThePoolOfTheAppliances(t *testing.T) {
	e := managedNode(t)

	err := e.managedEnable(ManagedOptions{VMID: 100, Pool: "pco", Yes: true})

	require.EqualError(t, err, "--pool pco: pool pco holds the appliances: grant the pool of the guests")
	require.Empty(t, e.node.sdn.zones)
}

// A second run finds everything there, applies nothing and grants again
// what is there already.
func TestManagedEnableTwice(t *testing.T) {
	e := managedNode(t)
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))

	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}), e.ask.text())

	require.Equal(t, 1, e.node.sdn.applies)
	require.Len(t, e.node.manifest(100).Appliance.Grants, 1)
	require.Contains(t, e.ask.text(), "SDN: zone pco and vnet pconet are there, nothing needed")
}

func TestManagedEnableAsks(t *testing.T) {
	e := managedNode(t)
	e.ask.answers = []answer{{"Apply the SDN configuration? It reloads the network on every node.", true}, {"Grant these?", false}}

	err := e.managedEnable(ManagedOptions{VMID: 100})

	require.ErrorIs(t, err, setup.ErrAborted)
	require.Equal(t, 1, e.node.sdn.applies, "the vnet is there")
	require.Empty(t, e.node.pcoLines(), "nothing is granted")
}

// The appliance must run, as its manifest notes what the node makes.
func TestManagedEnableNeedsTheApplianceRunning(t *testing.T) {
	e := managedNode(t)
	e.node.cts[100].running = false

	err := e.managedEnable(ManagedOptions{VMID: 100, Yes: true})

	require.EqualError(t, err, "lxc/100 is stopped: start it with pct start 100, as its manifest notes what this makes")

	e.node.cts[100].running = true
	e.node.cts[100].cfg["description"] = "my container"
	require.ErrorContains(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}), "lxc/100 is not a pco appliance")
	require.ErrorContains(t, e.managedEnable(ManagedOptions{VMID: 99, Yes: true}), "--vmid 99: want 100 to 999999999")
	require.Empty(t, e.node.sdn.zones)
}

// On Proxmox VE 8 the step goes without the lock.
func TestManagedEnableWithoutTheLock(t *testing.T) {
	e := managedNode(t)
	e.node.sdn.noLock = true

	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}), e.ask.text())

	require.Equal(t, 1, e.node.sdn.applies)
	require.Equal(t, 0, e.node.count("pvesh delete /cluster/sdn/lock"))
	require.Equal(t, 1, e.node.count("pvesh set /cluster/sdn"))
}
