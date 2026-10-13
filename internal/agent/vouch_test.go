package agent

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// vouchedFleet is a watch that runs and has pinned the addresses of routes
// guests, each running since long before, with proofs all made at once.
func vouchedFleet(routes int, at time.Time) (*vouching, []resolve.Binding) {
	v := &vouching{watching: at.Add(-time.Hour), pinned: map[netip.Addr]pinnedSince{}, guests: map[string]guestSeen{}}
	bindings := make([]resolve.Binding, routes)
	for i := range bindings {
		addr := netip.AddrFrom4([4]byte{10, 1, byte(i / 250), byte(i%250 + 1)})
		mac := fmt.Sprintf("bc:24:11:00:%02x:%02x", i>>8, i&0xff)
		ref := fmt.Sprintf("qemu/%d", 101+i)
		v.pinned[addr] = pinnedSince{mac: mac, bridge: "vmbr0", port: fmt.Sprintf("tap%di0", 101+i), since: at.Add(-time.Minute)}
		v.guests[ref] = guestSeen{running: true, since: at.Add(-time.Hour)}
		bindings[i] = resolve.Binding{Guest: ref, Addr: addr, MAC: mac, VerifiedAt: at}
	}
	return v, bindings
}

// The proofs made in one cycle come due over the interval, a share of them in
// each cycle, and none stands for longer than the interval.
func TestTheProofsOfOneCycleComeDueOverTheInterval(t *testing.T) {
	const routes, poll, every = 1000, 10 * time.Second, time.Minute
	v, bindings := vouchedFleet(routes, t0)

	most, oldest := 0, time.Duration(0)
	for now := t0.Add(poll); now.Before(t0.Add(5 * every)); now = now.Add(poll) {
		proven := 0
		for i, b := range bindings {
			if !v.vouches(b, now, every) {
				oldest = max(oldest, now.Sub(b.VerifiedAt))
				bindings[i].VerifiedAt = now
				proven++
			}
		}
		most = max(most, proven)
	}

	require.LessOrEqual(t, most, 2*routes*int(poll/time.Second)/int(every/time.Second), "proven in one cycle at most")
	require.LessOrEqual(t, oldest, every)
}

// A proof dated after now is due though now falls in the interval of the
// proof, as a clock that went back a little does.
func TestAProofDatedAfterNowIsDue(t *testing.T) {
	const every = 24 * time.Hour
	v, bindings := vouchedFleet(1, t0)
	b := bindings[0]
	now := b.VerifiedAt.Add(-time.Second)
	require.False(t, due(b.Addr, now, b.VerifiedAt, every), "the two are in one interval")

	require.True(t, due(b.Addr, b.VerifiedAt, now, every))
	require.False(t, v.vouches(b, now, every))
}

func TestAProofStandsOnlyForTheNodeAndPinItWasMadeWith(t *testing.T) {
	const every = time.Minute
	t.Run("another node", func(t *testing.T) {
		v, bindings := vouchedFleet(1, t0)
		g := model.Guest{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Node: testNode, Running: true}
		v.guests = nil
		v.see(inventoryOf1(g), t0.Add(-time.Minute))
		require.True(t, v.vouches(bindings[0], t0.Add(10*time.Second), every))

		g.Node = "pve2"
		v.see(inventoryOf1(g), t0.Add(10*time.Second))

		require.False(t, v.vouches(bindings[0], t0.Add(10*time.Second), every))
	})

	// The pin of another bridge counts from when the watch got it: the proof
	// made in the cycle that set it, and stands only from the next one.
	t.Run("another bridge", func(t *testing.T) {
		v, bindings := vouchedFleet(1, t0)
		b := bindings[0]
		pin := egress.Pin{MAC: mustMAC(b.MAC), Bridge: "vmbr1", Port: "tap101i0"}
		v.pin(map[netip.Addr]egress.Pin{b.Addr: pin}, t0)

		require.False(t, v.vouches(b, t0.Add(10*time.Second), every), "the proof made as the pin changed")
		b.VerifiedAt = t0.Add(10 * time.Second)
		v.pin(map[netip.Addr]egress.Pin{b.Addr: pin}, t0.Add(10*time.Second))
		require.True(t, v.vouches(b, t0.Add(20*time.Second), every), "the next one")
	})
}

// inventoryOf1 is an inventory with one guest.
func inventoryOf1(g model.Guest) inventory.Snapshot {
	return inventory.Snapshot{Guests: []model.Guest{g}, Complete: true}
}
