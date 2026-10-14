//go:build scale

package engine

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Five nodes, two accounts, a thousand routes and three tunnels pinned to
// nodes in each account stay under 60 % of the budget of a credential once
// they settled, and nothing is written: the shared tunnels and the zones are
// read every cycle, as on a node alone, and what the pinned tunnels add, a
// read of each and a listing of each account once a minute and a read of
// their tokens, stays under 60 requests in five minutes.
func TestAFiveNodeClusterStaysWithinItsCloudflareBudget(t *testing.T) {
	const (
		routes = 1000
		window = 5 * time.Minute
		budget = cfapi.DefaultBudget * 60 / 100
		// pinnedBudget is six reads and two listings a minute, and the
		// tokens of the six read again when the accounts are listed.
		pinnedBudget = 60
	)
	nodes := []string{"pve1", "pve2", "pve3", "pve4", "pve5"}
	var others []store.NodeEntry
	for i, n := range nodes[1:] {
		idx := i + 1
		others = append(others, store.NodeEntry{Name: n, Since: t0, State: store.NodeActive, StateSince: t0, Index: &idx})
	}
	e, r, listed := clusterOf(t, others...)
	*listed = nil
	inv := inventory.Snapshot{Complete: true}
	for i, n := range nodes {
		*listed = append(*listed, members.Member{Name: n, ID: i + 1, Online: true})
		inv.Nodes = append(inv.Nodes, inventory.Node{Name: n, Addr: netip.AddrFrom4([4]byte{10, 0, 1, byte(i + 1)}), Online: true, Local: i == 0})
	}
	e.cf.AddAccount("acc2", "Second")
	e.cf.AddZone("zone2", "example.org", "acc2")
	acts := map[string]*agentAct{}
	for _, n := range nodes[1:] {
		acts[n] = &agentAct{kind: pki.KindHost, started: t0.Add(-time.Hour), fail: map[string]string{}}
	}
	for i := range routes {
		vmid := 1000 + i
		zone := "example.com"
		if i%2 == 1 {
			zone = "example.org"
		}
		host := fmt.Sprintf("g%d.%s", vmid, zone)
		node := nodes[i%len(nodes)]
		g := carded(guest(vmid, fmt.Sprintf("vm-%d", vmid), host+" -> :8080"), node, fmt.Sprintf("bc:24:11:%02x:%02x:01", vmid/256, vmid%256))
		inv.Guests = append(inv.Guests, g)
		// Three guests of each account, on pve2 to pve4, are reached on
		// their own node alone: their routes are pinned there.
		if i >= 10 && i < 22 && node != "pve1" && node != "pve5" {
			for _, other := range nodes[1:] {
				if other != node {
					acts[other].fail[host] = "not reached here"
				}
			}
			e.res.unreachable[host] = "not reached here"
		}
	}
	e.inv.set(inv)
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	e.enforce()
	w := &world{env: e, r: r, lease: l, listed: listed, acts: acts}
	w.watchHere(t)

	for range 6 * 15 {
		w.tick(10 * time.Second)
	}
	st := w.tick(10 * time.Second)
	pinned := 0
	for _, tp := range st.Tunnels {
		if _, ok := planner.PinnedTunnelNode(testInstall, tp.Name); ok {
			pinned++
		}
	}
	require.Equal(t, 6, pinned, "three pinned tunnels in each account")
	// Cloudflare lists the connectors the agents run, each on the
	// configuration last written.
	pinnedIDs := map[string]bool{}
	for _, account := range []string{testAccount, "acc2"} {
		for _, tun := range w.cf.TunnelsIn(account) {
			conns := []cfapi.Connector{{ID: defaultConnectorID, ConfigVersion: 1 << 20}}
			if node, ok := planner.PinnedTunnelNode(testInstall, tun.Name); ok {
				pinnedIDs[tun.ID] = true
				conns = []cfapi.Connector{{ID: node + "-" + tun.ID, ConfigVersion: 1 << 20}}
			} else {
				for _, n := range nodes[1:] {
					conns = append(conns, cfapi.Connector{ID: n + "-" + tun.ID, ConfigVersion: 1 << 20})
				}
			}
			w.cf.SetConnectors(account, tun.ID, conns)
		}
	}
	for range 6 * 5 {
		w.tick(10 * time.Second)
	}

	worst, worstPinned := 0, 0
	for range 3 {
		before := len(w.cf.Calls())
		for range int(window / (10 * time.Second)) {
			w.tick(10 * time.Second)
		}
		calls := w.cf.Calls()[before:]
		ofPinned := 0
		for _, c := range calls {
			require.False(t, strings.HasPrefix(c, "Put") || strings.HasPrefix(c, "Create") || strings.HasPrefix(c, "Delete") ||
				strings.HasPrefix(c, "Update"), "nothing is written once it settled: %s", c)
			f := strings.Fields(c)
			if f[0] == "Tunnels" || len(f) > 2 && pinnedIDs[f[2]] {
				ofPinned++
			}
		}
		worst, worstPinned = max(worst, len(calls)), max(worstPinned, ofPinned)
	}
	t.Logf("at most %d requests in five minutes, %d of them for the pinned tunnels", worst, worstPinned)
	require.LessOrEqual(t, worst, budget)
	require.LessOrEqual(t, worstPinned, pinnedBudget)
}
