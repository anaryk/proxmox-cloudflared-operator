package applianceinstall

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
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
	mark := "pco appliance vm100, installed 2026-10-01 by pco appliance install"
	note := e.sdnNote(100)
	require.Regexp(t, `^[0-9a-f]{32}$`, note, "the node keeps a note of its own, which the appliance cannot read")
	zoneLine, vnetLine := "made the SDN zone pco, note "+note, "made the SDN vnet pconet, note "+note
	require.Equal(t, []string{
		"pvesh create /cluster/sdn/lock --allow-pending 1 --output-format json",
		"pvesh get /cluster/sdn/zones --pending 1 --output-format json",
		"pvesh get /cluster/sdn/vnets --pending 1 --output-format json",
		"pct set 100 --description " + mark + "\n" + zoneLine,
		"pct exec 100 --keep-env 0 -- pco appliance manifest sdn --created-zone=true",
		"pvesh create /cluster/sdn/zones --zone pco --type simple --lock-token lock-1",
		"pct set 100 --description " + mark + "\n" + zoneLine + "\n" + vnetLine,
		"pct exec 100 --keep-env 0 -- pco appliance manifest sdn --created-vnet=true",
		"pvesh create /cluster/sdn/vnets --vnet pconet --zone pco --isolate-ports 0 --lock-token lock-1",
		"pvesh set /cluster/sdn --lock-token lock-1",
		"pvesh delete /cluster/sdn/lock --lock-token lock-1",
	}, sdn[:11], "each object is noted on the node and in the appliance before it is made")
	require.Equal(t, mark+"\n"+zoneLine+"\n"+vnetLine, e.node.cts[100].cfg["description"])
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

// What is made and granted is shown and asked about once, before anything
// is made.
func TestManagedEnableAsks(t *testing.T) {
	e := managedNode(t)
	e.node.pools["web"] = &fakePool{}
	e.ask.answers = []answer{{"Make the managed network for lxc/100 and grant these? The SDN apply reloads the network on every node.", false}}

	err := e.managedEnable(ManagedOptions{VMID: 100, Pool: "web"})

	require.ErrorIs(t, err, setup.ErrAborted)
	require.Equal(t, 0, e.node.sdn.applies, "nothing is applied")
	require.Empty(t, e.node.sdn.zones, "nothing is made")
	require.Empty(t, e.node.pcoLines(), "nothing is granted")
	require.Equal(t, 0, e.node.count("pvesh create /cluster/sdn/lock"))
	shown := e.ask.text()
	for _, line := range []string{
		"role PCOManaged (VM.Config.Network) on /vms/100",
		"role PCOSDN (SDN.Use, SDN.Audit) on /sdn/zones/pco/pconet",
		"pveum acl modify /pool/web --users pco@pve --tokens 'pco@pve!vm100' --roles PCOManaged",
	} {
		require.Contains(t, shown, line)
	}

	e.ask.answers = []answer{{"Make the managed network for lxc/100 and grant these?", true}}
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Pool: "web"}), e.ask.text())
	require.Equal(t, 1, e.node.sdn.applies)
	require.Len(t, e.node.pcoLines(), 6)
}

// The manifest inside the appliance is the appliance's word: the zone and
// the vnet go only when the description of the container, which the node
// keeps, names them as made for it too.
func TestUninstallKeepsTheSDNObjectsTheNodeDoesNotRecord(t *testing.T) {
	e := managedNode(t)
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
	desc, ok := withSDNNotes(e.node.cts[100].cfg["description"], false, true, e.sdnNote(100))
	require.True(t, ok)
	e.node.cts[100].cfg["description"] = desc

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.NotEmpty(t, e.node.sdn.zones, "the zone stays")
	require.Empty(t, e.node.sdn.vnets, "the vnet goes, which both name")
	require.Contains(t, e.ask.text(), "(kept: the SDN zone pco, which the manifest of lxc/100 names as pco's and the description of its container does not)")
	require.Contains(t, e.ask.text(), "the SDN vnet pconet, which an SDN apply removes from every node")

	// With neither named on the node, a guest's card on pconet is not pco's
	// business either: nothing refuses the uninstall for it.
	e = managedNode(t)
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
	desc, _ = withSDNNotes(e.node.cts[100].cfg["description"], false, false, "")
	e.node.cts[100].cfg["description"] = desc
	e.guestOnPconet()
	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())
	require.NotEmpty(t, e.node.sdn.zones)
	require.NotEmpty(t, e.node.sdn.vnets)
	require.Equal(t, 1, e.node.sdn.applies, "no second apply")
}

