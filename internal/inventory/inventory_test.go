package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestRefreshFirstFetchesEveryConfig(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Empty(t, snap.Problems)
	require.Equal(t, clk.t, snap.TakenAt)
	require.Equal(t, []model.GuestRef{refDB, refApp, refWeb, refBatch}, refs(snap))
	for _, ref := range []model.GuestRef{refWeb, refBatch, refDB, refApp} {
		require.Equal(t, 1, src.count("config "+ref.String()), ref.String())
	}

	web := guestOf(t, snap, refWeb)
	require.Equal(t, "web-1", web.Name)
	require.Equal(t, "pve1", web.Node)
	require.True(t, web.Running)
	require.Equal(t, []string{"cf-tunnel", "prod"}, web.Tags)
	require.Equal(t, "front end", web.Description)
	require.Equal(t, "uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", web.Identity)
	require.Len(t, web.NICs, 1)
	require.Equal(t, []model.ReportedAddr{{Iface: "eth0", MAC: "bc:24:11:00:aa:01", Addr: ip("10.20.0.15")}}, web.Reported)
}

func TestRefreshSecondFetchesOnlyWatchedConfigs(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Len(t, snap.Guests, 4)
	for _, ref := range []model.GuestRef{refWeb, refBatch, refApp} {
		require.Equal(t, 2, src.count("config "+ref.String()), "watched %s", ref)
	}
	require.Equal(t, 1, src.count("config "+refDB.String()), "unwatched guest comes from the cache")
	require.Equal(t, "db-1", guestOf(t, snap, refDB).Name)
	require.Equal(t, "bc:24:11:00:bb:01", guestOf(t, snap, refDB).NICs[0].MAC)
}

func TestRefreshFullSweepRefetchesUnwatchedConfigs(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	clk.advance(5*time.Minute - time.Second)
	inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("config "+refDB.String()), "just before the sweep")

	clk.advance(time.Second)
	inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("config "+refDB.String()), "at the sweep")

	clk.advance(time.Minute)
	inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("config "+refDB.String()), "the sweep restarts the interval")
}

func TestRefreshHonoursConfiguredSweepInterval(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{FullSweepEvery: time.Minute})
	inv.Refresh(t.Context())

	clk.advance(time.Minute)
	inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("config "+refDB.String()))
}

func TestRefreshUsesCachedConfigWithCurrentResourceRow(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	first := inv.Refresh(t.Context())
	src.update(refDB, func(row *pve.Resource) {
		row.Node = "pve1"
		row.Name = "db-2"
		row.Status = "stopped"
		row.Tags = []string{"prod", "db"}
	})
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.Equal(t, 1, src.count("config "+refDB.String()), "the unwatched guest is not read again")
	db := guestOf(t, snap, refDB)
	require.Equal(t, "pve1", db.Node)
	require.Equal(t, "db-2", db.Name)
	require.False(t, db.Running)
	require.Equal(t, []string{"prod", "db"}, db.Tags)
	require.Equal(t, guestOf(t, first, refDB).Identity, db.Identity)
	require.Equal(t, guestOf(t, first, refDB).NICs, db.NICs)
}

func TestRefreshAlwaysReadsWhatItIsTold(t *testing.T) {
	src := newFake()
	src.update(refDB, func(row *pve.Resource) { row.Pool = "pco" })
	src.update(refWeb, func(row *pve.Resource) { row.Pool = "prod" })
	asked := map[model.GuestRef]string{}
	inv, clk := newInventory(src, Options{AlwaysRead: func(ref model.GuestRef, pool string) bool {
		asked[ref] = pool
		return pool == "pco"
	}})

	first := inv.Refresh(t.Context())
	require.Equal(t, "pco", guestOf(t, first, refDB).Pool)
	require.Equal(t, "prod", guestOf(t, first, refWeb).Pool)
	require.Empty(t, guestOf(t, first, refApp).Pool)
	for range 3 {
		clk.advance(10 * time.Second)
		inv.Refresh(t.Context())
	}

	require.Equal(t, 4, src.count("config "+refDB.String()), "an untagged guest in the pool is read every refresh")
	require.Equal(t, "pco", asked[refDB], "asked with the pool of the resource row")

	src.resourcesErr = serverErr()
	clk.advance(10 * time.Second)
	previous := inv.Refresh(t.Context())
	require.False(t, previous.Complete)
	require.Equal(t, "pco", guestOf(t, previous, refDB).Pool, "the previous guests keep their pool")
}

