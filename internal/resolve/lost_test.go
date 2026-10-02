package resolve

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// twoAddresses is web-1 with 10.20.0.10 and 10.20.0.11 on net0, both
// answering from its own port; the route is bound to the second.
func twoAddresses(t *testing.T) (*scenario, *Binding) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10", "10.20.0.11")}
	s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
	return s, atLevel(boundTo("10.20.0.11"), LevelPort)
}

func TestLostProof(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *scenario, prev *Binding) *Binding
		lost  bool
	}{
		{name: "a stranger answers for the bound address and another one is bound", lost: true, setup: func(s *scenario, prev *Binding) *Binding {
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
			return prev
		}},
		{name: "a stranger answers for the bound address and nothing else passes", lost: true, setup: func(s *scenario, prev *Binding) *Binding {
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
			delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))
			return prev
		}},
		{name: "the bound address may never be served", lost: true, setup: func(s *scenario, prev *Binding) *Binding {
			s.deny = denylist(t, ips("10.20.0.2"), prefixes("10.20.0.11/32"))
			return prev
		}},
		{name: "the guest stopped", lost: true, setup: func(s *scenario, prev *Binding) *Binding {
			s.web().Running = false
			return prev
		}},
		{name: "the proof is in doubt", lost: true, setup: func(s *scenario, prev *Binding) *Binding {
			s.prober.ifacesErr = errors.New("netlink: no buffer space")
			return provenAt(prev, t0.Add(-time.Hour))
		}},
		{name: "a prober error with a fresh proof", setup: func(s *scenario, prev *Binding) *Binding {
			s.prober.ifacesErr = errors.New("netlink: no buffer space")
			return prev
		}},
		{name: "the port does not answer and another address does", setup: func(s *scenario, prev *Binding) *Binding {
			s.prober.dialErr[ip("10.20.0.11")] = errDial
			return failingAt(prev, t0.Add(-time.Hour))
		}},
		{name: "the bound address passes", setup: func(_ *scenario, prev *Binding) *Binding { return prev }},
		{name: "withdrawn before", setup: func(s *scenario, prev *Binding) *Binding {
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
			return withdrawnAt(prev, t0.Add(-time.Minute))
		}},
		{name: "a binding of another route", setup: func(s *scenario, prev *Binding) *Binding {
			// The address that route was bound to is the first candidate of
			// this one, and fails.
			s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}
			other := *prev
			other.Hostname, other.Addr = "other.example.com", ip("10.20.0.10")
			return &other
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, prev := twoAddresses(t)
			prev = tc.setup(s, prev)

			res := s.resolve(t, webRoute(), prev)
			addr, lost := LostProof(webRoute(), prev, res)

			require.Equal(t, tc.lost, lost, "target %+v, candidates %+v", res.Target, res.Candidates)
			if lost {
				require.Equal(t, ip("10.20.0.11"), addr)
			}
		})
	}
}

// A binding moves up to an address proven at a higher level: the one it
// leaves passed, and lost nothing.
func TestLostProofOfABindingThatMovesUp(t *testing.T) {
	s := trustedFirst(t)
	prev := atLevel(boundTo("10.40.0.10"), LevelObserved)

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
	_, lost := LostProof(webRoute(), prev, res)
	require.False(t, lost)
}

func TestLostProofWithoutABinding(t *testing.T) {
	s := newScenario(t)

	_, lost := LostProof(webRoute(), nil, s.resolve(t, webRoute(), nil))

	require.False(t, lost)
}