// The description can be changed by whoever may set the options of the
// container: a note in it counts only with the one the node keeps where only
// root writes. Without it, or with another, both objects stay.
func TestUninstallKeepsTheSDNObjectsWithoutTheNoteOfTheNode(t *testing.T) {
	for name, change := range map[string]func(e *testEnv){
		"the note of the node is gone": func(e *testEnv) { require.NoError(t, os.Remove(e.sdnNotePath(100))) },
		"the description has another": func(e *testEnv) {
			desc, ok := withSDNNotes(e.node.cts[100].cfg["description"], true, true, strings.Repeat("0", 32))
			require.True(t, ok)
			e.node.cts[100].cfg["description"] = desc
		},
		"the description has none": func(e *testEnv) {
			desc, ok := withSDNNotes(e.node.cts[100].cfg["description"], false, false, "")
			require.True(t, ok)
			e.node.cts[100].cfg["description"] = desc + "\nmade the SDN zone pco\nmade the SDN vnet pconet"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := managedNode(t)
			require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
			change(e)

			require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

			require.NotEmpty(t, e.node.sdn.zones)
			require.NotEmpty(t, e.node.sdn.vnets)
			require.Contains(t, e.ask.text(), "(kept: the SDN zone pco, which the description of lxc/100 names as pco's under a note this node does not keep)")
			require.Contains(t, e.ask.text(), "(kept: the SDN vnet pconet, which the description of lxc/100 names as pco's under a note this node does not keep)")
		})
	}
}

func (e *testEnv) sdnNotePath(vmid int) string {
	return filepath.Join(e.pveDir, "priv", "pco-appliance", fmt.Sprintf("sdn-vm%d", vmid))
}

// sdnNote is the note the node keeps of the SDN objects made for the
// appliance vmid.
func (e *testEnv) sdnNote(vmid int) string {
	e.t.Helper()
	b, err := os.ReadFile(e.sdnNotePath(vmid))
	require.NoError(e.t, err)
	info, err := os.Stat(e.sdnNotePath(vmid))
	require.NoError(e.t, err)
	require.Equal(e.t, os.FileMode(0o600), info.Mode().Perm())
	return strings.TrimSpace(string(b))
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

// guestOnPconet is lxc/105, a container of an admin's with pco's card on
// pconet: the managed MAC of 10.203.118.5.
func (e *testEnv) guestOnPconet() {
	const vmid = 105
	e.node.cts[vmid] = &fakeCT{running: true, files: map[string]fakeFile{}, cfg: map[string]string{
		"hostname": "web", "net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:01:05,ip=dhcp",
		"net1": "name=eth1,bridge=pconet,hwaddr=" + pcoCard + ",ip=10.203.118.5/24,firewall=1",
	}}
}

const pcoCard = "06:50:43:4F:76:05"

// The appliance's own card goes with its container; the zone and the vnet go
// last, under the lock, once no guest has a card on pconet.
func TestUninstallRemovesTheManagedNetworkOfTheAppliance(t *testing.T) {
	e := managedNode(t)
	e.node.pools["web"] = &fakePool{}
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Pool: "web", Yes: true}))
	e.node.cts[100].cfg["net1"] = "name=leg1,bridge=pconet,hwaddr=02:50:43:4F:FF:01,firewall=0"

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Empty(t, e.node.sdn.zones)
	require.Empty(t, e.node.sdn.vnets)
	require.Equal(t, 2, e.node.sdn.applies)
	require.Empty(t, e.node.sdn.lock)
	require.Empty(t, e.node.pcoLines(), "the grants of the vnet and of the pool")
	require.Nil(t, e.node.roles["PCOManaged"])
	require.Nil(t, e.node.roles["PCOSDN"])
	require.Contains(t, e.ask.text(), "the SDN vnet pconet and zone pco, which an SDN apply removes from every node, reloading the network of each")
	destroyed := slices.Index(e.node.ran, "pct destroy 100 --purge 1")
	require.Greater(t, slices.Index(e.node.ran, "pvesh delete /cluster/sdn/vnets/pconet --lock-token lock-2"), destroyed, "the zone goes last")
	require.NoFileExists(t, e.sdnNotePath(100), "the note of the node goes with them")
}

