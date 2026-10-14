package gateway

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func leaseState(digest, holder string, valid bool) engine.State {
	return engine.State{
		Digest: digest, Node: "pve1",
		Lease: &engine.LeaseView{Holder: holder, Address: netip.MustParseAddr("10.9.0.2"), Valid: valid},
	}
}

func TestTheRoleComesFromTheLeaseOfTheState(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(leaseState("d1", "pve2", true))

	controller, addr, ok := s.gw.Role()
	require.False(t, controller)
	require.Equal(t, "10.9.0.2", addr)
	require.True(t, ok)
	require.Equal(t, 1, s.daemon.count("GET /v1/state"), "read once for the role")
}

func TestTheRoleFollowsTheDigestOfTheStream(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(leaseState("d1", "pve2", true))
	s.run()
	conn := s.daemon.stream.next(t)
	require.Eventually(t, func() bool { return s.gw.hub.digest() == "d1" }, 5e9, 1e7, "the gateway took the hello")
	reads := s.daemon.count("GET /v1/state")

	// The lease has not moved: the state held is current, and is not read
	// again for every page.
	controller, _, ok := s.gw.Role()
	require.False(t, controller)
	require.True(t, ok)
	require.Equal(t, reads, s.daemon.count("GET /v1/state"))

	s.daemon.serveState(leaseState("d2", "pve1", true))
	conn.send <- sse("", "state", engine.StateNotice{At: t0, Digest: "d2"})
	require.Eventually(t, func() bool {
		controller, _, _ := s.gw.Role()
		return controller
	}, 5e9, 1e7, "the role follows the state the stream announced")
}

func TestTheRoleOfASingleNodeAndOfASilentHolder(t *testing.T) {
	for _, tt := range []struct {
		name       string
		state      engine.State
		controller bool
		addr       string
		ok         bool
	}{
		{"no lease", engine.State{Digest: "d1", Node: "pve1"}, true, "", false},
		{"this node holds it", leaseState("d1", "pve1", true), true, "", false},
		{"the holder is silent", leaseState("d1", "pve2", false), false, "10.9.0.2", false},
		{"nobody holds it", engine.State{Digest: "d1", Node: "pve1", Lease: &engine.LeaseView{}}, false, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			s.daemon.serveState(tt.state)
			controller, addr, ok := s.gw.Role()
			require.Equal(t, tt.controller, controller)
			require.Equal(t, tt.addr, addr)
			require.Equal(t, tt.ok, ok)
		})
	}
}

func TestWithoutTheDaemonTheRoleIsNotKnown(t *testing.T) {
	s := newTestServer(t)
	s.daemon.close()
	controller, addr, ok := s.gw.Role()
	require.False(t, controller)
	require.Empty(t, addr)
	require.False(t, ok)
}