func TestRefreshAlwaysReadsTheOwnGuest(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{AlwaysRead: func(ref model.GuestRef, _ string) bool { return ref == refDB }})
	inv.Refresh(t.Context())
	clk.advance(10 * time.Second)
	inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("config "+refDB.String()))

	src.setOnline("pve2", false)
	clk.advance(10 * time.Second)
	inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("config "+refDB.String()), "nothing is asked of an offline node")
}

func TestRefreshGuestBecomingWatchedIsFetchedAtOnce(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	src.update(refDB, func(row *pve.Resource) { row.Tags = []string{"cf-tunnel"} })
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("config "+refDB.String()))
	require.Equal(t, 1, src.count("iface "+refDB.String()))
	require.NotEmpty(t, guestOf(t, snap, refDB).Reported)
}

func TestRefreshGateTagsAreComparedExactly(t *testing.T) {
	src := newFake()
	src.addGuest(
		pve.Resource{Kind: model.KindQEMU, VMID: 103, Name: "shout-1", Node: "pve1", Status: "running", Tags: []string{"CF-Tunnel"}},
		map[string]string{"name": "shout-1", "net0": "virtio=BC:24:11:00:AA:03,bridge=vmbr0"},
		[]pve.GuestIface{iface("eth0", "bc:24:11:00:aa:03", "10.20.0.16")},
	)
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 103}
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.Equal(t, 1, src.count("config "+ref.String()))
	require.Zero(t, src.count("iface "+ref.String()))
	require.Empty(t, guestOf(t, snap, ref).Reported)
}

func TestNewDefaultsGateTags(t *testing.T) {
	for name, tags := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			src := newFake()
			clk := &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
			inv := New(src, Options{GateTags: tags}, clk.now, zerolog.Nop())
			inv.Refresh(t.Context())
			clk.advance(10 * time.Second)

			snap := inv.Refresh(t.Context())

			for _, ref := range []model.GuestRef{refWeb, refBatch, refApp} {
				require.Equal(t, 2, src.count("config "+ref.String()), "%s carries cf-tunnel", ref)
			}
			require.Equal(t, 1, src.count("config "+refDB.String()), "untagged guest comes from the cache")
			require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
		})
	}
}

func TestRefreshConfigFailureMarksIncomplete(t *testing.T) {
	t.Run("cached guest is served from the cache", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.configErrs[refWeb] = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "qemu/101")
		require.Len(t, snap.Guests, 4, "other guests are still returned")
		require.Equal(t, guestOf(t, first, refWeb), guestOf(t, snap, refWeb))
	})

	t.Run("guest without a cache is left out", func(t *testing.T) {
		src := newFake()
		src.configErrs[refDB] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "lxc/200")
		require.Equal(t, []model.GuestRef{refApp, refWeb, refBatch}, refs(snap))
	})

	t.Run("failed sweep keeps the cache and retries next time", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		inv.Refresh(t.Context())
		src.configErrs[refDB] = serverErr()
		clk.advance(5 * time.Minute)

		snap := inv.Refresh(t.Context())
		require.False(t, snap.Complete)
		require.Equal(t, "db-1", guestOf(t, snap, refDB).Name)

		delete(src.configErrs, refDB)
		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Equal(t, 3, src.count("config "+refDB.String()))
	})

	t.Run("other watched guests keep their reported addresses", func(t *testing.T) {
		src := newFake()
		src.configErrs[refWeb] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.NotEmpty(t, guestOf(t, snap, refApp).Reported)
	})
}

func TestRefreshConfigNotFoundMarksIncomplete(t *testing.T) {
	t.Run("cached guest that moved to another node", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.moved[refWeb] = "pve2" // the resource row still says pve1
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Equal(t, []string{"config for qemu/101 (web-1) not found on pve1; will re-check"}, snap.Problems)
		require.Len(t, snap.Guests, 4)
		require.Equal(t, guestOf(t, first, refWeb), guestOf(t, snap, refWeb))

		src.update(refWeb, func(row *pve.Resource) { row.Node = "pve2" })
		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
		require.Equal(t, "pve2", guestOf(t, snap, refWeb).Node)
	})

	t.Run("guest without a cache is left out", func(t *testing.T) {
		src := newFake()
		src.moved[refWeb] = "pve2"
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Equal(t, []string{"config for qemu/101 (web-1) not found on pve1; will re-check"}, snap.Problems)
		require.Equal(t, []model.GuestRef{refDB, refApp, refBatch}, refs(snap))
	})

	t.Run("a deletion is confirmed by the next listing", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		inv.Refresh(t.Context())
		src.configErrs[refWeb] = notFoundErr()
		clk.advance(10 * time.Second)
		snap := inv.Refresh(t.Context())
		require.False(t, snap.Complete)
		require.Len(t, snap.Guests, 4, "still served from the cache")

		src.removeGuest(refWeb)
		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())

		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
		require.Equal(t, []model.GuestRef{refDB, refApp, refBatch}, refs(snap))
	})

	t.Run("a failed sweep of an unwatched guest", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.configErrs[refDB] = notFoundErr()
		clk.advance(5 * time.Minute)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Contains(t, snap.Problems[0], "lxc/200")
		require.Equal(t, guestOf(t, first, refDB), guestOf(t, snap, refDB))
	})
}

