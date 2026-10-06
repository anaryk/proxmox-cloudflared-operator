package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const lockToken = "7a3c1f0e-2b4d-4e6f-8a9b-0c1d2e3f4a5b"

// The SDN as pvesh lists it with --pending 1.
const (
	noZones   = `[]`
	noVnets   = `[]`
	zoneOther = `{"zone":"pcotest","type":"simple"}`
	zonePco   = `{"zone":"pco","type":"simple"}`
	zoneNew   = `{"zone":"pco","type":"simple","state":"new","pending":{"ipam":"pve"}}`
	vnetPco   = `{"vnet":"pconet","type":"vnet","zone":"pco","isolate-ports":1}`
	vnetNew   = `{"vnet":"pconet","type":"vnet","state":"new","pending":{"zone":"pco","isolate-ports":1}}`
)

func list(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

func sdnLocked() call {
	return call{line: "pvesh create /cluster/sdn/lock --allow-pending 1 --output-format json", out: `"` + lockToken + `"` + "\n"}
}

func sdnUnlocked() call {
	return call{line: "pvesh delete /cluster/sdn/lock --lock-token " + lockToken}
}

// sdnRead lists the zones and the vnets, and the subnets of each vnet in
// subnets, by vnet.
func sdnRead(zones, vnets string, subnets ...[2]string) []call {
	calls := []call{
		{line: "pvesh get /cluster/sdn/zones --pending 1 --output-format json", out: zones},
		{line: "pvesh get /cluster/sdn/vnets --pending 1 --output-format json", out: vnets},
	}
	for _, s := range subnets {
		calls = append(calls, call{line: "pvesh get /cluster/sdn/vnets/" + s[0] + "/subnets --pending 1 --output-format json", out: s[1]})
	}
	return calls
}

func zoneCreated(token bool) call {
	return call{line: "pvesh create /cluster/sdn/zones --zone pco --type simple" + lockArgs(token)}
}

func vnetCreated(isolate string, token bool) call {
	return call{line: "pvesh create /cluster/sdn/vnets --vnet pconet --zone pco --isolate-ports " + isolate + lockArgs(token)}
}

func sdnApplied(token bool) call { return call{line: "pvesh set /cluster/sdn" + lockArgs(token)} }

func lockArgs(token bool) string {
	if token {
		return " --lock-token " + lockToken
	}
	return ""
}

// sdnEnv is a host for the SDN step, with a manifest that notes every record
// among the commands.
type sdnEnv struct {
	*testEnv
	m Manifest
}

func newSDNEnv(t *testing.T) *sdnEnv {
	return &sdnEnv{testEnv: newTestEnv(t)}
}

func (e *sdnEnv) sdn() SDN {
	return SDN{
		Run: e.run, Ask: e.ask, Ours: e.m, IsolatePorts: true,
		Record: func(change func(*Manifest)) error {
			change(&e.m)
			e.events = append(e.events, fmt.Sprintf("record zone=%t vnet=%t", e.m.CreatedZone, e.m.CreatedVnet))
			return nil
		},
	}
}

func (e *sdnEnv) ensure(s SDN) error { return s.Ensure(context.Background()) }

// The zone and the vnet are made under the lock, each noted in the manifest
// before the command that makes it, with the token on every create and on
// the apply, and the lock goes at the end.
func TestSDNMadeUnderTheLockInOrder(t *testing.T) {
	e := newSDNEnv(t)
	e.script([]call{sdnLocked()}, sdnRead(list(zoneOther), noVnets),
		[]call{zoneCreated(true), vnetCreated("1", true), sdnApplied(true), sdnUnlocked()})
	e.ask.answers = []answer{{"Apply the SDN configuration? It reloads the network on every node.", true}}

	require.NoError(t, e.ensure(e.sdn()))

	e.done()
	require.Equal(t, []string{
		"run pvesh create /cluster/sdn/lock --allow-pending 1 --output-format json",
		"run pvesh get /cluster/sdn/zones --pending 1 --output-format json",
		"run pvesh get /cluster/sdn/vnets --pending 1 --output-format json",
		"record zone=true vnet=false",
		"run pvesh create /cluster/sdn/zones --zone pco --type simple --lock-token " + lockToken,
		"record zone=true vnet=true",
		"run pvesh create /cluster/sdn/vnets --vnet pconet --zone pco --isolate-ports 1 --lock-token " + lockToken,
		"run pvesh set /cluster/sdn --lock-token " + lockToken,
		"run pvesh delete /cluster/sdn/lock --lock-token " + lockToken,
	}, e.events)
	e.requireShown("SDN: applying zone pco (new), vnet pconet (new)")
}

// In the appliance the guest firewall isolates, and the appliance's own card
// is a port of the vnet as well.
func TestSDNVnetOfTheAppliance(t *testing.T) {
	e := newSDNEnv(t)
	e.script([]call{sdnLocked()}, sdnRead(noZones, noVnets),
		[]call{zoneCreated(true), vnetCreated("0", true), sdnApplied(true), sdnUnlocked()})
	s := e.sdn()
	s.IsolatePorts, s.Yes = false, true

	require.NoError(t, e.ensure(s))

	e.done()
}

// A failure after the zone was made still releases the lock; the note of
// the vnet that is not there is taken back, the zone's stays, and a run again
// goes on from there.
func TestSDNLockReleasedOnAFailureAfterTheZone(t *testing.T) {
	e := newSDNEnv(t)
	failed := vnetCreated("1", true)
	failed.err = exitErr(255, "create sdn vnet object failed: vnet ID 'pconet' already defined")
	e.script([]call{sdnLocked()}, sdnRead(noZones, noVnets), []call{zoneCreated(true), failed},
		sdnRead(list(zoneNew), noVnets), []call{sdnUnlocked()})

	err := e.ensure(e.sdn())

	require.ErrorContains(t, err, "creating SDN vnet pconet")
	e.done()
	require.True(t, e.m.CreatedZone)
	require.False(t, e.m.CreatedVnet, "the note of what is not there is taken back")
	require.Equal(t, "run pvesh delete /cluster/sdn/lock --lock-token "+lockToken, e.events[len(e.events)-1])

	// A run again finds the zone it made pending and makes the vnet.
	e.script([]call{sdnLocked()}, sdnRead(list(zoneNew), noVnets),
		[]call{vnetCreated("1", true), sdnApplied(true), sdnUnlocked()})
	s := e.sdn()
	s.Yes = true
	require.NoError(t, e.ensure(s))
	e.done()
}

// A lock that cannot be released is the admin's to lift, and is named.
func TestSDNLockNotReleased(t *testing.T) {
	e := newSDNEnv(t)
	unlock := sdnUnlocked()
	unlock.err = exitErr(255, "connection refused")
	refused := zoneCreated(true)
	refused.err = exitErr(255, "permission denied")
	e.script([]call{sdnLocked()}, sdnRead(noZones, noVnets), []call{refused}, sdnRead(noZones, noVnets), []call{unlock})

	require.Error(t, e.ensure(e.sdn()))

	e.done()
	e.requireShown("warn: the lock of the SDN configuration could not be released (" +
		"exit status 255: connection refused): pvesh delete /cluster/sdn/lock --force releases it")
	require.False(t, e.m.CreatedZone)
}

// After the apply, which releases the lock on Proxmox VE 9, a release that
// fails says nothing.
func TestSDNReleaseAfterTheApplyMayFindNoLock(t *testing.T) {
	e := newSDNEnv(t)
	unlock := sdnUnlocked()
	unlock.err = exitErr(255, "no lock found")
	e.script([]call{sdnLocked()}, sdnRead(noZones, noVnets), []call{zoneCreated(true), vnetCreated("1", true), sdnApplied(true), unlock})
	s := e.sdn()
	s.Yes = true

	require.NoError(t, e.ensure(s))

	e.done()
	require.NotContains(t, e.ask.text(), "warn:")
}

// Proxmox VE 8 has no lock: the creates and the apply go without a token,
// and the pending changes are read again right before the apply.
func TestSDNWithoutTheLockOfProxmoxVE8(t *testing.T) {
	e := newSDNEnv(t)
	noLock := sdnLocked()
	noLock.err = exitErr(255, "No 'create' handler defined for '/cluster/sdn/lock'")
	e.script([]call{noLock}, sdnRead(noZones, noVnets), []call{zoneCreated(false), vnetCreated("1", false)},
		sdnRead(list(zoneNew), list(vnetNew), [2]string{"pconet", "[]"}), []call{sdnApplied(false)})
	s := e.sdn()
	s.Yes = true

	require.NoError(t, e.ensure(s))

	e.done()
	e.requireShown("SDN: this Proxmox VE has no lock of the SDN configuration; the pending changes are read again right before the apply")
}

// Without the lock, a change of somebody else's that came while the run made
// its own is refused right before the apply.
func TestSDNWithoutTheLockRefusesAChangeThatCame(t *testing.T) {
	e := newSDNEnv(t)
	noLock := sdnLocked()
	noLock.err = exitErr(255, "No 'create' handler defined for '/cluster/sdn/lock'")
	e.script([]call{noLock}, sdnRead(noZones, noVnets), []call{zoneCreated(false), vnetCreated("1", false)},
		sdnRead(list(zoneNew, `{"zone":"tenants","type":"vlan","state":"new","pending":{"bridge":"vmbr1"}}`), list(vnetNew), [2]string{"pconet", "[]"}),
		[]call{{line: "pvesh delete /cluster/sdn/vnets/pconet"}, {line: "pvesh delete /cluster/sdn/zones/pco"}})
	s := e.sdn()
	s.Yes = true

	err := e.ensure(s)

	require.ErrorContains(t, err, "zone tenants (new)")
	e.done()
	require.Equal(t, Manifest{}, e.m, "what the run made goes again, so that the other apply does not apply it")
}

func TestSDNRefusesChangesOfOthers(t *testing.T) {
	for _, tt := range []struct {
		name, zones, vnets string
		subnets            [][2]string
		named              string
	}{
		{"a zone", list(`{"zone":"tenants","type":"vlan","state":"new","pending":{"bridge":"vmbr1"}}`), noVnets, nil, "zone tenants (new)"},
		{"a vnet", list(zoneOther), list(`{"vnet":"blue","type":"vnet","zone":"pcotest","state":"changed","pending":{"alias":"x"}}`),
			[][2]string{{"blue", "[]"}}, "vnet blue (changed)"},
		{"a subnet", list(zoneOther), list(`{"vnet":"blue","type":"vnet","zone":"pcotest"}`),
			[][2]string{{"blue", `[{"vnet":"blue","cidr":"10.1.0.0/24","type":"subnet","state":"new","pending":{"gateway":"10.1.0.1"}}]`}},
			"subnet 10.1.0.0/24 of vnet blue (new)"},
		{"a zone pco pco did not make", list(zoneNew), noVnets, nil, "zone pco (new)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newSDNEnv(t)
			e.script([]call{sdnLocked()}, sdnRead(tt.zones, tt.vnets, tt.subnets...), []call{sdnUnlocked()})
			s := e.sdn()
			s.Yes = true

			err := e.ensure(s)

			require.ErrorContains(t, err, "the SDN has changes that are not applied and are not pco's: "+tt.named)
			require.ErrorContains(t, err, "--confirm-sdn-apply")
			e.done()
			require.Equal(t, Manifest{}, e.m, "nothing is made")
		})
	}
}