// The manifest that says the zone is pco's goes with the container, so a
// guest's card on pconet refuses the uninstall before anything goes: a run
// after it could no longer tell the zone is pco's.
func TestUninstallRefusedWhileAGuestHasACardOnTheManagedNetwork(t *testing.T) {
	e := managedNode(t)
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
	e.guestOnPconet()

	err := e.in.Uninstall(t.Context(), 100, uninstallOptions())

	require.EqualError(t, err, "the SDN zone pco is kept: guests lxc/105 still have a card on pconet; detach them first or pass "+
		"--detach-managed; nothing was removed")
	require.NotEmpty(t, e.node.sdn.zones)
	require.NotNil(t, e.node.cts[100])

	// A card pco did not attach stays with --detach-managed as well.
	o := uninstallOptions()
	o.DetachManaged = true
	err = e.in.Uninstall(t.Context(), 100, o)
	require.EqualError(t, err, "the SDN zone pco is kept: guests lxc/105 still have a card on pconet, which pco did not attach: "+
		"remove it; nothing was removed")
}

// attachedTo105 is what the appliance lists for the card it attached to
// lxc/105, with pco's firewall changes.
func attachedTo105(addr string) map[string]any {
	return map[string]any{"owner": "lxc/105", "identity": "x", "nic": 1, "mac": store.ManagedMAC(netip.MustParseAddr(addr)),
		"addr": addr, "consent": "tag", "since": "2026-10-01T12:00:00Z", "firewall": map[string]any{"enabled": false, "rules": 1}}
}

// --detach-managed reads the attachments from the appliance before it goes,
// stops the daemon in it so that it attaches nothing again, removes the
// cards with the firewall pco turned on, and then the zone. The card of a
// guest the appliance's token may change goes; an admin's rule on the same
// card stays.
func TestUninstallDetachesTheCardsOfTheAppliance(t *testing.T) {
	e := managedNode(t)
	e.node.pools["web"] = &fakePool{Members: []int{105}}
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Pool: "web", Yes: true}))
	e.guestOnPconet()
	e.node.guestFirewalls[105] = &fakeGuestFirewall{enabled: true,
		rules: []fakeRule{{"net0", ""}, pcoRule("net1"), {"net1", "backup agent"}, pcoRule("net1")}, ipsets: []string{"ipfilter-net1"}}
	e.node.attachments = []map[string]any{attachedTo105("10.203.118.5")}
	o := uninstallOptions()
	o.DetachManaged = true

	require.NoError(t, e.in.Uninstall(t.Context(), 100, o), e.ask.text())

	require.NotContains(t, e.node.cts[105].cfg, "net1")
	require.Equal(t, &fakeGuestFirewall{rules: []fakeRule{{"net0", ""}, {"net1", "backup agent"}}, ipsets: []string{}}, e.node.guestFirewalls[105],
		"the firewall as it was, but for the admin's rule")
	require.Empty(t, e.node.sdn.zones)
	stopped := slices.Index(e.node.ran, "pct exec 100 --keep-env 0 -- systemctl stop pco.service")
	removed := slices.IndexFunc(e.node.ran, func(line string) bool {
		return strings.HasPrefix(line, "pvesh set /nodes/pve1/lxc/105/config --delete net1 --digest ")
	})
	require.Greater(t, stopped, slices.Index(e.node.ran, "pct exec 100 --keep-env 0 -- pco appliance attachments"), "the daemon stops after the attachments are read")
	require.Less(t, stopped, removed, "the daemon stops before a card goes")
	require.Less(t, removed, slices.Index(e.node.ran, "pct destroy 100 --purge 1"))
	require.Contains(t, e.ask.text(), "the managed cards lxc/100 attached to lxc/105, removed first")
}

