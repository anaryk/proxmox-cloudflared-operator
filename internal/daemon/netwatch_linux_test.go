package daemon

import (
	"context"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The watch of the links and routes runs in a network namespace of the test's
// own; nothing of the host it runs on is touched.
func TestLinuxTheGatewayIsCheckedWhenALinkOrARouteChanges(t *testing.T) {
	if os.Geteuid() != 0 {
		if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
			t.Fatal("needs root to create a network namespace")
		}
		t.Skip("needs root to create a network namespace")
	}
	changes := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, done := make(chan netns.NsHandle, 1), make(chan error, 1)
	go func() {
		// The thread stays in the namespace and is thrown away with it.
		runtime.LockOSThread()
		ns, err := netns.New()
		if err != nil {
			done <- err
			return
		}
		ready <- ns
		done <- watchLinksAndRoutes(ctx, func() { changes <- struct{}{} })
	}()
	var ns netns.NsHandle
	select {
	case ns = <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	h, err := netlink.NewHandleAt(ns)
	require.NoError(t, err)
	t.Cleanup(h.Close)
	seen := func(what string) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatalf("no change seen after %s", what)
		}
		for len(changes) > 0 {
			<-changes
		}
	}
	// Give the subscriptions a moment to be made.
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "pcotw0"}}))
	seen("a link was added")
	link, err := h.LinkByName("pcotw0")
	require.NoError(t, err)
	require.NoError(t, h.LinkSetUp(link))
	seen("a link went up")
	_, dst, err := net.ParseCIDR("198.18.0.0/16")
	require.NoError(t, err)
	require.NoError(t, h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst}))
	seen("a route was added")
	require.NoError(t, h.RouteDel(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst}))
	seen("a route was removed")

	cancel()
	require.NoError(t, <-done)
}
