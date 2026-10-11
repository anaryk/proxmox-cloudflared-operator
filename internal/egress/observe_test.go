package egress

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	webMAC   = "bc:24:11:00:00:01"
	webMAC1  = "bc:24:11:00:00:11"
	dbMAC    = "bc:24:11:00:00:02"
	nobodies = "bc:24:11:ff:ff:01"
)

var (
	web = model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	db  = model.GuestRef{Kind: model.KindLXC, VMID: 102}
)

// observing is an observer over a snapshot the test changes, with the
// observations it made and the time it says it is.
type observing struct {
	o        *observer
	snapshot map[string]Expected
	seen     []Observation
	clock    time.Time
}

func observe(t *testing.T) *observing {
	t.Helper()
	ob := &observing{
		snapshot: map[string]Expected{
			webMAC:  {MAC: webMAC, Guest: web, NIC: 0},
			webMAC1: {MAC: webMAC1, Guest: web, NIC: 1},
			dbMAC:   {MAC: dbMAC, Guest: db, NIC: 0},
		},
		clock: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	ob.o = newObserver(
		func() map[string]Expected { return ob.snapshot },
		func(o Observation) { ob.seen = append(ob.seen, o) },
		func() time.Time { return ob.clock },
	)
	return ob
}

// learn puts the entries into the observer one by one and returns what they
// made it report, which it forgets.
func (ob *observing) learn(t *testing.T, entries ...entry) []Observation {
	t.Helper()
	for _, e := range entries {
		ob.o.see(e)
	}
	out := ob.seen
	ob.seen = nil
	return out
}

func forwarding(t *testing.T, bridge string, vlan int, hw, port string) entry {
	t.Helper()
	return entry{mac: mac(t, hw), bridge: bridge, vlan: vlan, port: port}
}

func gone(e entry) entry {
	e.gone = true
	return e
}

func TestAMACOnItsOwnPortIsLearned(t *testing.T) {
	for _, port := range []string{"tap101i0", "fwpr101p0", "veth101i0"} {
		t.Run(port, func(t *testing.T) {
			ob := observe(t)

			got := ob.learn(t, forwarding(t, "vmbr0", 30, webMAC, port))

			require.Equal(t, []Observation{{
				Kind: Learned, MAC: webMAC, Bridge: "vmbr0", VLAN: 30, Port: port,
				At: ob.clock,
			}}, got)
		})
	}
}

func TestEachNICHasItsOwnPort(t *testing.T) {
	ob := observe(t)

	got := ob.learn(t,
		forwarding(t, "vmbr0", 0, webMAC, "tap101i0"),
		forwarding(t, "vmbr1", 0, webMAC1, "tap101i1"),
	)

	require.Equal(t, []ObservationKind{Learned, Learned}, kinds(got))
}

func TestAMACMovedToTheUplinkHasMoved(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))
	ob.clock = ob.clock.Add(time.Minute)

	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1"))

	require.Equal(t, []Observation{{
		Kind: Moved, MAC: webMAC, Bridge: "vmbr0", Port: "eno1", From: "tap101i0",
		At: time.Date(2026, 10, 6, 12, 1, 0, 0, time.UTC),
	}}, got)
}

func TestAMoveIsReportedOnce(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1"))

	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1")))
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "bond0")))

	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"), forwarding(t, "vmbr0", 0, webMAC, "bond0"))
	require.Equal(t, []ObservationKind{Learned, Moved}, kinds(got))
	require.Equal(t, "tap101i0", got[1].From)
}

// On a node that does not run the guest its MAC is learned on the uplink all
// along, which moves nothing.
func TestAMACOnTheUplinkOfANodeThatDoesNotRunTheGuestMovesNothing(t *testing.T) {
	ob := observe(t)

	got := ob.learn(t,
		forwarding(t, "vmbr0", 0, webMAC, "eno1"),
		forwarding(t, "vmbr0", 0, webMAC, "eno1"),
		forwarding(t, "vmbr0", 0, webMAC, "bond0"),
	)

	require.Empty(t, got)
}

func TestAMACOnAnotherGuestsPortIsAConflict(t *testing.T) {
	tests := []struct {
		port  string
		owner model.GuestRef
	}{
		{port: "tap102i0", owner: model.GuestRef{Kind: model.KindQEMU, VMID: 102}},
		{port: "veth205i1", owner: model.GuestRef{Kind: model.KindLXC, VMID: 205}},
		{port: "fwpr102p0", owner: db},
		{port: "fwpr400p0", owner: model.GuestRef{VMID: 400}},
	}
	for _, tc := range tests {
		t.Run(tc.port, func(t *testing.T) {
			ob := observe(t)

			got := ob.learn(t, forwarding(t, "vmbr0", 30, webMAC, tc.port))

			require.Equal(t, []Observation{{
				Kind: Conflict, MAC: webMAC, Bridge: "vmbr0", VLAN: 30, Port: tc.port, Owner: &tc.owner,
				At: ob.clock,
			}}, got)
		})
	}
}

