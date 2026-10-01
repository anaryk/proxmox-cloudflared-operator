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

	clk.advance(14 * time.Second)
	snap := inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.True(t, snap.Complete)

	clk.advance(time.Second)
	delete(src.ifaceErrs, refWeb)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()), "a guest without an agent is asked again after 15s")
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
}

// hangingAgent makes the interface call of web-1 wait for its deadline.
func hangingAgent(src *fakeSource, inv *Inventory) {
	inv.callTimeout = time.Millisecond
	src.ifaceHooks[refWeb] = func(ctx context.Context) ([]pve.GuestIface, error) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("fetching agent interfaces of qemu/101: %w", ctx.Err())
		case <-time.After(2 * time.Second): // only reached when no timeout is applied
			return nil, errors.New("agent call was never cut short")
		}
	}
}

func TestRefreshReportedCacheLifetimes(t *testing.T) {
	tests := []struct {
		name  string
		opts  Options
		setup func(src *fakeSource, inv *Inventory)
		life  time.Duration
	}{
		{"agent unavailable", Options{}, func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("No QEMU guest agent configured")
		}, 15 * time.Second},
		{"forbidden", Options{}, func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = forbiddenErr()
		}, 15 * time.Second},
		{"unexpected error", Options{}, func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = serverErr()
		}, 15 * time.Second},
		{"empty answer", Options{}, func(src *fakeSource, _ *Inventory) {
			src.ifaces[refWeb] = []pve.GuestIface{iface("lo", "", "127.0.0.1")}
		}, 15 * time.Second},
		{"answer with addresses", Options{}, func(*fakeSource, *Inventory) {}, time.Minute},
		{"timeout", Options{}, hangingAgent, time.Minute},
		{"agent unavailable, short ttl", Options{ReportedTTL: 5 * time.Second}, func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("QEMU guest agent is not running")
		}, 5 * time.Second},
		{"empty answer, short ttl", Options{ReportedTTL: 5 * time.Second}, func(src *fakeSource, _ *Inventory) {
			src.ifaces[refWeb] = nil
		}, 5 * time.Second},
		{"timeout, short ttl", Options{ReportedTTL: 5 * time.Second}, hangingAgent, 5 * time.Second},
		{"answer with addresses, ttl above the cap", Options{ReportedTTL: 5 * time.Minute}, func(*fakeSource, *Inventory) {}, 5 * time.Minute},
		{"agent unavailable, ttl above the cap", Options{ReportedTTL: 5 * time.Minute}, func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("QEMU guest agent is not running")
		}, 15 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFake()
			inv, clk := newInventory(src, tt.opts)
			tt.setup(src, inv)
			call := "iface " + refWeb.String()

			inv.Refresh(t.Context())
			require.Equal(t, 1, src.count(call))

			clk.advance(tt.life - time.Second)
			inv.Refresh(t.Context())
			require.Equal(t, 1, src.count(call), "still cached just before %s", tt.life)

			clk.advance(time.Second)
			inv.Refresh(t.Context())
			require.Equal(t, 2, src.count(call), "asked again after %s", tt.life)
		})
	}
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

func TestRefreshOtherInterfaceFailureIsAdvisory(t *testing.T) {
	t.Run("nothing cached", func(t *testing.T) {
		src := newFake()
		src.ifaceErrs[refWeb] = serverErr()
		inv, clk := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "qemu/101")
		require.Contains(t, snap.Problems[0], "interfaces not refreshed")
		require.Empty(t, guestOf(t, snap, refWeb).Reported)
		require.Len(t, snap.Guests, 4)
		require.NotEmpty(t, guestOf(t, snap, refApp).Reported)

		clk.advance(14 * time.Second)
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Len(t, snap.Problems, 1, "the line stays while the failed answer is cached")
		require.Equal(t, 1, src.count("iface "+refWeb.String()))

		delete(src.ifaceErrs, refWeb)
		clk.advance(time.Second)
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
		require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
	})

	t.Run("a failure replaces the old answer", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		require.NotEmpty(t, guestOf(t, first, refWeb).Reported)
		src.ifaceErrs[refWeb] = serverErr()
		clk.advance(time.Minute)

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Empty(t, guestOf(t, snap, refWeb).Reported, "an answer that cannot be renewed is not served")
	})

	t.Run("one line per guest", func(t *testing.T) {
		src := newFake()
		src.ifaceErrs[refWeb] = serverErr()
		src.ifaceErrs[refApp] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete)
		require.Len(t, snap.Problems, 2)
		require.Contains(t, snap.Problems[0], "lxc/201")
		require.Contains(t, snap.Problems[1], "qemu/101")
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
