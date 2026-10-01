package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestRefreshAgentUnavailableStaysComplete(t *testing.T) {
	var sawDeadline atomic.Bool
	tests := []struct {
		name  string
		setup func(src *fakeSource, inv *Inventory)
	}{
		{"agent missing", func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("No QEMU guest agent configured")
		}},
		{"agent not running", func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("QEMU guest agent is not running")
		}},
		{"agent times out", func(src *fakeSource, inv *Inventory) {
			inv.callTimeout = time.Millisecond
			src.ifaceHooks[refWeb] = func(ctx context.Context) ([]pve.GuestIface, error) {
				if _, ok := ctx.Deadline(); ok {
					sawDeadline.Store(true)
				}
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("fetching agent interfaces of qemu/101: %w", ctx.Err())
				case <-time.After(2 * time.Second): // only reached when no timeout is applied
					return nil, errors.New("agent call was never cut short")
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFake()
			inv, _ := newInventory(src, Options{})
			tt.setup(src, inv)

			snap := inv.Refresh(t.Context())

			require.True(t, snap.Complete)
			require.Empty(t, snap.Problems)
			require.Len(t, snap.Guests, 4)
			require.Empty(t, guestOf(t, snap, refWeb).Reported)
			require.Equal(t, "web-1", guestOf(t, snap, refWeb).Name)
			require.NotEmpty(t, guestOf(t, snap, refApp).Reported, "other guests are unaffected")
		})
	}
	require.True(t, sawDeadline.Load(), "interface calls carry their own deadline")
}

func TestRefreshCachesUnavailableAgentForTTL(t *testing.T) {
	src := newFake()
	src.ifaceErrs[refWeb] = agentErr("QEMU guest agent is not running")
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	clk.advance(30 * time.Second)
	snap := inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.True(t, snap.Complete)

	clk.advance(30 * time.Second)
	delete(src.ifaceErrs, refWeb)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()))
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
}

func TestRefreshForbiddenInterfacesAreOneAdvisoryProblem(t *testing.T) {
	src := newFake()
	src.ifaceErrs[refWeb] = forbiddenErr()
	src.ifaceErrs[refApp] = forbiddenErr()
	inv, clk := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Len(t, snap.Problems, 1)
	require.Contains(t, snap.Problems[0], "guest-agent privilege")
	require.Empty(t, guestOf(t, snap, refWeb).Reported)
	require.Len(t, snap.Guests, 4)

	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.True(t, snap.Complete)
	require.Len(t, snap.Problems, 1, "the line stays while the privilege is missing")
	require.Equal(t, 1, src.count("iface "+refWeb.String()), "forbidden answers are cached too")
}

func TestRefreshOtherInterfaceFailureMarksIncomplete(t *testing.T) {
	t.Run("nothing cached", func(t *testing.T) {
		src := newFake()
		src.ifaceErrs[refWeb] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "qemu/101")
		require.Empty(t, guestOf(t, snap, refWeb).Reported)
		require.Len(t, snap.Guests, 4)
	})

	t.Run("stale answer is served", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		require.True(t, first.Complete)
		src.ifaceErrs[refWeb] = serverErr()
		clk.advance(time.Minute)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Equal(t, guestOf(t, first, refWeb).Reported, guestOf(t, snap, refWeb).Reported)

		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())
		require.Equal(t, 3, src.count("iface "+refWeb.String()), "a failed fetch is retried at once")
		require.False(t, snap.Complete)
	})
}

func TestRefreshReportedCacheHonoursTTL(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.Equal(t, 1, src.count("iface "+refApp.String()))

	clk.advance(59 * time.Second)
	snap := inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported, "served from the cache")

	src.ifaces[refWeb] = []pve.GuestIface{iface("eth0", "bc:24:11:00:aa:01", "10.20.0.99")}
	clk.advance(time.Second)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()))
	require.Equal(t, 2, src.count("iface "+refApp.String()))
	require.Equal(t, ip("10.20.0.99"), guestOf(t, snap, refWeb).Reported[0].Addr)
}

func TestRefreshHonoursConfiguredReportedTTL(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{ReportedTTL: 5 * time.Second})
	inv.Refresh(t.Context())

	clk.advance(5 * time.Second)
	inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("iface "+refWeb.String()))
}

