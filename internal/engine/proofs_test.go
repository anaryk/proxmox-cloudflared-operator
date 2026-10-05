package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// wireProber answers as a node on whose vmbr0 every guest owns its address
// and has its MAC learned on its own port, and counts the ARP requests.
type wireProber struct {
	mu      sync.Mutex
	arps    int
	dialErr error
}

func (p *wireProber) Interfaces(context.Context) ([]resolve.HostIface, error) {
	return []resolve.HostIface{{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/24")}}}, nil
}

func (p *wireProber) Route(context.Context, netip.Addr) (string, bool, error) {
	return "vmbr0", true, nil
}

func (p *wireProber) ARP(context.Context, string, netip.Addr) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.arps++
	return []string{testMAC}, nil
}

func (p *wireProber) FDBPorts(context.Context, string, int, string) ([]string, error) {
	return []string{"tap101i0"}, nil
}

func (p *wireProber) Dial(context.Context, netip.AddrPort) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialErr
}

// taken returns the ARP requests since the last call.
func (p *wireProber) taken() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.arps
	p.arps = 0
	return n
}

func (p *wireProber) failDials(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dialErr = err
}

// wired is an env whose engine resolves through the real resolver: web-1
// publishes two hostnames on one port of its one address.
func wired(t *testing.T) (*env, *wireProber) {
	t.Helper()
	e := newEnv(t)
	p := &wireProber{}
	e.eng.d.Resolver = resolve.NewResolver(p, resolve.Settings{LocalNode: testNode}, e.clock.now)
	e.inv.set(snapshot(webOnItsAddress("")))
	return e, p
}

func webOnItsAddress(digest string) model.Guest {
	g := guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8080")
	g.NICs = []model.NIC{{Index: 0, MAC: testMAC, Bridge: "vmbr0", Static: []netip.Addr{guestAddr}}}
	g.Digest = digest
	return g
}

// next runs a cycle a poll interval after the last one and returns the ARP
// requests it made; the address must be served after it.
func (e *env) next(t *testing.T, p *wireProber) int {
	t.Helper()
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.Contains(t, e.eng.Bound(), guestAddr, "%v", st.Problems)
	return p.taken()
}

func TestWithoutTheWatchEveryCycleProvesTheAddressOnce(t *testing.T) {
	e, p := wired(t)

	for range 4 {
		require.Equal(t, 1, e.next(t, p), "two routes on one address are one check")
	}
}

// A proof the watch vouches for stands until its address comes due: once in
// every interval, at a time of the address's own.
func TestAProofTheWatchVouchesForStandsUntilItsAddressComesDue(t *testing.T) {
	e, p := wired(t)
	e.eng.Watching(true)
	e.toIntervalOf(guestAddr, time.Minute)

	require.Equal(t, 1, e.next(t, p), "the first proof")
	require.Equal(t, 1, e.next(t, p), "made before the watch had the address")
	for i := range 3 {
		require.Zero(t, e.next(t, p), "%d0s old", i+1)
	}
	require.Equal(t, 1, e.next(t, p), "the address comes due")
	for i := range 5 {
		require.Zero(t, e.next(t, p), "%d0s old", i+1)
	}
	require.Equal(t, 1, e.next(t, p), "a minute later")

	t.Run("a shorter interval", func(t *testing.T) {
		e.settings(func(s *store.Settings) { s.ReverifyInterval = store.Duration(20 * time.Second) })
		proven := 0
		for range 6 {
			proven += e.next(t, p)
		}
		require.Equal(t, 3, proven, "once in each 20s")
	})
}

// toIntervalOf moves the clock to the start of the interval of a minute in
// which the proof of addr is due, so that one made in the next cycles stands
// for the rest of it.
func (e *env) toIntervalOf(addr netip.Addr, every time.Duration) {
	from := e.clock.now()
	for s := time.Second; s <= every; s += time.Second {
		if due(addr, from, from.Add(s), every) {
			e.clock.advance(s)
			return
		}
	}
}

func TestAProofIsMadeAgain(t *testing.T) {
	tests := []struct {
		name   string
		change func(e *env, p *wireProber)
		after  int // the ARP requests of the cycle after that
	}{
		{"when the watch reports the address moved", func(e *env, _ *wireProber) {
			e.eng.Moved(t.Context(), guestAddr)
			e.eng.moving.Wait()
		}, 1},
		{"when the configuration of the guest changed", func(e *env, _ *wireProber) {
			e.inv.set(snapshot(webOnItsAddress("d2")))
		}, 1},
		{"when the port does not answer", func(_ *env, p *wireProber) {
			p.failDials(errors.New("connection refused"))
		}, 1},
		{"when the watch stopped", func(e *env, _ *wireProber) { e.eng.Watching(false) }, 1},
		{"when the watch started again", func(e *env, _ *wireProber) {
			e.eng.Watching(false)
			e.clock.advance(time.Second)
			e.eng.Watching(true)
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, p := wired(t)
			e.eng.Watching(true)
			e.toIntervalOf(guestAddr, time.Minute)
			e.next(t, p)
			e.next(t, p)
			require.Zero(t, e.next(t, p), "the proof stands")

			tt.change(e, p)
			p.taken()

			require.Equal(t, tt.after, e.next(t, p))
		})
	}
}

// A port that keeps failing has its address proven in every cycle, until it
// answers again and the watch vouches for the proof once more.
func TestAFailingPortIsProvenInEveryCycle(t *testing.T) {
	e, p := wired(t)
	e.eng.Watching(true)
	e.toIntervalOf(guestAddr, time.Minute)
	e.next(t, p)
	e.next(t, p)
	p.failDials(errors.New("connection refused"))

	for range 3 {
		require.Equal(t, 1, e.next(t, p))
	}
	p.failDials(nil)
	require.Equal(t, 1, e.next(t, p))
	require.Zero(t, e.next(t, p))
}

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

func TestAProofDatedAfterNowIsDue(t *testing.T) {
	v, bindings := vouchedFleet(1, t0)

	require.False(t, v.vouches(bindings[0], t0.Add(-time.Second), time.Minute))
}

func TestAProofStandsOnlyForTheNodeAndPinItWasMadeWith(t *testing.T) {
	const every = time.Minute
	t.Run("another node", func(t *testing.T) {
		v, bindings := vouchedFleet(1, t0)
		g := model.Guest{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Node: testNode, Running: true}
		v.guests = nil
		v.see(snapshot(g), t0.Add(-time.Minute))
		require.True(t, v.vouches(bindings[0], t0.Add(10*time.Second), every))

		g.Node = "pve2"
		v.see(snapshot(g), t0.Add(10*time.Second))

		require.False(t, v.vouches(bindings[0], t0.Add(10*time.Second), every))
	})

	// The pin of another bridge counts from when the watch got it: the proof
	// made in the cycle that set it, and stands only from the next one.
	t.Run("another bridge", func(t *testing.T) {
		v, bindings := vouchedFleet(1, t0)
		b := bindings[0]
		pin := egress.Pin{MAC: hardware(t, b.MAC), Bridge: "vmbr1", Port: "tap101i0"}
		v.pin(map[netip.Addr]egress.Pin{b.Addr: pin}, t0)

		require.False(t, v.vouches(b, t0.Add(10*time.Second), every), "the proof made as the pin changed")
		b.VerifiedAt = t0.Add(10 * time.Second)
		v.pin(map[netip.Addr]egress.Pin{b.Addr: pin}, t0.Add(10*time.Second))
		require.True(t, v.vouches(b, t0.Add(20*time.Second), every), "the next one")
	})
}