// What the appliance lists is its word: a card of a guest its token may not
// change (no VM.Config.Network on it, through the guest or its pool) and a
// card whose address is outside the managed subnet are not its, and stay.
func TestUninstallLeavesTheCardsTheApplianceMayNotChange(t *testing.T) {
	expired := t0.Add(-time.Hour).Unix()
	for _, tt := range []struct {
		name       string
		granted    bool
		attachment map[string]any
		prepare    func(*fakeNode)
		warning    string
	}{
		{"a guest the token is not granted", false, attachedTo105("10.203.118.5"), nil,
			"lxc/100 lists a card it attached to lxc/105, and its token pco@pve!vm100 holds no VM.Config.Network on /vms/105; ignored"},
		{"an address outside the managed subnet", true, attachedTo105("10.204.0.5"), nil,
			"lxc/100 lists a card it attached to lxc/105 with 10.204.0.5, outside its managed subnet 10.203.118.0/24; ignored"},
		{"a token that expired", true, attachedTo105("10.203.118.5"),
			func(f *fakeNode) { f.user("pco@pve").Tokens[0].Expire = expired },
			"lxc/100 lists a card it attached to lxc/105, and its token pco@pve!vm100 expired at 2026-10-01 11:00 UTC; ignored"},
		{"a user that is disabled", true, attachedTo105("10.203.118.5"),
			func(f *fakeNode) { f.user("pco@pve").Enabled = false },
			"lxc/100 lists a card it attached to lxc/105, and the user pco@pve of its token is disabled; ignored"},
		{"a user that expired", true, attachedTo105("10.203.118.5"),
			func(f *fakeNode) { f.user("pco@pve").Expire = expired },
			"lxc/100 lists a card it attached to lxc/105, and the user pco@pve of its token expired at 2026-10-01 11:00 UTC; ignored"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := managedNode(t)
			require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
			if tt.granted {
				e.node.pools["web"] = &fakePool{Members: []int{105}}
				e.node.grant("/pool/web", "token", "pco@pve!vm100", "PCOManaged")
				e.node.grant("/pool/web", "user", "pco@pve", "PCOManaged")
			}
			if tt.prepare != nil {
				tt.prepare(e.node)
			}
			e.guestOnPconet()
			e.node.attachments = []map[string]any{tt.attachment}
			o := uninstallOptions()
			o.DetachManaged = true

			err := e.in.Uninstall(t.Context(), 100, o)

			require.ErrorContains(t, err, "guests lxc/105 still have a card on pconet, which pco did not attach")
			require.Contains(t, e.ask.text(), "warn: "+tt.warning)
			require.Contains(t, e.node.cts[105].cfg, "net1")
			require.NotNil(t, e.node.cts[100])
		})
	}
}

// The appliance's list is asked about per guest once, however many cards it
// names on the guest, and a list longer than the guests of the cluster is
// refused before anything is asked.
func TestUninstallAsksOncePerGuestAndCapsTheList(t *testing.T) {
	second := attachedTo105("10.203.118.6")
	second["nic"] = 2
	t.Run("once per guest", func(t *testing.T) {
		e := managedNode(t)
		require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
		e.guestOnPconet()
		e.node.attachments = []map[string]any{attachedTo105("10.203.118.5"), second}
		o := uninstallOptions()
		o.DetachManaged = true

		_ = e.in.Uninstall(t.Context(), 100, o)

		asked := 0
		for _, line := range e.node.ran {
			if strings.HasPrefix(line, "pvesh get /access/permissions --userid pco@pve!vm100 --path /vms/105 ") {
				asked++
			}
		}
		require.Equal(t, 1, asked)
	})
	t.Run("more cards than guests", func(t *testing.T) {
		e := managedNode(t)
		require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
		e.guestOnPconet()
		var many []map[string]any
		for i := range 3 {
			a := attachedTo105(fmt.Sprintf("10.203.118.%d", 5+i))
			a["nic"] = 1 + i
			many = append(many, a)
		}
		e.node.attachments = many
		o := uninstallOptions()
		o.DetachManaged = true

		err := e.in.Uninstall(t.Context(), 100, o)

		require.ErrorContains(t, err, "pco appliance attachments listed 3 cards, more than the 2 guests of the cluster")
		require.NotContains(t, e.node.ran, "pvesh get /access/permissions --userid pco@pve!vm100 --path /vms/105 --output-format json")
		require.NotNil(t, e.node.cts[100])
	})
}

// What a stopped appliance attached cannot be read; the uninstall refuses
// before anything goes.
func TestUninstallDetachNeedsTheApplianceRunning(t *testing.T) {
	e := managedNode(t)
	require.NoError(t, e.managedEnable(ManagedOptions{VMID: 100, Yes: true}))
	e.node.cts[100].running = false
	o := uninstallOptions()
	o.DetachManaged = true

	err := e.in.Uninstall(t.Context(), 100, o)

	require.EqualError(t, err, "--detach-managed reads the cards lxc/100 attached from it, and it is stopped: start it with pct start 100; nothing was removed")
	require.NotNil(t, e.node.cts[100])
}

// An appliance whose manifest does not name the zone leaves it.
func TestUninstallLeavesAZoneTheApplianceDidNotMake(t *testing.T) {
	e := managedNode(t)
	e.node.sdn.zones["pco"] = &sdnObject{typ: "simple"}
	e.node.sdn.vnets["pconet"] = &sdnObject{typ: "vnet", zone: "pco"}

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.NotEmpty(t, e.node.sdn.zones)
	require.Equal(t, 0, e.node.count("pvesh create /cluster/sdn/lock"))
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
