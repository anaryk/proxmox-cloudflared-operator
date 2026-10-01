package inventory

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefreshUnreadableStatusKeepsLastKnownState(t *testing.T) {
	const line = "status of 1 guests is unknown; using last known state"
	tests := []struct {
		name    string
		before  string // status at the first refresh
		after   string
		running bool
	}{
		{name: "running guest turns unknown", before: "running", after: "unknown", running: true},
		{name: "stopped guest turns unknown", before: "stopped", after: "unknown", running: false},
		{name: "running guest turns paused", before: "running", after: "paused", running: true},
		{name: "stopped guest turns paused", before: "stopped", after: "paused", running: false},
		{name: "running guest turns suspended", before: "running", after: "suspended", running: true},
		{name: "running guest loses its status", before: "running", after: "", running: true},
		{name: "running guest in another spelling", before: "running", after: "Running", running: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFake()
			src.setStatus(refWeb, tt.before)
			inv, clk := newInventory(src, Options{})
			inv.Refresh(t.Context())
			src.setStatus(refWeb, tt.after)
			clk.advance(10 * time.Second)

			snap := inv.Refresh(t.Context())

			require.True(t, snap.Complete)
			require.Equal(t, []string{line}, snap.Problems)
			web := guestOf(t, snap, refWeb)
			require.Equal(t, tt.running, web.Running)
			require.Equal(t, tt.running, len(web.Reported) > 0, "a guest taken as running keeps its reported addresses")
		})
	}
}

func TestRefreshUnreadableStatusFollowsTheLastReadOne(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	steps := []struct {
		status  string
		running bool
	}{
		{"running", true},
		{"unknown", true},
		{"unknown", true},
		{"stopped", false},
		{"unknown", false},
		{"running", true},
		{"paused", true},
	}
	for i, st := range steps {
		src.setStatus(refWeb, st.status)

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete, "step %d", i)
		require.Equal(t, st.running, guestOf(t, snap, refWeb).Running, "step %d: %s", i, st.status)
		if st.status == "running" || st.status == "stopped" {
			require.Empty(t, snap.Problems, "step %d", i)
		} else {
			require.Equal(t, []string{"status of 1 guests is unknown; using last known state"}, snap.Problems, "step %d", i)
		}
		clk.advance(10 * time.Second)
	}
}

func TestRefreshUnreadableStatusOfANewGuest(t *testing.T) {
	src := newFake()
	src.setStatus(refWeb, "unknown")
	src.setStatus(refApp, "unknown")
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []string{"status of 2 guests is unknown; using last known state"}, snap.Problems, "one line per refresh")
	require.False(t, guestOf(t, snap, refWeb).Running)
	require.False(t, guestOf(t, snap, refApp).Running)
	require.Zero(t, src.count("iface "+refWeb.String()))
	require.True(t, guestOf(t, snap, refDB).Running)
}

func TestRefreshUnreadableStatusOfAReplacedGuest(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	first := inv.Refresh(t.Context())
	require.True(t, guestOf(t, first, refWeb).Running)
	src.configs[refWeb].Values["smbios1"] = "uuid=11111111-2222-4333-8444-555555555555"
	src.setStatus(refWeb, "unknown")
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	web := guestOf(t, snap, refWeb)
	require.NotEqual(t, guestOf(t, first, refWeb).Identity, web.Identity)
	require.False(t, web.Running, "a new guest under the same vmid does not inherit the state of the old one")
	require.Equal(t, []string{"status of 1 guests is unknown; using last known state"}, snap.Problems)
}

func TestRefreshUnreadableStatusAfterAFailedListing(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	src.resourcesErr = serverErr()
	clk.advance(10 * time.Second)
	inv.Refresh(t.Context())
	src.resourcesErr = nil
	src.setStatus(refWeb, "unknown")
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.True(t, guestOf(t, snap, refWeb).Running, "a failed listing does not lose the last known state")
}
