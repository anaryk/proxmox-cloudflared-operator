package gateway

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func leaseState(digest, holder string) engine.State {
	return engine.State{
		Digest: digest, Node: "pve1",
		Lease: &engine.LeaseView{Holder: holder, Address: netip.MustParseAddr("10.9.0.2")},
		Nodes: []engine.NodeView{{Name: "pve2", Kind: "host", AgentAddress: netip.MustParseAddr("10.9.0.2")}},
	}
}

// settled waits for the role to be what the daemon's state says.
func settled(t *testing.T, s *testServer, controller bool, addr string, ok bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		c, a, o := s.gw.Role()
		return c == controller && a == addr && o == ok
	}, 5*time.Second, 10*time.Millisecond, "the role follows the state")
}

func TestTheRoleComesFromTheLeaseOfTheStateWithoutHoldingThePage(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(leaseState("d1", "pve2"))

	// Nothing is held yet: the page is served here, and the state is read
	// in the background, once.
	controller, addr, ok := s.gw.Role()
	require.False(t, controller)
	require.Empty(t, addr)
	require.False(t, ok)
	settled(t, s, false, "10.9.0.2", true)
	require.Equal(t, 1, s.daemon.count("GET /v1/state"))

	// Without a stream the state held cannot be known current, but a page
	// a moment later starts no second read.
	_, _, _ = s.gw.Role()
	_, _, _ = s.gw.Role()
	require.Equal(t, 1, s.daemon.count("GET /v1/state"), "the refresh is rate-limited")
}

func TestTheRoleFollowsTheDigestOfTheStream(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(leaseState("d1", "pve2"))
	s.run()
	conn := s.daemon.stream.next(t)
	require.Eventually(t, func() bool { return s.gw.hub.digest() == "d1" }, 5*time.Second, 10*time.Millisecond, "the gateway took the hello")
	reads := s.daemon.count("GET /v1/state")

	// The lease has not moved: the state held is current, and is not read
	// again for every page.
	controller, _, ok := s.gw.Role()
	require.False(t, controller)
	require.True(t, ok)
	require.Equal(t, reads, s.daemon.count("GET /v1/state"))

	s.daemon.serveState(leaseState("d2", "pve1"))
	conn.send <- sse("", "state", engine.StateNotice{At: t0, Digest: "d2"})
	settled(t, s, true, "", false)
}

func TestTheRoleOfASingleNodeAndOfAHolderWithoutAnAddress(t *testing.T) {
	for _, tt := range []struct {
		name       string
		state      engine.State
		controller bool
		addr       string
		ok         bool
	}{
		{"no lease", engine.State{Digest: "d1", Node: "pve1"}, true, "", false},
		{"this node holds it", leaseState("d1", "pve1"), true, "", false},
		{"the holder has no address", engine.State{Digest: "d1", Node: "pve1", Lease: &engine.LeaseView{Holder: "pve2"}}, false, "", false},
		{"nobody holds it", engine.State{Digest: "d1", Node: "pve1", Lease: &engine.LeaseView{}}, false, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			s.daemon.serveState(tt.state)
			settled(t, s, tt.controller, tt.addr, tt.ok)
		})
	}
}

func TestOnlyTheRegisteredAddressOfTheHolderIsRedirectedTo(t *testing.T) {
	t.Run("the registry names another address for the holder", func(t *testing.T) {
		s := newTestServer(t)
		st := leaseState("d1", "pve2")
		st.Nodes[0].AgentAddress = netip.MustParseAddr("10.9.0.3")
		s.daemon.serveState(st)
		settled(t, s, false, "", false)
	})
	t.Run("the holder is not in the registry the state shows", func(t *testing.T) {
		s := newTestServer(t)
		st := leaseState("d1", "pve2")
		st.Nodes = nil
		s.daemon.serveState(st)
		settled(t, s, false, "10.9.0.2", true)
	})
	t.Run("the holder's registered address is not one to send a browser to", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1", "0.0.0.0", "224.0.0.1", "::1", "fe80::1%eth0"} {
			s := newTestServer(t)
			st := leaseState("d1", "pve2")
			st.Lease.Address = netip.MustParseAddr(addr)
			st.Nodes[0].AgentAddress = st.Lease.Address
			s.daemon.serveState(st)
			settled(t, s, false, "", false)
		}
	})
}

func TestTheControllersAddressOfARefusalReachesThePage(t *testing.T) {
	s := newTestServer(t)
	s.daemon.on("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Pco-Controller", "10.9.0.2")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"this node is not the controller; run it on pve2 (10.9.0.2)","code":"not_controller"}`))
	})
	b := s.signIn(admin)
	rec := b.send(b.request(http.MethodPost, "/api/v1/sync", "{}"))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "10.9.0.2", rec.Header().Get("Pco-Controller"))
	require.Contains(t, rec.Body.String(), `"not_controller"`)
}

func TestWithoutTheDaemonTheRoleIsNotKnownAtOnce(t *testing.T) {
	s := newTestServer(t)
	s.daemon.close()
	start := time.Now()
	controller, addr, ok := s.gw.Role()
	require.Less(t, time.Since(start), 500*time.Millisecond, "a page never waits for the daemon")
	require.False(t, controller)
	require.Empty(t, addr)
	require.False(t, ok)
}
