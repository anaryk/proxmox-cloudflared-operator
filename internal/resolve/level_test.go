package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func TestLevelAtLeast(t *testing.T) {
	tests := []struct {
		level Level
		min   Level
		want  bool
	}{
		{"", "", true},
		{"", LevelObserved, false},
		{"", LevelPort, false},
		{LevelObserved, "", true},
		{LevelObserved, LevelObserved, true},
		{LevelObserved, LevelFiltered, false},
		{LevelObserved, LevelPort, false},
		{LevelFiltered, LevelObserved, true},
		{LevelFiltered, LevelFiltered, true},
		{LevelFiltered, LevelPort, false},
		{LevelPort, LevelObserved, true},
		{LevelPort, LevelFiltered, true},
		{LevelPort, LevelPort, true},
		// A route without a guest proves no identity: the engine exempts it
		// from the minimum, the scale does not rank it.
		{LevelManual, LevelObserved, false},
		{LevelManual, "", true},
		{"strange", LevelObserved, false},
		// A minimum that is no level is met by nothing.
		{LevelPort, LevelManual, false},
		{LevelPort, "strange", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.level)+" for "+string(tt.min), func(t *testing.T) {
			require.Equal(t, tt.want, tt.level.AtLeast(tt.min))
		})
	}
}

func TestLevelDecodesOnlyWhatAProofCanHave(t *testing.T) {
	for _, ok := range []string{`""`, `"observed"`, `"filtered"`, `"port"`} {
		var l Level
		require.NoError(t, json.Unmarshal([]byte(ok), &l), ok)
	}
	for _, bad := range []string{`"manual"`, `"Port"`, `"port "`, `"\u001b[2J"`, `"anything"`} {
		var l Level
		err := json.Unmarshal([]byte(bad), &l)
		require.Error(t, err, bad)
		require.Equal(t, "unknown identity level", err.Error(), "the value is not repeated")
		require.Empty(t, l)
	}
}

// A binding is never written with a level it could not be read back with.
func TestLevelEncodesOnlyWhatAProofCanHave(t *testing.T) {
	for _, ok := range []Level{LevelObserved, LevelFiltered, LevelPort} {
		b, err := json.Marshal(Binding{Level: ok})
		require.NoError(t, err, ok)
		require.Contains(t, string(b), `"level":"`+string(ok)+`"`)
	}
	b, err := json.Marshal(Binding{})
	require.NoError(t, err)
	require.NotContains(t, string(b), "level")
	for _, bad := range []Level{LevelManual, "Port", "anything"} {
		_, err := json.Marshal(Binding{Level: bad})
		require.ErrorIs(t, err, errUnknownLevel, bad)
	}
}

func TestResolveManualRouteThatNamesAGuestIsProven(t *testing.T) {
	s := newScenario(t)
	ref := web1Ref
	route := webRoute()
	route.Source, route.ManualID, route.Guest = model.SourceManual, "web", &ref

	res := s.resolve(t, route, nil)

	require.True(t, res.Target.Reachable)
	require.Equal(t, "manual/web", res.Target.Owner)
	require.Equal(t, LevelPort, res.Level, "a route that names a guest has its identity proven, whoever wrote it")
	require.Equal(t, LevelPort, res.Binding.Level)

	s.web().Node = "pve2"
	s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}
	res = s.resolve(t, route, res.Binding)
	require.Equal(t, LevelObserved, res.Level)
}

func requireLevel(t *testing.T, res Result, level Level) {
	t.Helper()
	require.Equal(t, level, res.Level, "level of the result")
	if level != "" && res.Binding != nil {
		require.Equal(t, level, res.Binding.Level, "level of the binding")
	}
}

func TestResolveLevels(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *scenario)
		level Level
	}{
		{name: "guest on this node", level: LevelPort},
		{
			name: "guest on another node",
			setup: func(s *scenario) {
				s.web().Node = "pve2"
				s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}
			},
			level: LevelObserved,
		},
		{
			name: "guest on a node that is not listed",
			setup: func(s *scenario) {
				s.web().Node = "pve7"
				delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))
			},
			level: LevelObserved,
		},
		{
			name:  "this node not named: the step applies to every guest",
			setup: func(s *scenario) { s.web().Node, s.settings.LocalNode = "pve2", "" },
			level: LevelPort,
		},
		{
			name:  "guest node not known: the step applies",
			setup: func(s *scenario) { s.web().Node = "" },
			level: LevelPort,
		},
		{
			name: "no nodes known: the step applies",
			setup: func(s *scenario) {
				s.web().Node = "pve2"
				s.nodes = nil
			},
			level: LevelPort,
		},
		{
			name: "several answering MACs, each on its own port",
			setup: func(s *scenario) {
				s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
				s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
				s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"tap101i1"}
			},
			level: LevelPort,
		},
		{
			name: "trusted static address",
			setup: func(s *scenario) {
				s.settings.TrustStatic = true
				s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")
				s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.40.0.10")}
			},
			level: LevelObserved,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			if tt.setup != nil {
				tt.setup(s)
			}

			res := s.resolve(t, webRoute(), nil)

			require.True(t, res.Target.Reachable, res.Target.Reason)
			requireLevel(t, res, tt.level)
		})
	}
}

func TestResolveLevelOfATrustedStaticAddress(t *testing.T) {
	s := newScenario(t)
	s.settings.TrustStatic = true
	s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.40.0.10")}

	res := s.resolve(t, webRoute(), nil)

	requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
	require.Empty(t, s.prober.ops("arp"), "nothing answered ARP: the configuration vouches for it")
	require.Empty(t, s.prober.ops("fdb"))
}

