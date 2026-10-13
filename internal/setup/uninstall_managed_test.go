package setup

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A guest of the cluster as the uninstall reads it: its ref, its node and
// its configuration.
type guestCfg struct{ ref, node, cfg string }

const (
	cardOnPconet = `"net1":"name=eth1,bridge=pconet,hwaddr=06:50:43:4F:76:05,ip=10.203.118.5/24,firewall=0"`
	cardOnVmbr0  = `"net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp"`
)

func ctOnPconet() guestCfg {
	return guestCfg{"lxc/105", testNode, `{"digest":"cfg1",` + cardOnVmbr0 + "," + cardOnPconet + "}"}
}

func ctOffPconet() guestCfg { return guestCfg{"lxc/105", testNode, "{" + cardOnVmbr0 + "}"} }

func vmOnPconet() guestCfg {
	return guestCfg{"qemu/120", "pve2", `{"digest":"cfg2","net0":"virtio=BC:24:11:00:00:02,bridge=vmbr0","net2":"virtio=06:50:43:4f:76:07,bridge=pconet,firewall=0"}`}
}

func resources(guests ...guestCfg) call {
	var rows []string
	for _, g := range guests {
		kind, vmid, _ := strings.Cut(g.ref, "/")
		rows = append(rows, `{"id":"`+g.ref+`","type":"`+kind+`","vmid":`+vmid+`,"node":"`+g.node+`"}`)
	}
	return call{line: "pvesh get /cluster/resources --type vm --output-format json", out: "[" + strings.Join(rows, ",") + "]"}
}

func configRead(g guestCfg) call {
	return call{line: "pvesh get /nodes/" + g.node + "/" + g.ref + "/config --current 1 --output-format json", out: g.cfg}
}

// cardsRead reads every guest of the cluster, as the uninstall does to find
// the cards on pconet.
func cardsRead(guests ...guestCfg) []call {
	calls := []call{resources(guests...)}
	for _, g := range guests {
		calls = append(calls, configRead(g))
	}
	return calls
}

// sdnRemoved is the removal of the zone and the vnet under the lock.
func sdnRemoved() []call {
	return slices.Concat([]call{sdnLocked()}, sdnRead(list(zonePco), list(vnetPco), [2]string{"pconet", "[]"}),
		[]call{vnetRemoved(), zoneRemoved(), sdnApplied(true), sdnUnlocked()})
}

var setupsManaged = Manifest{CreatedZone: true, CreatedVnet: true}

func TestUninstallRemovesTheManagedNetworkLast(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsManaged)
	e.script(connectorsSeen(), egressSeen(), cardsRead(ctOffPconet()), serviceStopped(), connectorsPruned(), egressDeleted(),
		cardsRead(ctOffPconet()), sdnRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))

	e.done()
	e.requireShown("the SDN vnet pconet and zone pco, which an SDN apply removes from every node, reloading the network of each")
	e.requireStoreGone()
	require.Equal(t, "run pvesh delete /cluster/sdn/lock --lock-token "+lockToken, e.events[len(e.events)-1], "the managed network goes last")
}

// While a guest has a card on pconet the zone stays, and so does the store
// with the manifest, so that a run again removes it.
func TestUninstallKeepsTheZoneWhileACardIsOnIt(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsManaged)
	e.script(connectorsSeen(), egressSeen(), cardsRead(ctOnPconet(), vmOnPconet()), serviceStopped(), connectorsPruned(), egressDeleted(),
		cardsRead(ctOnPconet(), vmOnPconet()))

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

	require.ErrorContains(t, err, "the SDN zone pco is kept: guests lxc/105, qemu/120 still have a card on pconet; "+
		"detach them first or pass --detach-managed")
	e.done()
	e.requireShown("(the SDN vnet pconet and zone pco are kept while guests lxc/105, qemu/120 have a card on pconet; " +
		"--detach-managed removes pco's cards first)")
	require.True(t, e.manifest().CreatedZone, "the store is kept")
}

func attached(owner string, nic int, addr string, fw *store.FirewallBefore) store.Attachment {
	a := netip.MustParseAddr(addr)
	return store.Attachment{Owner: owner, Identity: "id-" + owner, NIC: nic, MAC: store.ManagedMAC(a), Addr: a, Consent: "tag", Since: t0, Firewall: fw}
}