func TestSDNAppliesChangesOfOthersWhenConfirmed(t *testing.T) {
	e := newSDNEnv(t)
	tenants := `{"zone":"tenants","type":"vlan","state":"new","pending":{"bridge":"vmbr1"}}`
	e.script([]call{sdnLocked()}, sdnRead(list(tenants), noVnets),
		[]call{zoneCreated(true), vnetCreated("1", true), sdnApplied(true), sdnUnlocked()})
	s := e.sdn()
	s.Confirm = true
	e.ask.answers = []answer{{"Apply the SDN configuration?", true}}

	require.NoError(t, e.ensure(s))

	e.done()
	e.requireShown("SDN: applying zone pco (new), vnet pconet (new)")
	e.requireShown("SDN: and changes that are not pco's, as --confirm-sdn-apply says: zone tenants (new)")
}

// What a run made before and did not apply is pco's own pending change.
func TestSDNAppliesWhatAnEarlierRunMade(t *testing.T) {
	e := newSDNEnv(t)
	e.m = Manifest{CreatedZone: true, CreatedVnet: true}
	e.script([]call{sdnLocked()}, sdnRead(list(zoneNew), list(vnetNew), [2]string{"pconet", "[]"}),
		[]call{sdnApplied(true), sdnUnlocked()})
	s := e.sdn()
	s.Yes = true

	require.NoError(t, e.ensure(s))

	e.done()
	e.requireShown("SDN: applying zone pco (new), vnet pconet (new)")
}