func TestRefreshResourcesFailureKeepsPreviousGuests(t *testing.T) {
	t.Run("after a good refresh", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.resourcesErr = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Equal(t, first.Guests, snap.Guests)
		require.Equal(t, first.Nodes, snap.Nodes)
		require.Equal(t, clk.t, snap.TakenAt)
		require.Equal(t, 1, src.count("nodes"), "node calls are skipped")
		require.Equal(t, 1, src.count("config "+refWeb.String()), "so are config calls")

		src.resourcesErr = nil
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
	})

	t.Run("on the first refresh", func(t *testing.T) {
		src := newFake()
		src.resourcesErr = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Empty(t, snap.Guests)
		require.NotEmpty(t, snap.Problems)
	})
}

func TestRefreshEvictsGuestsThatDisappear(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	removed := slices.Clone(src.resources)
	src.removeGuest(refDB)
	src.removeGuest(refWeb)
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []model.GuestRef{refApp, refBatch}, refs(snap))

	src.resources = removed
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.Len(t, snap.Guests, 4)
	require.Equal(t, 2, src.count("config "+refDB.String()), "unwatched guest is read again, not served from the cache")
	require.Equal(t, 2, src.count("iface "+refWeb.String()), "reported cache is gone as well")
}

func TestSnapshotGuest(t *testing.T) {
	snap := Snapshot{Guests: []model.Guest{
		{Ref: refDB, Name: "db-1"},
		{Ref: refWeb, Name: "web-1"},
	}}

	g, ok := snap.Guest(refWeb)
	require.True(t, ok)
	require.Equal(t, "web-1", g.Name)

	_, ok = snap.Guest(model.GuestRef{Kind: model.KindLXC, VMID: 101})
	require.False(t, ok)
	_, ok = Snapshot{}.Guest(refWeb)
	require.False(t, ok)
}

func TestRefreshOrderIsDeterministic(t *testing.T) {
	build := func() *fakeSource {
		src := newFake()
		for vmid := 300; vmid < 320; vmid++ {
			kind, name := model.KindQEMU, fmt.Sprintf("vm-%d", vmid)
			values := map[string]string{"name": name, "net0": fmt.Sprintf("virtio=BC:24:11:00:CC:%02X,bridge=vmbr0", vmid-300)}
			if vmid%2 == 0 {
				kind = model.KindLXC
				values = map[string]string{"hostname": name, "net0": fmt.Sprintf("name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:DD:%02X", vmid-300)}
			}
			row := pve.Resource{Kind: kind, VMID: vmid, Name: name, Node: "pve1", Status: "running"}
			if vmid%3 == 0 {
				row.Tags = []string{"cf-tunnel"}
			}
			src.addGuest(row, values, []pve.GuestIface{iface("eth0", "", fmt.Sprintf("10.20.1.%d", vmid-300))})
		}
		src.nodes = append(src.nodes, pve.ClusterNode{Name: "pve0", Online: true}, pve.ClusterNode{Name: "pve9"})
		src.networks["pve0"] = []pve.NodeIface{{Name: "vmbr0", Type: "bridge"}}
		return src
	}
	want := func() Snapshot {
		inv, _ := newInventory(build(), Options{Concurrency: 4})
		return inv.Refresh(t.Context())
	}()
	require.True(t, want.Complete)
	require.Len(t, want.Guests, 24)
	require.True(t, slices.IsSortedFunc(want.Guests, func(a, b model.Guest) int {
		return strings.Compare(a.Ref.String(), b.Ref.String())
	}))

	rng := rand.New(rand.NewPCG(7, 11))
	for range 10 {
		src := build()
		rng.Shuffle(len(src.resources), func(i, j int) { src.resources[i], src.resources[j] = src.resources[j], src.resources[i] })
		rng.Shuffle(len(src.nodes), func(i, j int) { src.nodes[i], src.nodes[j] = src.nodes[j], src.nodes[i] })
		inv, _ := newInventory(src, Options{Concurrency: 4})

		got := inv.Refresh(t.Context())

		require.Equal(t, want.Guests, got.Guests)
		require.Equal(t, want.Nodes, got.Nodes)
		require.Equal(t, want.Problems, got.Problems)
	}
}