// --detach-managed removes pco's cards right after the daemon stopped, waits
// for the configurations to drop them, and removes the zone at the end.
func TestUninstallDetachesTheManagedCardsFirst(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsManaged)
	require.NoError(t, e.st.SaveAttachment(attached("lxc/105", 1, "10.203.118.5", nil)))
	e.script(connectorsSeen(), egressSeen(), cardsRead(ctOnPconet()), serviceStopped(),
		[]call{resources(ctOnPconet()), configRead(ctOnPconet()), {line: "pvesh set /nodes/pve1/lxc/105/config --delete net1 --digest cfg1"},
			configRead(ctOffPconet())},
		connectorsPruned(), egressDeleted(), cardsRead(ctOffPconet()), sdnRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, DetachManaged: true}))

	e.done()
	e.requireShown("the managed cards pco attached to lxc/105, removed first")
	e.requireShown("managed card of lxc/105: removed net1")
	e.requireStoreGone()
}

// A card that is not pco's keeps the zone even with --detach-managed.
func TestUninstallKeepsTheZoneForACardThatIsNotPcos(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsManaged)
	e.script(connectorsSeen(), egressSeen(), cardsRead(vmOnPconet()), serviceStopped(),
		connectorsPruned(), egressDeleted(), cardsRead(vmOnPconet()))

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, DetachManaged: true})

	require.ErrorContains(t, err, "the SDN zone pco is kept: guests qemu/120 still have a card on pconet, which pco did not attach: remove it")
	e.done()
}

// A VM without network hotplug keeps the card until it restarts: the wait
// is bounded, and the zone stays.
func TestUninstallWaitsBoundedForTheCards(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsManaged)
	require.NoError(t, e.st.SaveAttachment(attached("qemu/120", 2, "10.203.118.7", nil)))
	var slept []time.Duration
	e.s.host.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	waits := []call{resources(vmOnPconet()), configRead(vmOnPconet()), {line: "pvesh set /nodes/pve2/qemu/120/config --delete net2 --digest cfg2"}}
	for range detachPolls {
		waits = append(waits, configRead(vmOnPconet()))
	}
	e.script(connectorsSeen(), egressSeen(), cardsRead(vmOnPconet()), serviceStopped(), waits, connectorsPruned(), egressDeleted(),
		cardsRead(vmOnPconet()))

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, DetachManaged: true})

	require.ErrorContains(t, err, "the SDN zone pco is kept: guests qemu/120 still have a card on pconet; "+
		"a VM without network hotplug drops it when it restarts, then run pco uninstall again")
	e.done()
	require.Len(t, slept, detachPolls-1)
	e.requireShown("warn: managed card of qemu/120: net2 is still in its configuration; a VM without network hotplug drops it when it restarts")
}