func TestSDNThereAlready(t *testing.T) {
	e := newSDNEnv(t)
	e.script([]call{sdnLocked()}, sdnRead(list(zonePco), list(vnetPco), [2]string{"pconet", "[]"}), []call{sdnUnlocked()})

	require.NoError(t, e.ensure(e.sdn()))

	e.done()
	e.requireShown("SDN: zone pco and vnet pconet are there, nothing needed")
}

// An operator who does not apply leaves nothing pending: what the run made
// goes again, and its notes with it.
func TestSDNNotApplied(t *testing.T) {
	e := newSDNEnv(t)
	e.script([]call{sdnLocked()}, sdnRead(noZones, noVnets), []call{zoneCreated(true), vnetCreated("1", true),
		{line: "pvesh delete /cluster/sdn/vnets/pconet --lock-token " + lockToken},
		{line: "pvesh delete /cluster/sdn/zones/pco --lock-token " + lockToken},
		sdnUnlocked()})
	e.ask.answers = []answer{{"Apply the SDN configuration?", false}}

	err := e.ensure(e.sdn())

	require.ErrorIs(t, err, ErrAborted)
	e.done()
	require.Equal(t, Manifest{}, e.m)
}

func TestSDNRefusesObjectsItCannotUse(t *testing.T) {
	for _, tt := range []struct {
		name, zones, vnets, subnets, want string
	}{
		{"a zone of another type", list(`{"zone":"pco","type":"vlan","bridge":"vmbr1"}`), noVnets, "",
			"SDN zone pco is there with type vlan: the managed network needs a zone of type simple"},
		{"the vnet in another zone", list(zoneOther), list(`{"vnet":"pconet","zone":"pcotest","type":"vnet"}`), "[]",
			"SDN vnet pconet is there in zone pcotest, not in zone pco"},
		{"the vnet with other ports", list(zonePco), list(`{"vnet":"pconet","zone":"pco","type":"vnet"}`), "[]",
			"SDN vnet pconet has isolate-ports 0, and the managed network of this node needs 1: " +
				"pvesh set /cluster/sdn/vnets/pconet --isolate-ports 1, then pvesh set /cluster/sdn"},
		{"a subnet on the vnet", list(zonePco), list(vnetPco), `[{"vnet":"pconet","cidr":"10.203.118.0/24","type":"subnet"}]`,
			"SDN vnet pconet has the subnet 10.203.118.0/24: the managed network has none, pco's leg is its only gateway"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newSDNEnv(t)
			var subnets [][2]string
			if tt.subnets != "" {
				subnets = [][2]string{{"pconet", tt.subnets}}
			}
			e.script([]call{sdnLocked()}, sdnRead(tt.zones, tt.vnets, subnets...), []call{sdnUnlocked()})

			err := e.ensure(e.sdn())

			require.EqualError(t, err, tt.want)
			e.done()
		})
	}
}

