package resolve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// apiRoute is a second route of web-1, on the same port as webRoute unless
// port says otherwise.
func apiRoute(port uint16) model.Route {
	route := webRoute()
	route.Hostname, route.Target.Port = "api.example.com", port
	return route
}

// countOps counts the calls of each op.
func (f *fakeProber) countOps() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, c := range f.calls {
		out[c.op]++
	}
	return out
}

// carrying is a binding of webRoute proven at port 30 seconds before t0,
// whose proof the share may let stand.
func carrying() *Binding {
	return &Binding{
		Owner: webOwner, Hostname: webHost, Guest: webOwner, Addr: ip("10.20.0.10"), MAC: mac0,
		VerifiedAt: t0.Add(-30 * time.Second), Level: LevelPort, Since: t0.Add(-time.Hour),
		Bridge: "vmbr0", Port: "tap101i0", Ports: map[string]string{mac0: "tap101i0"},
	}
}

func (s *scenario) resolveShared(t *testing.T, route model.Route, prev *Binding, share *Shared) Result {
	t.Helper()
	r := NewResolver(s.prober, s.settings, s.clock.now)
	return r.Resolve(t.Context(), route, s.snapshot(), prev, s.deny, s.required, share)
}

func TestRoutesThatShareAnAddressShareOneProof(t *testing.T) {
	tests := []struct {
		name  string
		share func() *Shared
		api   uint16
		want  map[string]int
	}{
		{"nothing shared", func() *Shared { return nil }, 80,
			map[string]int{"interfaces": 2, "route": 2, "arp": 2, "fdb": 2, "dial": 2}},
		{"one port", func() *Shared { return NewShared(nil) }, 80,
			map[string]int{"interfaces": 1, "route": 1, "arp": 1, "fdb": 1, "dial": 1}},
		{"two ports", func() *Shared { return NewShared(nil) }, 8080,
			map[string]int{"interfaces": 1, "route": 1, "arp": 1, "fdb": 1, "dial": 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			share := tt.share()

			web := s.resolveShared(t, webRoute(), nil, share)
			api := s.resolveShared(t, apiRoute(tt.api), nil, share)

			requireServed(t, web, "10.20.0.10", t0)
			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, api.Target)
			require.Equal(t, LevelPort, api.Level)
			require.Equal(t, tt.want, s.prober.countOps())
		})
	}
}

func TestConcurrentRoutesOfOneAddressWaitForOneProof(t *testing.T) {
	s := newScenario(t)
	r := NewResolver(s.prober, s.settings, s.clock.now)
	snap, share := s.snapshot(), NewShared(nil)
	results := make([]Result, 16)

	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() { results[i] = r.Resolve(t.Context(), webRoute(), snap, nil, s.deny, s.required, share) })
	}
	wg.Wait()

	for _, res := range results {
		requireServed(t, res, "10.20.0.10", t0)
	}
	require.Equal(t, map[string]int{"interfaces": 1, "route": 1, "arp": 1, "fdb": 1, "dial": 1}, s.prober.countOps())
}

// A call cancelled while it proved an address hands nothing on: the next one
// asks the wire itself.
func TestACancelledProofIsNotShared(t *testing.T) {
	s := newScenario(t)
	ctx, cancel := context.WithCancel(t.Context())
	s.prober.cancelOn, s.prober.cancel = "arp", cancel
	r := NewResolver(s.prober, s.settings, s.clock.now)
	share := NewShared(nil)

	first := r.Resolve(ctx, webRoute(), s.snapshot(), nil, s.deny, s.required, share)
	s.prober.cancel = nil
	second := r.Resolve(t.Context(), apiRoute(80), s.snapshot(), nil, s.deny, s.required, share)

	require.Equal(t, reasonCancelled, first.Target.Reason)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, second.Target)
	require.Equal(t, 2, s.prober.countOps()["arp"])
}