func TestResolveUnverifiedTargetHasNoLevel(t *testing.T) {
	t.Run("nothing answers", func(t *testing.T) {
		s := newScenario(t)
		delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, "no ARP answer on vmbr0")
		require.Empty(t, res.Level)
	})
	t.Run("port not answering, nothing bound", func(t *testing.T) {
		s := newScenario(t)
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolve(t, webRoute(), nil)

		require.Empty(t, res.Level)
	})
	t.Run("bound address withdrawn", func(t *testing.T) {
		s := newScenario(t)
		delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))

		res := s.resolve(t, webRoute(), atLevel(boundTo("10.20.0.10"), LevelPort))

		require.True(t, res.Target.Withdrawn)
		require.Empty(t, res.Level)
	})
	t.Run("guest stopped", func(t *testing.T) {
		s := newScenario(t)
		s.web().Running = false

		res := s.resolve(t, webRoute(), atLevel(boundTo("10.20.0.10"), LevelPort))

		require.True(t, res.Target.Withdrawn)
		require.Empty(t, res.Level)
	})
	t.Run("rejected", func(t *testing.T) {
		s := newScenario(t)

		res := s.resolve(t, routeFor(ip("10.20.0.2"), ""), nil)

		require.True(t, res.Target.Rejected)
		require.Empty(t, res.Level)
	})
}

// atLevel returns a copy of b proven at level.
func atLevel(b *Binding, level Level) *Binding {
	c := *b
	c.Level = level
	return &c
}

func TestResolvePortFailureKeepsTheLevelJustProven(t *testing.T) {
	s, prev := stickyScenario(t)
	prev = atLevel(prev, LevelObserved)

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, atLevel(provenAt(failingAt(prev, t0), t0), LevelPort), res.Binding, "the identity was proven again, on its port")
	require.Equal(t, LevelPort, res.Level)
}

func TestResolveBindingKeepsItsLevel(t *testing.T) {
	arpFails := func(s *scenario) { s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed") }
	unknown := func(s *scenario) { s.web().Running, s.web().StatusUnknown = false, true }
	tests := []struct {
		name   string
		setup  func(s *scenario)
		cancel bool
		stored Level // of the binding passed in
		level  Level // of the result
	}{
		{name: "prober error on a binding proven at port", setup: arpFails, stored: LevelPort, level: LevelPort},
		{name: "prober error on a binding proven at observed", setup: arpFails, stored: LevelObserved, level: LevelObserved},
		{name: "guest state unknown", setup: unknown, stored: LevelPort, level: LevelPort},
		{name: "cancelled", cancel: true, stored: LevelPort, level: LevelPort},
		{name: "prober error on a binding of an older version", setup: arpFails, level: LevelObserved},
		{name: "guest state unknown on a binding of an older version", setup: unknown, level: LevelObserved},
		{name: "cancelled on a binding of an older version", cancel: true, level: LevelObserved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			if tt.setup != nil {
				tt.setup(s)
			}
			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			prev := atLevel(boundTo("10.20.0.10"), tt.stored)

			res := s.resolveCtx(ctx, webRoute(), prev)

			require.Equal(t, ip("10.20.0.10"), res.Target.Addr)
			require.False(t, res.Target.Withdrawn)
			require.Equal(t, tt.level, res.Level)
			require.Equal(t, tt.stored, res.Binding.Level, "the binding says no more than its proof did")
		})
	}
}

func TestResolveBindingOfAnOlderVersionIsProvenAgain(t *testing.T) {
	s := newScenario(t)
	prev := throughJSON(t, boundTo("10.20.0.10"))
	require.Empty(t, prev.Level)

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
}

func TestResolveLevelFollowsTheGuestToAnotherNode(t *testing.T) {
	s := newScenario(t)
	s.web().Node = "pve2"
	s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}

	res := s.resolve(t, webRoute(), atLevel(boundTo("10.20.0.10"), LevelPort))

	requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
}

func TestResolveLevelOfARouteWithoutGuest(t *testing.T) {
	route := model.Route{
		Hostname: "app.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Addr: ip("10.20.0.50"), Port: 8080},
		Source:   model.SourceManual,
		ManualID: "app",
	}
	t.Run("answering", func(t *testing.T) {
		s := newScenario(t)

		res := s.resolve(t, route, nil)

		require.True(t, res.Target.Reachable)
		require.Equal(t, LevelManual, res.Level)
	})
	t.Run("on a node with allowNode", func(t *testing.T) {
		s := newScenario(t)
		r := route
		r.Target.Addr, r.Options.AllowNode = ip("10.20.0.2"), true

		res := s.resolve(t, r, nil)

		require.True(t, res.Target.Reachable)
		require.Equal(t, LevelManual, res.Level)
	})
	t.Run("not answering", func(t *testing.T) {
		s := newScenario(t)
		s.prober.dialErr[ip("10.20.0.50")] = errDial

		res := s.resolve(t, route, nil)

		require.False(t, res.Target.Reachable)
		require.Equal(t, LevelManual, res.Level)
	})
	t.Run("rejected", func(t *testing.T) {
		s := newScenario(t)
		r := route
		r.Target.Addr = ip("10.20.0.2")

		res := s.resolve(t, r, nil)

		require.True(t, res.Target.Rejected)
		require.Empty(t, res.Level)
	})
	t.Run("nothing learned", func(t *testing.T) {
		s := newScenario(t)
		s.prober.ifacesErr = errors.New("netlink closed")

		res := s.resolve(t, route, nil)

		require.True(t, res.Target.Withdrawn)
		require.Empty(t, res.Level)
	})
}