func TestAGuestPortOfAnotherVMIDIsAConflictWhateverTheKind(t *testing.T) {
	ob := observe(t)

	// The expected guest is a virtual machine; a container has the port.
	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "veth102i0"))

	require.Len(t, got, 1)
	require.Equal(t, Conflict, got[0].Kind)
	require.Equal(t, &model.GuestRef{Kind: model.KindLXC, VMID: 102}, got[0].Owner)
}

func TestAConflictIsReportedEveryTimeTheTableShowsIt(t *testing.T) {
	ob := observe(t)

	first := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap102i0"))
	again := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap102i0"))

	require.Equal(t, []ObservationKind{Conflict}, kinds(first))
	require.Equal(t, []ObservationKind{Conflict}, kinds(again))
}

func TestAMACThatLeftItsPortForAnotherGuestsIsAConflictThatSaysWhereItWas(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))

	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap102i0"))

	require.Len(t, got, 1)
	require.Equal(t, Conflict, got[0].Kind)
	require.Equal(t, "tap101i0", got[0].From)
	require.Equal(t, "tap102i0", got[0].Port)
}

func TestAMACThatIsNotExpectedGivesNothing(t *testing.T) {
	ob := observe(t)

	got := ob.learn(t,
		forwarding(t, "vmbr0", 0, nobodies, "tap101i0"),
		forwarding(t, "vmbr0", 0, nobodies, "tap102i0"),
		forwarding(t, "vmbr0", 0, nobodies, "eno1"),
	)

	require.Empty(t, got)
}

func TestTheOtherGuestsOwnMACOnItsOwnPortIsLearnedForIt(t *testing.T) {
	ob := observe(t)

	got := ob.learn(t, forwarding(t, "vmbr0", 0, dbMAC, "veth102i0"))

	require.Equal(t, []ObservationKind{Learned}, kinds(got))
	require.Equal(t, dbMAC, got[0].MAC)
}

// The port of the same guest that another NIC has is no conflict, and not
// where this NIC's MAC belongs either.
func TestAMACOnAnotherNICsPortOfItsGuestHasMoved(t *testing.T) {
	ob := observe(t)
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i1")), "never seen on its own port")
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))

	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i1"))

	require.Len(t, got, 1)
	require.Equal(t, Moved, got[0].Kind)
	require.Equal(t, "tap101i0", got[0].From)
	require.Equal(t, "tap101i1", got[0].Port)
}

func TestTheBridgeAndTheVLANTellEntriesApart(t *testing.T) {
	ob := observe(t)
	ob.learn(t,
		forwarding(t, "vmbr0", 30, webMAC, "tap101i0"),
		forwarding(t, "vmbr0", 40, webMAC, "tap101i0"),
		forwarding(t, "vmbr1", 0, webMAC, "tap101i0"),
	)

	got := ob.learn(t, forwarding(t, "vmbr0", 40, webMAC, "eno1"))

	require.Len(t, got, 1)
	require.Equal(t, Moved, got[0].Kind)
	require.Equal(t, 40, got[0].VLAN)
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr2", 0, webMAC, "eno1")), "another bridge never had it")
	require.Equal(t, []ObservationKind{Moved}, kinds(ob.learn(t, forwarding(t, "vmbr1", 0, webMAC, "eno1"))))
	require.Equal(t, []ObservationKind{Moved}, kinds(ob.learn(t, forwarding(t, "vmbr0", 30, webMAC, "eno1"))))
}

// The bridge sends nothing while an entry stays as it is, so what it showed
// is reported once, and once more when the entry goes: a guest that stops
// takes its port and the entries on it, and an entry that is not refreshed
// ages out the same way.
func TestWhatTheTableShowedEndsWhenTheEntryGoes(t *testing.T) {
	tests := []struct {
		port string
		kind ObservationKind
	}{
		{port: "tap101i0", kind: Learned},
		{port: "tap102i0", kind: Conflict},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			ob := observe(t)
			on := forwarding(t, "vmbr0", 30, webMAC, tc.port)
			require.Equal(t, []ObservationKind{tc.kind}, kinds(ob.learn(t, on)))
			ob.clock = ob.clock.Add(5 * time.Minute)

			got := ob.learn(t, gone(on))

			require.Equal(t, []Observation{{
				Kind: Gone, MAC: webMAC, Bridge: "vmbr0", VLAN: 30, Port: tc.port,
				At: ob.clock,
			}}, got)
			require.Empty(t, ob.learn(t, gone(on)), "an entry goes once")
		})
	}
}

// A guest that stops has its entries deleted with its port; the MAC that
// turns up on the uplink after it started on another node is not a move.
func TestAnEntryThatWentTakesWhatWasKnownOfItsPort(t *testing.T) {
	ob := observe(t)
	own := forwarding(t, "vmbr0", 0, webMAC, "tap101i0")
	ob.learn(t, own)

	require.Equal(t, []ObservationKind{Gone}, kinds(ob.learn(t, gone(own))))
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1")))
}