func TestRefreshBoundsConcurrency(t *testing.T) {
	const limit = 3
	src := newFake()
	for vmid := 300; vmid < 309; vmid++ {
		src.addGuest(
			pve.Resource{Kind: model.KindQEMU, VMID: vmid, Name: fmt.Sprintf("vm-%d", vmid), Node: "pve1", Status: "running"},
			map[string]string{"name": "vm", "net0": fmt.Sprintf("virtio=BC:24:11:00:CC:%02X,bridge=vmbr0", vmid-300)},
			nil,
		)
	}
	var (
		mu       sync.Mutex
		once     sync.Once
		inflight int
		peak     int
		release  = make(chan struct{})
	)
	src.configGate = func(ctx context.Context) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		if inflight == limit {
			once.Do(func() { close(release) })
		}
		mu.Unlock()
		// The first calls wait until "limit" of them are in flight at once,
		// which shows that the pool really runs that many.
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		inflight--
		mu.Unlock()
	}
	inv, _ := newInventory(src, Options{Concurrency: limit})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	snap := inv.Refresh(ctx)

	require.True(t, snap.Complete)
	require.Len(t, snap.Guests, 13)
	require.Equal(t, limit, peak)
}

func TestRefreshCancelledReturnsPreviousGuests(t *testing.T) {
	t.Run("during the refresh", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		src.configGate = func(context.Context) { cancel() }
		clk.advance(10 * time.Second)

		snap := inv.Refresh(ctx)

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "cancelled")
		require.Equal(t, first.Guests, snap.Guests)
		require.Equal(t, first.Nodes, snap.Nodes)
		require.Equal(t, 1, src.count("network pve1"), "no node is asked after the cancellation")

		src.configGate = nil
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
	})

	t.Run("before it starts", func(t *testing.T) {
		src := newFake()
		inv, _ := newInventory(src, Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		snap := inv.Refresh(ctx)

		require.False(t, snap.Complete)
		require.Contains(t, snap.Problems[0], "cancelled")
		require.Empty(t, snap.Guests)
		require.Zero(t, src.count("resources"))
	})

	t.Run("a deadline of the caller is not an agent timeout", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		src.ifaceHooks[refWeb] = func(ctx context.Context) ([]pve.GuestIface, error) {
			cancel()
			<-ctx.Done()
			return nil, fmt.Errorf("fetching: %w", context.DeadlineExceeded)
		}
		clk.advance(time.Minute)

		snap := inv.Refresh(ctx)

		require.False(t, snap.Complete)
		require.Contains(t, snap.Problems[0], "cancelled")
		require.Equal(t, first.Guests, snap.Guests)
	})
}

func TestRefreshReportsDuplicateResources(t *testing.T) {
	src := newFake()
	dup := src.resources[0]
	dup.Node = "pve2"
	src.resources = append(src.resources, dup)
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.False(t, snap.Complete)
	require.Len(t, snap.Problems, 1)
	require.Contains(t, snap.Problems[0], "qemu/101 is listed on both pve1 and pve2")
	require.Len(t, snap.Guests, 4)
	require.Equal(t, 1, src.count("config "+refWeb.String()))
}

func TestRefreshNeverLogsDescriptions(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(zerolog.SyncWriter(&buf)).Level(zerolog.DebugLevel)
	src := newFake()
	src.configs[refWeb].Values["description"] = "SECRET-NOTE"
	clk := &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	inv := New(src, Options{GateTags: []string{"cf-tunnel"}}, clk.now, log)

	inv.Refresh(t.Context())
	src.configErrs[refApp] = serverErr()
	src.configErrs[refBatch] = notFoundErr()
	src.ifaceErrs[refWeb] = errors.New("boom")
	clk.advance(time.Minute)
	inv.Refresh(t.Context())

	require.NotEmpty(t, buf.String())
	require.NotContains(t, buf.String(), "SECRET-NOTE")
}

