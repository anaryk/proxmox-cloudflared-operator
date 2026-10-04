package engine

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

func TestAProofTheWatchVouchesForStandsUntilTheInterval(t *testing.T) {
	e, p := wired(t)
	e.eng.Watching(true)

	require.Equal(t, 1, e.next(t, p), "the first proof")
	require.Equal(t, 1, e.next(t, p), "made before the watch had the address")
	for i := range 5 {
		require.Zero(t, e.next(t, p), "%d0s old", i+1)
	}
	require.Equal(t, 1, e.next(t, p), "a minute old")
	require.Zero(t, e.next(t, p))

	t.Run("a shorter interval", func(t *testing.T) {
		e.settings(func(s *store.Settings) { s.ReverifyInterval = store.Duration(20 * time.Second) })
		require.Equal(t, 1, e.next(t, p), "20s old")
		require.Zero(t, e.next(t, p), "10s old")
		require.Equal(t, 1, e.next(t, p), "20s old")
	})
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