// The lock that Proxmox refuses for another reason stops the step.
func TestSDNLockRefused(t *testing.T) {
	e := newSDNEnv(t)
	refused := sdnLocked()
	refused.err = exitErr(255, "the SDN configuration is locked by another task")
	e.script([]call{refused})

	err := e.ensure(e.sdn())

	require.ErrorContains(t, err, "taking the lock of the SDN configuration")
	require.ErrorContains(t, err, "pvesh delete /cluster/sdn/lock --force")
	e.done()
}

func vnetRemoved() call {
	return call{line: "pvesh delete /cluster/sdn/vnets/pconet --lock-token " + lockToken}
}

func zoneRemoved() call {
	return call{line: "pvesh delete /cluster/sdn/zones/pco --lock-token " + lockToken}
}

// The vnet goes before its zone, under the lock, and the apply removes both
// from every node.
func TestSDNRemovedUnderTheLock(t *testing.T) {
	e := newSDNEnv(t)
	e.m = Manifest{CreatedZone: true, CreatedVnet: true}
	e.script([]call{sdnLocked()}, sdnRead(list(zoneOther, zonePco), list(vnetPco), [2]string{"pconet", "[]"}),
		[]call{vnetRemoved(), zoneRemoved(), sdnApplied(true), sdnUnlocked()})

	require.NoError(t, e.sdn().Remove(context.Background()))

	e.done()
	e.requireShown("SDN: removed vnet pconet")
	e.requireShown("SDN: removed zone pco")
}