// The port that went with the entry may have gone before it could be named;
// the observer knows which one it was.
func TestAnEntryThatWentIsNamedByThePortItWasOn(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))

	got := ob.learn(t, gone(forwarding(t, "vmbr0", 0, webMAC, "")))

	require.Len(t, got, 1)
	require.Equal(t, Gone, got[0].Kind)
	require.Equal(t, "tap101i0", got[0].Port)
}

func TestADeletionOfAnEntryNothingWasReportedOfSaysNothing(t *testing.T) {
	ob := observe(t)
	uplink := forwarding(t, "vmbr0", 0, webMAC, "eno1")

	require.Empty(t, ob.learn(t, uplink, gone(uplink)), "on a node that does not run the guest")
	require.Equal(t, []ObservationKind{Learned, Moved}, kinds(ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"), uplink)))
	require.Empty(t, ob.learn(t, gone(uplink)), "after a move")
	require.Empty(t, ob.learn(t, gone(forwarding(t, "vmbr0", 0, nobodies, "tap101i0"))), "of a MAC not expected")
}

// The clone that had the MAC is quiet and the guest, elsewhere, speaks: the
// bridge moves the entry to the uplink, and the conflict it showed is over.
func TestAConflictWhoseMACLeavesForTheUplinkIsOver(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap102i0"))
	ob.clock = ob.clock.Add(time.Minute)

	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1"))

	require.Equal(t, []Observation{{
		Kind: Gone, MAC: webMAC, Bridge: "vmbr0", Port: "tap102i0", At: ob.clock,
	}}, got)
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1")), "and stays over")
}

func TestADeletionOfAnotherMACsEntryChangesNothing(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))

	require.Empty(t, ob.learn(t, gone(forwarding(t, "vmbr0", 0, dbMAC, "veth102i0"))))

	require.Equal(t, []ObservationKind{Moved}, kinds(ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1"))))
}

func TestTheSnapshotIsReadAtEveryEntry(t *testing.T) {
	ob := observe(t)
	late := forwarding(t, "vmbr0", 0, nobodies, "tap103i0")
	require.Empty(t, ob.learn(t, late))

	ob.snapshot[nobodies] = Expected{MAC: nobodies, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 103}}
	got := ob.learn(t, late)

	require.Equal(t, []ObservationKind{Learned}, kinds(got))

	delete(ob.snapshot, nobodies)
	require.Empty(t, ob.learn(t, late))
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, nobodies, "eno1")), "what it was before it was dropped is forgotten")
}

func TestAMACNoLongerExpectedIsForgotten(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))
	saved := ob.snapshot[webMAC]

	delete(ob.snapshot, webMAC)
	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1")))
	ob.snapshot[webMAC] = saved

	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "eno1")))
}

func TestAnEntryWithoutAPortNameOrABridgeNameIsHalfKnown(t *testing.T) {
	ob := observe(t)
	ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, "tap101i0"))

	require.Empty(t, ob.learn(t, forwarding(t, "", 0, webMAC, "eno1")), "a bridge that cannot be named places nothing")

	got := ob.learn(t, forwarding(t, "vmbr0", 0, webMAC, ""))
	require.Len(t, got, 1, "a port that cannot be named is not the guest's")
	require.Equal(t, Moved, got[0].Kind)
	require.Equal(t, "tap101i0", got[0].From)
	require.Empty(t, got[0].Port)

	require.Empty(t, ob.learn(t, forwarding(t, "vmbr0", 0, nobodies, "")))
}

func TestANeighbourEntryIsNotForwarding(t *testing.T) {
	ob := observe(t)

	got := ob.learn(t, entry{addr: addr("10.20.0.10"), dev: "vmbr0", mac: mac(t, webMAC)})

	require.Empty(t, got)
}

func TestWhichPortsAreAGuests(t *testing.T) {
	tests := []struct {
		name string
		port guestPort
		ok   bool
	}{
		{name: "tap101i0", port: guestPort{vmid: 101, nic: 0, kind: model.KindQEMU}, ok: true},
		{name: "tap9999999i12", port: guestPort{vmid: 9999999, nic: 12, kind: model.KindQEMU}, ok: true},
		{name: "veth205i1", port: guestPort{vmid: 205, nic: 1, kind: model.KindLXC}, ok: true},
		{name: "fwpr101p0", port: guestPort{vmid: 101, nic: 0}, ok: true},
		{name: "fwpr101i0"},
		{name: "tap101p0"},
		{name: "veth101p0"},
		{name: "tap101i"},
		{name: "tap101"},
		{name: "tapi0"},
		{name: "tap101i0.5"},
		{name: "tap0101i0"},
		{name: "tap101i00"},
		{name: "tap-1i0"},
		{name: "tap101i-1"},
		{name: "tap99999999999999999999i0"},
		{name: "fwln101i0"},
		{name: "fwbr101i0"},
		{name: "vethpco1"},
		{name: "vethpco-i0"},
		{name: "eno1"},
		{name: "vmbr0"},
		{name: "bond0.30"},
		{name: "vxlan_labx"},
		{name: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseGuestPort(tc.name)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.port, got)
		})
	}
}

func kinds(obs []Observation) []ObservationKind {
	out := make([]ObservationKind, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.Kind)
	}
	return out
}