// scribble overwrites everything a caller can reach in a snapshot.
func scribble(snap *Snapshot) {
	for i := range snap.Guests {
		g := &snap.Guests[i]
		g.Name = "scribbled"
		for k := range g.Tags {
			g.Tags[k] = "scribbled"
		}
		for k := range g.NICs {
			g.NICs[k].MAC = "scribbled"
			for m := range g.NICs[k].Static {
				g.NICs[k].Static[m] = ip("203.0.113.9")
			}
		}
		for k := range g.Reported {
			g.Reported[k] = model.ReportedAddr{Iface: "scribbled"}
		}
	}
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		n.Name = "scribbled"
		for k := range n.Ifaces {
			n.Ifaces[k].Name = "scribbled"
			for m := range n.Ifaces[k].Addrs {
				n.Ifaces[k].Addrs[m] = netip.MustParsePrefix("203.0.113.0/24")
			}
			for m := range n.Ifaces[k].Ports {
				n.Ifaces[k].Ports[m] = "scribbled"
			}
		}
	}
	for k := range snap.Problems {
		snap.Problems[k] = "scribbled"
	}
	snap.Guests = slices.DeleteFunc(snap.Guests, func(model.Guest) bool { return true })
	snap.Nodes = slices.DeleteFunc(snap.Nodes, func(Node) bool { return true })
}

func TestRefreshSnapshotsShareNothingWithTheInventory(t *testing.T) {
	pristine := func() Snapshot {
		inv, _ := newInventory(newFake(), Options{})
		return inv.Refresh(t.Context())
	}()
	require.True(t, pristine.Complete)
	require.NotEmpty(t, pristine.Guests[0].NICs[0].Static, "the fixture exercises static addresses")
	require.NotEmpty(t, pristine.Nodes[0].Ifaces[0].Ports)

	src := newFake()
	inv, clk := newInventory(src, Options{})
	first := inv.Refresh(t.Context())
	require.Equal(t, pristine.Guests, first.Guests)
	scribble(&first)

	clk.advance(10 * time.Second)
	second := inv.Refresh(t.Context())
	require.Equal(t, pristine.Guests, second.Guests, "reported answers come from the cache")
	require.Equal(t, pristine.Nodes, second.Nodes)
	scribble(&second)

	src.resourcesErr = serverErr()
	clk.advance(10 * time.Second)
	previous := inv.Refresh(t.Context())
	require.False(t, previous.Complete)
	require.Equal(t, pristine.Guests, previous.Guests)
	require.Equal(t, pristine.Nodes, previous.Nodes)
	scribble(&previous)

	again := inv.Refresh(t.Context())
	require.False(t, again.Complete)
	require.Equal(t, pristine.Guests, again.Guests, "a returned fallback copy does not feed the next one")
	require.Equal(t, pristine.Nodes, again.Nodes)
}

func TestRefreshGoodAt(t *testing.T) {
	src := newFake()
	src.configErrs[refWeb] = serverErr()
	inv, clk := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())
	require.False(t, snap.Complete)
	require.True(t, snap.GoodAt.IsZero(), "no refresh has been complete yet")
	require.Equal(t, clk.t, snap.TakenAt)

	delete(src.configErrs, refWeb)
	clk.advance(10 * time.Second)
	good := clk.t
	snap = inv.Refresh(t.Context())
	require.True(t, snap.Complete)
	require.Equal(t, good, snap.GoodAt)
	require.Equal(t, good, snap.TakenAt)

	src.configErrs[refWeb] = serverErr()
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.False(t, snap.Complete)
	require.Equal(t, good, snap.GoodAt)
	require.Equal(t, clk.t, snap.TakenAt)

	src.resourcesErr = serverErr()
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.False(t, snap.Complete)
	require.Equal(t, good, snap.GoodAt, "a fallback snapshot says when the last complete one was taken")
	require.Equal(t, clk.t, snap.TakenAt)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	clk.advance(10 * time.Second)
	snap = inv.Refresh(ctx)
	require.Equal(t, good, snap.GoodAt)

	src.resourcesErr = nil
	delete(src.configErrs, refWeb)
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.True(t, snap.Complete)
	require.Equal(t, clk.t, snap.GoodAt)
}

func TestRefreshConcurrentCalls(t *testing.T) {
	src := newFake()
	inv, _ := newInventory(src, Options{})
	snaps := make([]Snapshot, 8)

	var wg sync.WaitGroup
	for k := range snaps {
		wg.Go(func() {
			snaps[k] = inv.Refresh(t.Context())
			scribble(&snaps[k])
		})
	}
	wg.Wait()

	again := inv.Refresh(t.Context())
	require.True(t, again.Complete)
	require.Len(t, again.Guests, 4)
	require.Equal(t, "web-1", guestOf(t, again, refWeb).Name, "no snapshot handed out shares memory with the next one")
}