// A zone that holds a vnet of somebody else's stays, and so does what the
// manifest does not note as pco's.
func TestSDNRemovesOnlyWhatIsPcos(t *testing.T) {
	e := newSDNEnv(t)
	e.m = Manifest{CreatedZone: true, CreatedVnet: true}
	e.script([]call{sdnLocked()}, sdnRead(list(zonePco), list(vnetPco, `{"vnet":"lab","zone":"pco","type":"vnet"}`),
		[2]string{"pconet", "[]"}, [2]string{"lab", "[]"}),
		[]call{vnetRemoved(), sdnApplied(true), sdnUnlocked()})

	require.NoError(t, e.sdn().Remove(context.Background()))

	e.done()
	e.requireShown("SDN: zone pco is kept: it holds the vnet lab as well")

	e.m = Manifest{CreatedVnet: true}
	e.script([]call{sdnLocked()}, sdnRead(list(zonePco), list(vnetPco), [2]string{"pconet", "[]"}),
		[]call{vnetRemoved(), sdnApplied(true), sdnUnlocked()})
	require.NoError(t, e.sdn().Remove(context.Background()))
	e.done()
}

func TestSDNRemovalRefusesChangesOfOthers(t *testing.T) {
	e := newSDNEnv(t)
	e.m = Manifest{CreatedZone: true, CreatedVnet: true}
	tenants := `{"zone":"tenants","type":"vlan","state":"new","pending":{"bridge":"vmbr1"}}`
	e.script([]call{sdnLocked()}, sdnRead(list(zonePco, tenants), list(vnetPco), [2]string{"pconet", "[]"}), []call{sdnUnlocked()})

	err := e.sdn().Remove(context.Background())

	require.EqualError(t, err, "the SDN has changes that are not applied and are not pco's: zone tenants (new); "+
		"an apply would apply them as well: apply them or roll them back, then run this again")
	e.done()
}

// A removal an earlier run made and did not apply is applied.
func TestSDNRemovalAppliesWhatAnEarlierRunRemoved(t *testing.T) {
	e := newSDNEnv(t)
	e.m = Manifest{CreatedZone: true, CreatedVnet: true}
	e.script([]call{sdnLocked()}, sdnRead(list(`{"zone":"pco","type":"simple","state":"deleted"}`),
		list(`{"vnet":"pconet","type":"vnet","zone":"pco","state":"deleted"}`)),
		[]call{sdnApplied(true), sdnUnlocked()})

	require.NoError(t, e.sdn().Remove(context.Background()))

	e.done()
}

func TestSDNRemovalWithoutTheLock(t *testing.T) {
	e := newSDNEnv(t)
	e.m = Manifest{CreatedZone: true, CreatedVnet: true}
	noLock := sdnLocked()
	noLock.err = exitErr(255, "No 'create' handler defined for '/cluster/sdn/lock'")
	e.script([]call{noLock}, sdnRead(list(zonePco), list(vnetPco), [2]string{"pconet", "[]"}),
		[]call{{line: "pvesh delete /cluster/sdn/vnets/pconet"}, {line: "pvesh delete /cluster/sdn/zones/pco"}},
		sdnRead(list(`{"zone":"pco","type":"simple","state":"deleted"}`), list(`{"vnet":"pconet","type":"vnet","zone":"pco","state":"deleted"}`)),
		[]call{sdnApplied(false)})

	require.NoError(t, e.sdn().Remove(context.Background()))

	e.done()
}
