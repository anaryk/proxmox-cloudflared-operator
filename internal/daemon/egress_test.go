package daemon

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestTheDaemonGivesTheVerifiedTargetsToTheEgressFilter(t *testing.T) {
	w := newWorld(t)
	d := w.start()

	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	scripts := w.nft.applied()
	require.NotEmpty(t, scripts)
	last := scripts[len(scripts)-1]
	require.Contains(t, last, "table inet pco_egress {")
	require.Contains(t, last, guestAddress+" . 8080")
	require.Contains(t, last, "meta skuid 986 jump connector")
	require.Contains(t, last, "10.20.0.1\n", "the resolvers of the node")
}

// The connector user may be created after the daemon started: until then no
// rule is written for a target the connectors cannot be confined to.
func TestWithoutTheConnectorUserNoTunnelConfigurationIsWritten(t *testing.T) {
	w := newWorld(t)
	noUser := errors.New("user pco-connector does not exist: install the pco package or run systemd-sysusers")
	var created atomic.Bool
	w.deps.ConnectorUID = func() (uint32, error) {
		if !created.Load() {
			return 0, noUser
		}
		return testConnectorUID, nil
	}
	d := w.start()
	_, err := d.client.Apply(t.Context(), false, "")
	require.NoError(t, err)

	st := d.await(func(st engine.State) bool { return st.Mode == "enforce" && len(st.Problems) > 0 })

	require.Contains(t, st.Problems, "setting the egress filter: "+noUser.Error()+"; no tunnel configuration is written until it is set")
	require.Empty(t, w.nft.applied())
	for _, call := range w.cf.Calls() {
		require.False(t, strings.HasPrefix(call, "CreateTunnel") || strings.HasPrefix(call, "PutTunnelConfig"), call)
	}

	created.Store(true)
	require.NoError(t, d.client.Sync(t.Context()))
	d.await(func(st engine.State) bool { return len(st.Tunnels) == 1 && st.Tunnels[0].Verified })
	require.NotEmpty(t, w.nft.applied())
}

// The watch is given the pins the resolver proved, and a move it reports
// takes the address out of the table at once; the verification that follows
// puts it back.
func TestTheDaemonBlocksAnAddressWhoseMACMoved(t *testing.T) {
	w := newWorld(t)
	d := w.start()
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })
	bound, onMove := w.watch.running(t)

	hw, err := net.ParseMAC(guestMAC)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return reflect.DeepEqual(map[netip.Addr]egress.Pin{
			netip.MustParseAddr(guestAddress): {MAC: hw, Bridge: "vmbr0", Port: "tap101i0"},
		}, bound())
	}, 10*time.Second, 5*time.Millisecond, "%v", bound())
	before := len(w.nft.applied())

	onMove(netip.MustParseAddr(guestAddress))

	require.Eventually(t, func() bool { return len(w.nft.applied()) >= before+2 }, 10*time.Second, 5*time.Millisecond)
	scripts := w.nft.applied()[before:]
	require.Equal(t, "delete element inet pco_egress targets4 { "+guestAddress+" . 8080 }\n", scripts[0])
	require.Contains(t, scripts[1], guestAddress+" . 8080", "verified again and back")
}

func TestTheFilterOfAnUnknownUserIsNoFailureWhileItIsOff(t *testing.T) {
	w := newWorld(t)
	w.deps.ConnectorUID = func() (uint32, error) { return 0, errors.New("no such user") }
	require.NoError(t, egress.NewOverrides(w.paths.Local).SwitchOff(t0))
	d := w.start()

	st := d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	require.Empty(t, st.Problems)
	require.Empty(t, w.nft.applied())
}