// In the appliance pco turned on the guest's firewall and gave the card its
// rules and its ipset: pco's rules go with the card, told by their comment,
// an admin's rule on the same card stays, and a firewall that was off is off
// again.
func TestDetachPutsBackTheFirewall(t *testing.T) {
	e := newTestEnv(t)
	ct := ctOnPconet()
	fw := "/nodes/pve1/lxc/105/firewall"
	rules := func(digest string, pos ...int) string {
		all := map[int]string{
			0: `{"pos":0,"type":"in","action":"ACCEPT","iface":"net0","dport":"22","digest":"` + digest + `"}`,
			1: `{"pos":1,"type":"in","action":"ACCEPT","iface":"net1","proto":"udp","comment":"pco:0123456789ab","digest":"` + digest + `"}`,
			2: `{"pos":2,"type":"in","action":"ACCEPT","iface":"net1","dport":"8080","comment":"backup agent","digest":"` + digest + `"}`,
			3: `{"pos":3,"type":"in","action":"DROP","iface":"net1","comment":"pco:0123456789ab","digest":"` + digest + `"}`,
			4: `{"pos":4,"type":"out","action":"DROP","iface":"net1","comment":"pco:0123456789ab","digest":"` + digest + `"}`,
		}
		var out []string
		for _, p := range pos {
			out = append(out, all[p])
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	readRules := "pvesh get " + fw + "/rules --output-format json"
	e.script([]call{
		resources(ct), configRead(ct),
		{line: "pvesh set /nodes/pve1/lxc/105/config --delete net1 --digest cfg1"},
		{line: readRules, out: rules("r1", 0, 1, 2, 3, 4)},
		{line: "pvesh delete " + fw + "/rules/4 --digest r1"},
		{line: readRules, out: rules("r2", 0, 1, 2, 3)},
		{line: "pvesh delete " + fw + "/rules/3 --digest r2"},
		{line: readRules, out: rules("r3", 0, 1, 2)},
		{line: "pvesh delete " + fw + "/rules/1 --digest r3"},
		{line: readRules, out: rules("r4", 0, 2)},
		{line: "pvesh get " + fw + "/ipset --output-format json", out: `[{"name":"ipfilter-net1"},{"name":"trusted"}]`},
		{line: "pvesh delete " + fw + "/ipset/ipfilter-net1 --force 1"},
		{line: "pvesh get " + fw + "/options --output-format json", out: `{"enable":1,"policy_in":"ACCEPT","policy_out":"ACCEPT","digest":"o1"}`},
		{line: readRules, out: rules("r5", 0, 2)},
		{line: "pvesh set " + fw + "/options --enable 0 --policy_in DROP --delete policy_out --digest o1"},
		configRead(ctOffPconet()),
	})
	d := Detacher{Run: e.run, Ask: e.ask, Sleep: func(context.Context, time.Duration) error { return nil }}

	left, err := d.Detach(context.Background(), []store.Attachment{attached("lxc/105", 1, "10.203.118.5",
		&store.FirewallBefore{Enabled: false, Rules: 2, PolicyIn: "DROP"})})

	require.NoError(t, err)
	require.Empty(t, left)
	e.done()
	e.requireShown("managed card of lxc/105: the firewall is as it was before pco attached the card")
}

// A firewall that an admin took up after pco turned it on stays on: its
// policies changed, or it has more rules that are not pco's.
func TestDetachLeavesAFirewallAnAdminTookUp(t *testing.T) {
	for name, tt := range map[string]struct{ options, rules, why string }{
		"a policy changed": {`{"enable":1,"policy_in":"DROP","policy_out":"ACCEPT","digest":"o1"}`, `[]`,
			"its policies were changed since pco turned it on"},
		"a rule added": {`{"enable":1,"policy_in":"ACCEPT","policy_out":"ACCEPT","digest":"o1"}`,
			`[{"pos":0,"type":"in","action":"ACCEPT","iface":"net0","dport":"22","enable":1,"digest":"r1"}]`,
			"it has 1 rules more than when pco turned it on"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			ct := ctOnPconet()
			fw := "/nodes/pve1/lxc/105/firewall"
			e.script([]call{
				resources(ct), configRead(ct),
				{line: "pvesh set /nodes/pve1/lxc/105/config --delete net1 --digest cfg1"},
				{line: "pvesh get " + fw + "/rules --output-format json", out: tt.rules},
				{line: "pvesh get " + fw + "/ipset --output-format json", out: `[]`},
				{line: "pvesh get " + fw + "/options --output-format json", out: tt.options},
				{line: "pvesh get " + fw + "/rules --output-format json", out: tt.rules},
				configRead(ctOffPconet()),
			})
			d := Detacher{Run: e.run, Ask: e.ask, Sleep: func(context.Context, time.Duration) error { return nil }}

			_, err := d.Detach(context.Background(), []store.Attachment{attached("lxc/105", 1, "10.203.118.5", &store.FirewallBefore{})})

			require.NoError(t, err)
			e.done()
			e.requireShown("warn: managed card of lxc/105: the firewall stays on, which pco turned on for the card: " + tt.why +
				"; pco's rules and ipset on net1 are removed")
		})
	}
}

// A card that is gone already was removed by somebody else, and net1 may be
// an admin's card by now: nothing of the guest's firewall is touched, and the
// commands that would put it back are shown instead.
func TestDetachRunsNothingForACardThatIsGone(t *testing.T) {
	e := newTestEnv(t)
	ct := ctOffPconet()
	e.script([]call{resources(ct), configRead(ct)})
	d := Detacher{Run: e.run, Ask: e.ask, Sleep: func(context.Context, time.Duration) error { return nil }}

	left, err := d.Detach(context.Background(), []store.Attachment{attached("lxc/105", 1, "10.203.118.5", &store.FirewallBefore{Enabled: false, Rules: 1})})

	require.NoError(t, err)
	require.Empty(t, left)
	e.done()
	for _, line := range e.run.ran {
		require.NotContains(t, line, "/firewall", "no firewall command runs for a card that is gone")
	}
	e.requireShown("warn: managed card of lxc/105: net1 is gone already, so its firewall is left as it is; pco's rules on net1 " +
		"carry the comment pco:<install id>, and these put the firewall back by hand:")
	e.requireShown("  pvesh get /nodes/pve1/lxc/105/firewall/rules --output-format json, then pvesh delete /nodes/pve1/lxc/105/firewall/rules/<pos> for each rule on net1 with that comment, from the last")
	e.requireShown("  pvesh delete /nodes/pve1/lxc/105/firewall/ipset/ipfilter-net1 --force 1")
	e.requireShown("  pvesh set /nodes/pve1/lxc/105/firewall/options --enable 0")
}

// A firewall that was on stays on.
func TestDetachLeavesAFirewallThatWasOn(t *testing.T) {
	e := newTestEnv(t)
	ct := ctOnPconet()
	fw := "/nodes/pve1/lxc/105/firewall"
	e.script([]call{
		resources(ct), configRead(ct),
		{line: "pvesh set /nodes/pve1/lxc/105/config --delete net1 --digest cfg1"},
		{line: "pvesh get " + fw + "/rules --output-format json", out: `[]`},
		{line: "pvesh get " + fw + "/ipset --output-format json", out: `[]`},
		configRead(ctOffPconet()),
	})
	d := Detacher{Run: e.run, Ask: e.ask, Sleep: func(context.Context, time.Duration) error { return nil }}

	_, err := d.Detach(context.Background(), []store.Attachment{attached("lxc/105", 1, "10.203.118.5", &store.FirewallBefore{Enabled: true})})

	require.NoError(t, err)
	e.done()
}

// A card at the index whose MAC is not pco's is not pco's card.
func TestDetachLeavesACardThatIsNotPcos(t *testing.T) {
	e := newTestEnv(t)
	other := guestCfg{"lxc/105", testNode, `{"net1":"name=eth1,bridge=pconet,hwaddr=BC:24:11:00:00:09"}`}
	e.script([]call{resources(other), configRead(other)})
	d := Detacher{Run: e.run, Ask: e.ask, Sleep: func(context.Context, time.Duration) error { return nil }}

	left, err := d.Detach(context.Background(), []store.Attachment{attached("lxc/105", 1, "10.203.118.5", nil)})

	require.NoError(t, err)
	require.Empty(t, left)
	e.done()
	e.requireShown("warn: managed card of lxc/105: net1 on pconet has the MAC bc:24:11:00:00:09, not pco's 06:50:43:4f:76:05; left as it is")
}

func TestCardsOnReadEveryGuest(t *testing.T) {
	e := newTestEnv(t)
	template := guestCfg{"qemu/9000", "pve2", `{"template":1,"net0":"e1000,macaddr=BC:24:11:00:00:03,bridge=pconet"}`}
	gone := configRead(guestCfg{"lxc/130", testNode, ""})
	gone.err = exitErr(2, "Configuration file 'nodes/pve1/lxc/130.conf' does not exist")
	e.script([]call{resources(ctOnPconet(), vmOnPconet(), template, guestCfg{"lxc/130", testNode, ""}),
		configRead(ctOnPconet()), configRead(vmOnPconet()), gone, configRead(template)})

	cards, err := CardsOn(context.Background(), e.run, "pconet")

	require.NoError(t, err)
	e.done()
	require.Equal(t, []Card{
		{Owner: "lxc/105", Node: "pve1", NIC: 1, MAC: "06:50:43:4f:76:05"},
		{Owner: "qemu/120", Node: "pve2", NIC: 2, MAC: "06:50:43:4f:76:07"},
		{Owner: "qemu/9000", Node: "pve2", NIC: 0, MAC: "bc:24:11:00:00:03"},
	}, cards)
}