func TestRefreshStoppedOrUnwatchedGuestsGetNoInterfaceCalls(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})

	for range 3 {
		snap := inv.Refresh(t.Context())
		require.Empty(t, guestOf(t, snap, refBatch).Reported)
		require.Empty(t, guestOf(t, snap, refDB).Reported)
		clk.advance(2 * time.Minute)
	}

	require.Zero(t, src.count("iface "+refBatch.String()), "stopped")
	require.Zero(t, src.count("iface "+refDB.String()), "not watched")
}

func TestRefreshStoppingAGuestDropsItsReportedAddresses(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	src.setStatus(refWeb, "stopped")
	clk.advance(10 * time.Second)
	snap := inv.Refresh(t.Context())
	require.False(t, guestOf(t, snap, refWeb).Running)
	require.Empty(t, guestOf(t, snap, refWeb).Reported)

	src.setStatus(refWeb, "running")
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()), "no answer from before the stop is reused")
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
}

func TestRefreshReplacedGuestDropsReportedAddresses(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	src.configs[refWeb].Values["smbios1"] = "uuid=11111111-2222-4333-8444-555555555555"
	src.ifaces[refWeb] = []pve.GuestIface{iface("eth0", "bc:24:11:00:aa:09", "10.20.0.77")}
	clk.advance(10 * time.Second)
	snap := inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("iface "+refWeb.String()))
	require.Equal(t, ip("10.20.0.77"), guestOf(t, snap, refWeb).Reported[0].Addr)
}

func TestRefreshFiltersReportedAddresses(t *testing.T) {
	src := newFake()
	src.ifaces[refWeb] = []pve.GuestIface{
		iface("lo", "", "127.0.0.1"),
		iface("eth0", "bc:24:11:00:aa:01", "10.20.0.15", "127.0.0.2", "169.254.7.7"),
		iface("ens19", "", "10.20.1.15"),
		iface("docker0", "02:42:ac:11:00:01", "172.17.0.1"),
		iface("br-1a2b3c", "02:42:ac:12:00:01", "172.18.0.1"),
		iface("veth9c1", "", "172.19.0.1"),
		iface("cni0", "", "10.244.0.1"),
		iface("cali1234", "", "10.244.1.1"),
		iface("flannel.1", "", "10.244.2.0"),
		iface("virbr0", "", "192.168.122.1"),
		iface("lxcbr0", "", "10.0.3.1"),
		iface("podman0", "", "10.88.0.1"),
		iface("kube-ipvs0", "", "10.96.0.1"),
		iface("tailscale0", "", "100.64.0.5"),
		iface("wg0", "", "10.9.0.2"),
		iface("Local Area Connection", "", "10.20.2.15"),
	}
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []model.ReportedAddr{
		{Iface: "eth0", MAC: "bc:24:11:00:aa:01", Addr: ip("10.20.0.15")},
		{Iface: "ens19", MAC: "", Addr: ip("10.20.1.15")},
		{Iface: "Local Area Connection", MAC: "", Addr: ip("10.20.2.15")},
	}, guestOf(t, snap, refWeb).Reported)
}

func TestRefreshIgnoresIPv6ReportedAddresses(t *testing.T) {
	src := newFake()
	src.ifaces[refApp] = []pve.GuestIface{{
		Name:  "eth0",
		MAC:   "bc:24:11:00:bb:02",
		Addrs: []netip.Addr{ip("fd00::31"), ip("10.20.0.31")},
	}}
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.Equal(t, []model.ReportedAddr{
		{Iface: "eth0", MAC: "bc:24:11:00:bb:02", Addr: ip("10.20.0.31")},
	}, guestOf(t, snap, refApp).Reported)
}

func TestRefreshReadsContainerInterfacesFromLXC(t *testing.T) {
	src := newFake()
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.Equal(t, 1, src.count("iface "+refApp.String()))
	require.Equal(t, []model.ReportedAddr{
		{Iface: "eth0", MAC: "bc:24:11:00:bb:02", Addr: ip("10.20.0.31")},
	}, guestOf(t, snap, refApp).Reported)
}