func TestAProofTheShareLetsStandIsNotMadeAgain(t *testing.T) {
	s := newScenario(t)
	var asked []Binding
	share := NewShared(func(b Binding) bool { asked = append(asked, b); return true })

	res := s.resolveShared(t, webRoute(), carrying(), share)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, carrying(), res.Binding, "the binding keeps the time of its proof")
	require.Equal(t, LevelPort, res.Level)
	require.Equal(t, []Binding{*carrying()}, asked)
	require.Equal(t, map[string]int{"interfaces": 1, "dial": 1}, s.prober.countOps(), "nothing is asked of the wire")
}

func TestAProofIsMadeAgain(t *testing.T) {
	yes := func(Binding) bool { return true }
	tests := []struct {
		name  string
		share *Shared
		prev  func() *Binding
		setup func(s *scenario)
	}{
		{"when nothing is shared", nil, carrying, nil},
		{"when the share does not let it stand", NewShared(func(Binding) bool { return false }), carrying, nil},
		{"when the share has no say", NewShared(nil), carrying, nil},
		{"when the binding is failing", NewShared(yes), func() *Binding { return failingAt(carrying(), t0.Add(-20*time.Second)) }, nil},
		{"when the binding is withdrawn", NewShared(yes), func() *Binding { return withdrawnAt(carrying(), t0.Add(-20*time.Second)) }, nil},
		{"when the binding kept no level", NewShared(yes), func() *Binding { b := carrying(); b.Level = ""; return b }, nil},
		{"when the proof is too old", NewShared(yes), func() *Binding { return provenAt(carrying(), t0.Add(-6*time.Minute)) }, nil},
		{"when the guest runs elsewhere now", NewShared(yes), carrying, func(s *scenario) { s.web().Node = "pve2" }},
		// The watch looks for a MAC in the forwarding table of the bridge the
		// proof placed it on, and only there.
		{"when the proof is observed", NewShared(yes), func() *Binding { b := carrying(); b.Level = LevelObserved; return b }, nil},
		{"when the proof placed the MAC on no bridge", NewShared(yes), func() *Binding { b := carrying(); b.Bridge = ""; return b }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			if tt.setup != nil {
				tt.setup(s)
			}

			s.resolveShared(t, webRoute(), tt.prev(), tt.share)

			require.Equal(t, 1, s.prober.countOps()["arp"])
		})
	}
}

// A port that stops answering may be on someone else's address: the identity
// is proven again in the same call, and a failure withdraws the address.
func TestADialFailureProvesAReusedAddressAgain(t *testing.T) {
	share := func() *Shared { return NewShared(func(Binding) bool { return true }) }

	t.Run("the identity holds", func(t *testing.T) {
		s := newScenario(t)
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolveShared(t, webRoute(), carrying(), share())

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
		require.Equal(t, t0, res.Binding.VerifiedAt, "proven anew")
		require.Equal(t, map[string]int{"interfaces": 1, "route": 1, "arp": 1, "fdb": 1, "dial": 1}, s.prober.countOps())
	})

	t.Run("the identity is lost", func(t *testing.T) {
		s := newScenario(t)
		s.prober.dialErr[ip("10.20.0.10")] = errDial
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}

		res := s.resolveShared(t, webRoute(), carrying(), share())

		require.True(t, res.Target.Withdrawn)
		require.Equal(t, "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest", res.Target.Reason)
	})
}

// A MAC that another guest which runs has configured too passes only on the
// forwarding table, so a proof of it never stands without the wire.
func TestAMACOfTwoGuestsIsProvenOnTheWire(t *testing.T) {
	s := newScenario(t)
	s.addDB1(true, nicOn(0, mac0, "vmbr0", 0))

	res := s.resolveShared(t, webRoute(), carrying(), NewShared(func(Binding) bool { return true }))

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, t0, res.Binding.VerifiedAt)
	require.Equal(t, 1, s.prober.countOps()["fdb"])

	s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap102i0"}

	res = s.resolveShared(t, webRoute(), carrying(), NewShared(func(Binding) bool { return true }))

	require.True(t, res.Target.Withdrawn)
	require.Equal(t, "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port", res.Target.Reason)
}
